# pircd

Remote control for the [pi](https://github.com/earendil-works/pi) coding agent:
a small Go server that serves server-rendered pages and streams live updates
over SSE. Meant to be reached over Tailscale.

> **Security: pircd is remote shell access as your user. There is no built-in
> authentication.** Anyone who can reach the port can read sessions and execute
> commands with your user's permissions. Use only from trusted devices over a
> restricted tailnet. Never expose it directly to the internet.

- **/**: recent sessions (tap one to attach, i.e. `pi --resume`)
- **/new**: choose a recent project or browse folders, optionally name the session
- **/s/{id}**: transcript, prompt box (steer/queue while running, Stop), copy last response.
  In the prompt box, `/` suggests pi's extension commands, prompt templates and skills;
  `!cmd` runs a shell command whose output goes to the model with the next prompt, and
  `!!cmd` runs one without sharing its output. Attach files with the "+" button, drag and
  drop, or by pasting a screenshot; they're saved on the server under `~/.cache/pircd/uploads`
  (`-uploads`) and pi gets their paths. The "⋯" menu has stats, rename, compact,
  duplicate and release. Tool progress streams in 100-line chunks, with any remaining
  lines flushed when the tool finishes. Live output opens automatically and follows
  new lines unless you scroll up; collapse it with its summary. Saved shell logs
  are restored in completed results and available via "Download full output".

## Prompt-box colors

The prompt box's border shows pi's state instead of an idle/responding label:

| Color | Meaning |
|---|---|
| Gray | Idle, ready for a prompt. |
| Blue | Responding (generating an assistant reply). |
| Amber | Busy: working, running a tool or shell command, compacting context, or retrying. |
| Red | The pi process exited; use **restart** above the prompt box to reconnect. |

Colors adapt to light/dark mode, and the focus glow uses the same color.
Hover over the prompt box for the detailed activity (including the tool or command);
that detail is also available to screen readers. Exit errors and the restart link
remain visible. A blue dashed border while dragging files is a temporary drop target,
not an activity state.

## Status and requirements

**Experimental:** intended for personal use on trusted devices, not a hardened
multi-user service. RPC compatibility may change with pi releases.

- **Go 1.27.1 or newer** to build (see `go.mod`).
- **pi 1.0.0** is the currently tested version. Older versions are not supported;
  newer versions have not been verified. pircd requires pi's RPC mode, including
  the `agent_settled` event and queue/extension UI commands.
- **Node.js 24 LTS** is the tested runtime for pi and the browser tests. Both
  `pi` and `node` must be on the server/service's `PATH`.
- **Linux** is the tested server platform. The service installer requires
  systemd user services; terminal-session detection uses Linux `/proc`.
  macOS and Windows are not currently supported or tested.
- **Tailscale** is needed for the default network binding and recommended remote
  access. Explicit loopback binding works without it.
- Configure working **pi provider authentication** as the same OS user that will
  run pircd. Start `pi` in a terminal, log in/configure a provider, and verify a
  prompt succeeds before using pircd. Provider API keys supplied through the
  environment must also be available to the service; an interactive shell's
  environment is not automatically inherited by systemd.

Check prerequisites with `go version`, `node --version`, and `pi --version`.
Install pi using its [upstream instructions](https://github.com/earendil-works/pi).

## Quickstart

After installing the prerequisites and configuring pi authentication:

```sh
git clone https://github.com/S-Spektrum-M/pircd.git
cd pircd
go build -o pircd .
./pircd -addr 127.0.0.1:7337
```

Open <http://localhost:7337> on the server. This explicit loopback address does
not require Tailscale and does not allow remote connections. For remote access,
follow the network guidance below; do not bind to a public interface.
To run persistently on Linux, use the systemd installer below instead.

## Known limitations

- No built-in authentication or per-user isolation; network access grants shell access.
- Concurrent terminal/web use of a session is unsafe. Linux process detection is
  best-effort, not a lock; interactive selection, `--resume`, and races can evade it.
- Terminal-only pi UI and arbitrary extension TUI components are not reproduced
  in the browser. Supported extension dialogs/status widgets use RPC UI events.
- Live tool output requires the tool to report partial results (treated as replaceable
  snapshots, as with pi's built-in shell tools). Buffered tails under 100 complete
  lines wait until completion; direct `!cmd` / `!!cmd` output is still completion-only.
  Saved shell logs have a 4 MiB inline preview limit and an unabridged download.
  If pi's temporary log has been deleted, only its persisted tool result is available.
- Task time and token totals describe the current running process's completed
  task and are not persisted as historical per-task metrics.
- Notifications are best-effort; uploads are stored on the server and expire
  according to the retention setting, not immediately after a prompt.

## Run

```sh
go build -o pircd .
./pircd                      # binds tailscale0 IPv4 + 127.0.0.1 on :7337
./pircd -addr 100.x.y.z:8080 # explicit address(es), comma separated
```

| Flag | Default | |
|---|---|---|
| `-addr` | tailscale0 + loopback, port 7337 | listen address(es) |
| `-allowed-hosts` | `PIRCD_ALLOWED_HOSTS` | additional exact HTTP Host names, comma separated; required for DNS names other than `localhost` |
| `-pi` | `pi` | pi executable (must find `node` on PATH) |
| `-pi-args` | | extra args for every pi process, e.g. `--approve` |
| `-sessions` | `~/.pi/agent/sessions` | session directory (passed to pi as `--session-dir` if changed) |
| `-root` | `$HOME` | where the directory picker starts |
| `-idle` | `30m` | stop pi processes with no viewers after this long |
| `-uploads` | `~/.cache/pircd/uploads` | where files attached to prompts are saved, one folder per session |
| `-upload-retention` | `168h` (7 days) | delete a session's uploads once the session and its uploads are unused this long; `0` keeps them |
| `-hide-status` | `pi-vimmode` | extension status pills to hide (comma separated; hover a pill to see its key) |

### Request limits and browser protections

Ordinary form bodies are limited to 1 MiB. Multipart prompt requests are limited
to 200 MiB including multipart overhead, with at most 32 attachments. Failed
multi-file saves remove files already saved by that request. Temporary multipart
files are removed after request handling. Upload bodies have a five-minute read
deadline; other form bodies have 30 seconds.

Headers must arrive within 10 seconds; idle keep-alive connections close after
two minutes. SSE streams have no fixed lifetime, but individual writes are bounded
to 15 seconds so clients must continue reading. Browsers reconnect interrupted
streams automatically.

Non-static responses use `Cache-Control: no-store`; static assets require
revalidation. This is not a promise to erase content already displayed or kept
in browser memory. Framing is blocked, cross-origin referrers are suppressed, and MIME sniffing
is disabled. A same-origin Content Security Policy blocks inline scripts and
remote resources; inline styling remains allowed for dynamic UI sizing.

### Security and network access

- Restrict Tailscale ACLs/grants so only your trusted users/devices can reach
  pircd. Tailnet membership alone is not authorization to control your shell.
- Do **not** use Tailscale Funnel, public port forwarding, or a public reverse
  proxy without a separate authentication and authorization layer.
- `-root` only controls where the directory picker starts. It is **not** a
  filesystem sandbox. Pi and shell commands run with your user's permissions.
- Prefer a dedicated, unprivileged OS user when you need isolation; avoid giving
  that user production credentials or access to unrelated private files.

For HTTPS (including the async clipboard API), use Tailscale Serve. Add its exact
DNS name to the allowed hosts and restart pircd:

```sh
export PIRCD_ALLOWED_HOSTS=your-machine.your-tailnet.ts.net
./pircd
tailscale serve --bg 7337
```

Alternatively pass `-allowed-hosts your-machine.your-tailnet.ts.net`. For the
systemd service, put `PIRCD_ALLOWED_HOSTS` in `~/.config/pircd/env` and restart it.
Local interface IPs, loopback IPs, and `localhost` are allowed automatically;
other DNS names require explicit configuration. Do not add wildcard domains or
untrusted names. A reverse proxy must preserve the original Host header.
Forwarded host headers are not trusted.

Browser-request protections reject cross-origin writes and unknown Host names
(to reduce CSRF and DNS-rebinding risk). Native forms use the browser's Origin
and Fetch Metadata headers. Non-browser clients making writes without those
headers must send `X-Pircd: 1`; this header does not bypass origin checks.
These checks are **not authentication** and do not stop a client that can reach
the server from deliberately issuing commands. Network access control is still
required.

### Model and effort defaults

Changing the model or effort normally affects only the current session.
Checking **Make default** also writes the selection to this OS user's global
pi settings (`~/.pi/agent/settings.json`, or the configured pi agent directory).
This affects pi started outside pircd, including terminal sessions, and is shared
by processes running as the same OS user. Project-specific settings may override
these defaults. Unchecking the box does not undo previously saved defaults.

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

## License

pircd is licensed under the [Apache License 2.0](LICENSE).
Vendored third-party code retains its own license: see
[the browser dependency notices](web/static/vendor/README.md) and
[markdown-it's MIT license](web/static/vendor/markdown-it.LICENSE).
