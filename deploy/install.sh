#!/usr/bin/env bash
# Install perchd as a systemd user service.
#
#   deploy/install.sh            build, install and (re)start the service
#   deploy/install.sh uninstall  stop and remove it (keeps ~/.config/perchd/env)
#
# Re-run after pulling changes to rebuild and restart.
set -euo pipefail
cd "$(dirname "$0")/.."

bin="$HOME/.local/bin/perchd"
unit="$HOME/.config/systemd/user/perchd.service"
envfile="$HOME/.config/perchd/env"

case "${1:-install}" in
install)
	node=$(command -v node) || { echo "node not found on PATH (pi needs it)" >&2; exit 1; }
	pi=$(command -v pi) || { echo "pi not found on PATH" >&2; exit 1; }

	echo "building $bin"
	mkdir -p "$(dirname "$bin")"
	go build -o "$bin" ./cmd/perchd

	# Claude Agent SDK bridge, for -engine claude
	bridge="$HOME/.local/share/perchd/claude-bridge"
	if command -v npm >/dev/null; then
		echo "installing claude bridge in $bridge"
		mkdir -p "$bridge"
		install -m644 bridge/claude/bridge.mjs bridge/claude/package.json bridge/claude/package-lock.json "$bridge/"
		npm ci --prefix "$bridge" --omit=dev --no-audit --no-fund --loglevel=error
	else
		echo "npm not found; skipping the claude bridge" >&2
	fi

	install -Dm644 deploy/perchd.service "$unit"

	if [[ ! -f $envfile ]]; then
		mkdir -p "$(dirname "$envfile")"
		path="$(dirname "$node")"
		[[ $(dirname "$pi") != "$path" ]] && path+=":$(dirname "$pi")"
		path+=":/usr/local/bin:/usr/bin:/bin"
		cat >"$envfile" <<EOF
# Environment for the perchd user service (systemd EnvironmentFile syntax, no ~ expansion).
PATH=$path
# Extra perchd flags, e.g. -root $HOME/Projects -idle 1h -pi-args "--approve"
PERCHD_ARGS=
EOF
		echo "wrote $envfile"
	else
		echo "keeping existing $envfile"
	fi

	systemctl --user daemon-reload
	systemctl --user enable perchd.service
	systemctl --user restart perchd.service
	sleep 1
	systemctl --user --no-pager status perchd.service | head -n 12 || true

	if [[ $(loginctl show-user "$USER" -p Linger --value 2>/dev/null) != yes ]]; then
		cat <<EOF

Note: user services start at login. To start perchd at boot, before you log in, run:
    loginctl enable-linger $USER
EOF
	fi
	;;
uninstall)
	systemctl --user disable --now perchd.service 2>/dev/null || true
	rm -f "$unit" "$bin"
	rm -rf "$HOME/.local/share/perchd/claude-bridge"
	systemctl --user daemon-reload
	echo "removed perchd service and binary (left $envfile)"
	;;
*)
	echo "usage: $0 [install|uninstall]" >&2
	exit 2
	;;
esac
