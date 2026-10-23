# perch

Remote control for the [pi](https://github.com/earendil-works/pi) coding agent:
a small Go server that serves server-rendered pages and streams live updates
over SSE. Meant to be reached over Tailscale.

> **Security: perchd is remote shell access as your user. There is no built-in
> authentication.** Anyone who can reach the port can read sessions and execute
> commands with your user's permissions. Use only from trusted devices over a
> restricted tailnet. Never expose it directly to the internet.

- **/**: recent sessions (tap one to attach, i.e. `pi --resume`)
- **/new**: choose a recent project or browse folders, optionally name the session
- **/s/{id}**: transcript, prompt box (steer/queue while running, Stop), copy last response.
  In the prompt box, `/` suggests pi's extension commands, prompt templates and skills;
  `!cmd` runs a shell command whose output goes to the model with the next prompt, and
  `!!cmd` runs one without sharing its output. Attach files with the "+" button, drag and
  drop, or by pasting a screenshot; they're saved on the server under `~/.cache/perchd/uploads`
  (`-uploads`) and pi gets their paths. The "⋯" menu has stats, rename, compact,
  duplicate and release. Tool progress streams in 100-line chunks, with any remaining
  lines flushed when the tool finishes. Tool outputs are folded by default; click
  a summary to expand them. Expanded live output follows new lines unless you scroll up. Saved shell logs
  are restored in completed results and available via "Download full output".

## Experimental automatic session names

If a session has no name, perchd uses `gpt-6-luna` to summarize its first
successful text prompt into a short title. Naming runs in the background through
an ephemeral, tool-free pi process, using pi's configured model catalog and
credentials. It does not change the session's working model or transcript.
Only the prompt text is sent for naming, not attached files or shell commands.
This is an additional model request and may incur usage costs.

An existing name is never replaced, and a manual rename made while generation
is running takes precedence. If the model is unavailable or naming fails, the
session continues normally without a name; the next text prompt retries naming.
You can still rename the session from the “⋯” menu.

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
  newer versions have not been verified. perchd requires pi's RPC mode, including
  the `agent_settled` event and queue/extension UI commands.
- **Node.js 24 LTS** is the tested runtime for pi and the browser tests. Both
  `pi` and `node` must be on the server/service's `PATH`.
- **Linux** is the tested server platform. The service installer requires
  systemd user services; terminal-session detection uses Linux `/proc`.
  macOS and Windows are not currently supported or tested.
- **Tailscale** is needed for the default network binding and recommended remote
  access. Explicit loopback binding works without it.
- Configure working **pi provider authentication** as the same OS user that will
  run perchd. Start `pi` in a terminal, log in/configure a provider, and verify a
  prompt succeeds before using perchd. Provider API keys supplied through the
  environment must also be available to the service; an interactive shell's
  environment is not automatically inherited by systemd.

Check prerequisites with `go version`, `node --version`, and `pi --version`.
Install pi using its [upstream instructions](https://github.com/earendil-works/pi).

## Quickstart

After installing the prerequisites and configuring pi authentication:

```sh
git clone https://github.com/S-Spektrum-M/perch.git
cd perch
go build -o perchd ./cmd/perchd
./perchd -addr 127.0.0.1:7337
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
go build -o perchd ./cmd/perchd
./perchd                      # binds tailscale0 IPv4 + 127.0.0.1 on :7337
./perchd -addr 100.x.y.z:8080 # explicit address(es), comma separated
```

| Flag | Default | |
|---|---|---|
| `-addr` | tailscale0 + loopback, port 7337 | listen address(es) |
| `-allowed-hosts` | `PERCHD_ALLOWED_HOSTS` | additional exact HTTP Host names, comma separated; required for DNS names other than `localhost` |
| `-engine` | `PERCHD_ENGINE`, otherwise `pi` | agent backends, comma separated, e.g. `pi,claude`: `pi`, `claude` (Claude Agent SDK, see below), or `eta` (a stub; every session operation fails). The first is the default for new sessions |
| `-claude-auth` | `subscription` | `subscription` uses the Claude account login from `claude /login` and ignores `ANTHROPIC_API_KEY`; `env` uses API credentials from the environment |
| `-claude-permission-mode` | `bypassPermissions` | `bypassPermissions` never asks; `default` asks in the browser before edits and commands; also `acceptEdits`, `plan`, `auto` |
| `-claude-bridge` | `PERCHD_CLAUDE_BRIDGE`, otherwise `~/.local/share/perchd/claude-bridge/bridge.mjs` | the Node bridge, installed by `deploy/install.sh` |
| `-node` | `node` | node executable for the Claude bridge |
| `-pi` | `pi` | pi executable (must find `node` on PATH) |
| `-pi-args` | | extra args for every pi process, e.g. `--approve` |
| `-models-config` | `PERCHD_MODELS_CONFIG`, otherwise `~/.config/perchd/models.json` (`models-<engine>.json` for engines other than pi) | curated model picker file; respects `XDG_CONFIG_HOME`; missing file uses the engine's catalog |
| `-sessions` | `~/.pi/agent/sessions` | session directory (passed to pi as `--session-dir` if changed) |
| `-root` | `$HOME` | where the directory picker starts |
| `-idle` | `30m` | stop pi processes with no viewers after this long |
| `-uploads` | `~/.cache/perchd/uploads` | where files attached to prompts are saved, one folder per session |
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
  perchd. Tailnet membership alone is not authorization to control your shell.
- Do **not** use Tailscale Funnel, public port forwarding, or a public reverse
  proxy without a separate authentication and authorization layer.
- `-root` only controls where the directory picker starts. It is **not** a
  filesystem sandbox. Pi and shell commands run with your user's permissions.
- Prefer a dedicated, unprivileged OS user when you need isolation; avoid giving
  that user production credentials or access to unrelated private files.

For HTTPS (including the async clipboard API), use Tailscale Serve. Add its exact
DNS name to the allowed hosts and restart perchd:

```sh
export PERCHD_ALLOWED_HOSTS=your-machine.your-tailnet.ts.net
./perchd
tailscale serve --bg 7337
```

Alternatively pass `-allowed-hosts your-machine.your-tailnet.ts.net`. For the
systemd service, put `PERCHD_ALLOWED_HOSTS` in `~/.config/perchd/env` and restart it.
Local interface IPs, loopback IPs, and `localhost` are allowed automatically;
other DNS names require explicit configuration. Do not add wildcard domains or
untrusted names. A reverse proxy must preserve the original Host header.
Forwarded host headers are not trusted.

Browser-request protections reject cross-origin writes and unknown Host names
(to reduce CSRF and DNS-rebinding risk). Native forms use the browser's Origin
and Fetch Metadata headers. Non-browser clients making writes without those
headers must send `X-Perchd: 1`; this header does not bypass origin checks.
These checks are **not authentication** and do not stop a client that can reach
the server from deliberately issuing commands. Network access control is still
required.

### Claude backend (Claude Agent SDK)

`-engine pi,claude` runs pi and the [Claude Agent SDK](https://code.claude.com/docs/en/agent-sdk)
side by side (`-engine claude` alone works too). Claude sessions are Claude
Code's own (`~/.claude/projects`), so the home page lists your Claude Code
sessions next to pi's, and any of them can be continued in a terminal with
`claude --resume <id>` (release it in perchd first).

- **Choosing the engine:** **New**, **+** and the directory picker open a draft
  of the session; nothing starts until you send the first message. The draft's
  prompt box has a pi/claude switch and a model list for the picked agent
  (both preset to what you used last, models filtered by `models.json` /
  `models-claude.json`), and that message starts the agent on that model. With more than one engine, every session
  is tagged with its own, and each
  session keeps its own engine: open, duplicate and the model picker all use it.

- **Sign in with your Claude account:** run `claude /login` once as the user perchd
  runs as. With the default `-claude-auth subscription` the bridge hides
  `ANTHROPIC_API_KEY` so the SDK uses that login; the account shows under
  the session's **⋯** stats. Use `-claude-auth env` to bill an API key instead.
- **Permissions:** Claude runs with full permissions by default. With
  `-claude-permission-mode default` (or `acceptEdits`, `auto`) its permission
  prompts appear as dialogs (Allow / Allow for this session / Deny). Claude's
  multiple-choice questions are always dialogs.
- Each open session is one `node bridge.mjs` process (bridge/claude), which
  starts Claude Code. `deploy/install.sh` installs it with `npm ci`.
- Not supported yet: `!` shell commands, live tool output (results appear when
  a tool finishes), subagent progress, images in prompts. Steer and Queue both
  just send the message; Claude Code decides when it is read.
- The model picker uses `~/.config/perchd/models-claude.json` if present
  (provider `anthropic`, IDs like `opus` or `claude-opus-5-5`); **Make default**
  writes `~/.config/perchd/claude.json`, not Claude Code's settings.

### Model picker configuration

By default, the inline model/effort picker shows pi's available models. To curate
its list, create `~/.config/perchd/models.json` (or
`$XDG_CONFIG_HOME/perchd/models.json` when set):

```json
{
  "models": [
    {
      "provider": "your-provider",
      "id": "your-model-id",
      "label": "Everyday"
    },
    {
      "provider": "your-provider",
      "id": "another-model-id",
      "label": "Deep reasoning"
    }
  ]
}
```

[models.example.json](models.example.json) is a copyable template. **Replace its
placeholder provider and model IDs** with exact values from pi (the picker shows
`provider / id` beneath each model). This file only controls presentation; it
does not define providers, endpoints, credentials, or reasoning-effort levels.
Those remain pi's responsibility.

- The picker shows **only the configured entries, in file order**. Filtering
  temporarily ranks matching entries; clearing the filter restores file order.
- `label` is optional. When supplied, it appears in the picker and prompt pill;
  otherwise pi's model name is used, or the ID if unavailable.
- Configured models absent from pi's available catalog remain visible, marked
  **Unavailable in pi**, and cannot be selected. Matching uses both provider and ID.
- A current session model absent from the file keeps its actual name in the pill
  without being added to the list. Its effort and defaults can still be changed.
- Changes are read on each picker open/save and session page load—no restart
  needed. Other sessions refresh their labels when their picker or page is reopened.
- A missing file falls back to pi's full catalog. `{"models": []}` deliberately
  shows an empty list. Invalid/unreadable files show an error in the picker,
  rather than silently falling back; fix the file and click **Retry** or reopen it.
- Provider and ID are required; duplicate provider/ID pairs and unknown fields
  are rejected. The file is strict JSON, without comments or trailing commas.

Use `-models-config /path/to/models.json` or `PERCHD_MODELS_CONFIG` to override the
path (the flag takes precedence). Configuration is read on the server as the OS
user running perchd, not on the browser device. Removing the file restores the
full catalog; it never changes the session's selected model automatically.

### Model and effort defaults

Changing the model or effort normally affects only the current session.
Checking **Make default** also writes the selection to this OS user's global
pi settings (`~/.pi/agent/settings.json`, or the configured pi agent directory).
This affects pi started outside perchd, including terminal sessions, and is shared
by processes running as the same OS user. Project-specific settings may override
these defaults. Unchecking the box does not undo previously saved defaults.

### Done and question notifications (ntfy)

Subscribe to a topic in the ntfy phone app, then configure perchd with its full
URL. Notifications are sent by the server when an active run settles or a
question dialog opens (including select, confirm, input, and editor dialogs),
even if no browser is open or your phone is locked:

```sh
export PERCHD_NTFY_TOPIC=https://ntfy.sh/your-long-random-private-topic
export PERCHD_PUBLIC_URL=https://your-machine.your-tailnet.ts.net
# Optional, for an access-controlled ntfy server:
export PERCHD_NTFY_TOKEN=your-token
./perchd
```

Equivalent flags: `-ntfy-topic`, `-public-url`, `-ntfy-token`. Prefer the environment
for tokens so they do not appear in process arguments. For systemd, put these
variables in `~/.config/perchd/env` and restart the service.

Notifications include the session name (or project directory basename), never
prompts, responses, question text, or choices. Question notifications say the
agent needs your input. The optional public URL makes tapping the notification
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
Settings live in `~/.config/perchd/env`: `PATH` must include `node`, and
`PERCHD_ARGS` holds extra flags such as `-root /home/you/Projects`. The service runs
with `-wait-tailscale 2m`. At boot it waits for tailscaled, and if Tailscale never
comes up it exits so systemd retries, rather than serving on loopback only.

Logs: `journalctl --user -u perchd -f`

See [ARCHITECTURE.md](ARCHITECTURE.md) for how it works and where to extend it.

## License

perchd is licensed under the [Apache License 2.0](LICENSE).
Vendored third-party code retains its own license: see
[the browser dependency notices](web/static/vendor/README.md) and
[markdown-it's MIT license](web/static/vendor/markdown-it.LICENSE).
