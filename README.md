# pircd

Remote control for the [pi](https://github.com/earendil-works/pi) coding agent:
a small Go server that serves server-rendered pages and streams live updates
over SSE. Meant to be reached over Tailscale.

- **/**: recent sessions (tap one to attach, i.e. `pi --resume`)
- **/new**: pick a directory with Miller columns, optionally name the session
- **/s/{id}**: transcript, prompt box (steer/queue while running, Stop), copy last response.
  In the prompt box, `/` suggests pi's extension commands, prompt templates and skills;
  `!cmd` runs a shell command whose output goes to the model with the next prompt, and
  `!!cmd` runs one without sharing its output. Attach files with the "+" button, drag and
  drop, or by pasting a screenshot; they're saved on the server under `~/.cache/pircd/uploads`
  (`-uploads`) and pi gets their paths. The "⋯" menu has stats, rename, compact,
  duplicate and release.

## Run

```sh
go build -o pircd .
./pircd                      # binds tailscale0 IPv4 + 127.0.0.1 on :7337
./pircd -addr 100.x.y.z:8080 # explicit address(es), comma separated
```

| Flag | Default | |
|---|---|---|
| `-addr` | tailscale0 + loopback, port 7337 | listen address(es) |
| `-pi` | `pi` | pi executable (must find `node` on PATH) |
| `-pi-args` | | extra args for every pi process, e.g. `--approve` |
| `-sessions` | `~/.pi/agent/sessions` | session directory (passed to pi as `--session-dir` if changed) |
| `-root` | `$HOME` | where the directory picker starts |
| `-idle` | `30m` | stop pi processes with no viewers after this long |
| `-uploads` | `~/.cache/pircd/uploads` | where files attached to prompts are saved, one folder per session |
| `-upload-retention` | `168h` (7 days) | delete a session's uploads once the session and its uploads are unused this long; `0` keeps them |
| `-hide-status` | `pi-vimmode` | extension status pills to hide (comma separated; hover a pill to see its key) |

There is no authentication: anyone who can reach the port can run commands as
you. Keep it on the tailnet. For HTTPS (which gives you the async clipboard API), use
`tailscale serve --bg 7337`.

### Done notifications (ntfy)

Subscribe to a topic in the ntfy phone app, then configure pircd with its full
URL. Notifications are sent by the server when an active run settles, even if
no browser is open or your phone is locked:

```sh
export PIRCD_NTFY_TOPIC=https://ntfy.sh/your-long-random-private-topic
export PIRCD_PUBLIC_URL=https://your-machine.your-tailnet.ts.net
# Optional, for an access-controlled ntfy server:
export PIRCD_NTFY_TOKEN=your-token
./pircd
```

Equivalent flags: `-ntfy-topic`, `-public-url`, `-ntfy-token`. Prefer the environment
for tokens so they do not appear in process arguments. For systemd, put these
variables in `~/.config/pircd/env` and restart the service.

Notifications include the session name (or project directory basename), never
prompts or responses. The optional public URL makes tapping the notification
open that session and supplies the app's PNG notification icon; your phone must
be able to reach it (including for loading the icon). Anyone subscribed to a
public topic can read its notifications: use an unguessable topic or a private,
access-controlled ntfy server. Delivery failures are logged without retrying;
notifications are best-effort, not a persistent queue. A stopped or failed run
that settles also produces a done notification. Leave the topic unset to disable.

### Start with the system (systemd user service)

```sh
deploy/install.sh             # build to ~/.local/bin, install + enable + start the service
loginctl enable-linger $USER  # optional: start at boot instead of at login
deploy/install.sh uninstall   # remove it
```

Re-run `deploy/install.sh` after changing the code to rebuild and restart.
Settings live in `~/.config/pircd/env`: `PATH` must include `node`, and
`PIRCD_ARGS` holds extra flags such as `-root /home/you/Projects`. The service runs
with `-wait-tailscale 2m`. At boot it waits for tailscaled, and if Tailscale never
comes up it exits so systemd retries, rather than serving on loopback only.

Logs: `journalctl --user -u pircd -f`

See [ARCHITECTURE.md](ARCHITECTURE.md) for how it works and where to extend it.
