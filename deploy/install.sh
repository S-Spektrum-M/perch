#!/usr/bin/env bash
# Install pircd as a systemd user service.
#
#   deploy/install.sh            build, install and (re)start the service
#   deploy/install.sh uninstall  stop and remove it (keeps ~/.config/pircd/env)
#
# Re-run after pulling changes to rebuild and restart.
set -euo pipefail
cd "$(dirname "$0")/.."

bin="$HOME/.local/bin/pircd"
unit="$HOME/.config/systemd/user/pircd.service"
envfile="$HOME/.config/pircd/env"

case "${1:-install}" in
install)
	node=$(command -v node) || { echo "node not found on PATH (pi needs it)" >&2; exit 1; }
	pi=$(command -v pi) || { echo "pi not found on PATH" >&2; exit 1; }

	echo "building $bin"
	mkdir -p "$(dirname "$bin")"
	go build -o "$bin" .

	install -Dm644 deploy/pircd.service "$unit"

	if [[ ! -f $envfile ]]; then
		mkdir -p "$(dirname "$envfile")"
		path="$(dirname "$node")"
		[[ $(dirname "$pi") != "$path" ]] && path+=":$(dirname "$pi")"
		path+=":/usr/local/bin:/usr/bin:/bin"
		cat >"$envfile" <<EOF
# Environment for the pircd user service (systemd EnvironmentFile syntax, no ~ expansion).
PATH=$path
# Extra pircd flags, e.g. -root $HOME/Projects -idle 1h -pi-args "--approve"
PIRCD_ARGS=
EOF
		echo "wrote $envfile"
	else
		echo "keeping existing $envfile"
	fi

	systemctl --user daemon-reload
	systemctl --user enable pircd.service
	systemctl --user restart pircd.service
	sleep 1
	systemctl --user --no-pager status pircd.service | head -n 12 || true

	if [[ $(loginctl show-user "$USER" -p Linger --value 2>/dev/null) != yes ]]; then
		cat <<EOF

Note: user services start at login. To start pircd at boot, before you log in, run:
    loginctl enable-linger $USER
EOF
	fi
	;;
uninstall)
	systemctl --user disable --now pircd.service 2>/dev/null || true
	rm -f "$unit" "$bin"
	systemctl --user daemon-reload
	echo "removed pircd service and binary (left $envfile)"
	;;
*)
	echo "usage: $0 [install|uninstall]" >&2
	exit 2
	;;
esac
