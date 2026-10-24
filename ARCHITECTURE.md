# perchd architecture

perchd is a small Go server for driving [pi](https://github.com/earendil-works/pi)
coding-agent sessions from a browser (typically a phone over Tailscale). It is a
single static binary with no dependencies outside the Go standard library. Templates
and assets are embedded.

```
browser
  │ GET pages (server-rendered HTML)
  │ POST prompt / abort / ui
  │ ◀ SSE /s/{id}/events
  ▼
perchd
  ├─ Server    HTTP routes
  ├─ Manager
  │    └─ Agent ×N        transcript mirror, UI events
  └─ engine.Backend       lists and runs sessions
       ├─ pi.Backend      ~/.pi/agent/sessions; pi.RPC ×N ⇄ JSONL ⇄ pi --mode rpc
       ├─ claude.Backend  ~/.claude/projects; claude.Session ×N ⇄ JSONL ⇄ node bridge.mjs (Agent SDK)
       ├─ agy.Backend     $GEMINI_HOME/conversations; agy.Session ×N ⇄ JSON-RPC 2.0 ⇄ agy_acp_server.par
       └─ eta.Backend     stub for Eta (agentd)
```

`-engine` (`PERCHD_ENGINE`) lists the backends, e.g. `pi,claude`; the first is
the default for new sessions. Several can run at once: session IDs are UUIDs in
each, so they share one ID space, and `SessionInfo.Engine` records the owner.

## Design choices

| Decision | Why |
|---|---|
| **Drive pi through `pi --mode rpc`** (one child process per attached session) | RPC mode is pi's official, stable integration surface: it emits the same events the TUI uses, accepts prompts, steering and aborts, and keeps session files compatible with `pi --resume`. perchd never writes session files itself. |
| **Server-side rendering for everything, including live updates** | Every piece of HTML comes from one set of Go templates (`web/templates/fragments.html`). A message looks the same on first load and when it streams in, and changing the UI means editing templates and CSS, not keeping two renderers in sync. |
| **SSE + plain POSTs instead of WebSockets** | Traffic is almost entirely server→client. With SSE the stdlib is enough, it passes through proxies and `tailscale serve`, and the browser reconnects on its own with `Last-Event-ID`, which handles phones sleeping and waking. Commands are ordinary form POSTs, which also work without JavaScript. |
| **Batched frames** | After a wake-up, the SSE handler waits 40 ms and then sends every pending event as one JSON array frame, merging consecutive text deltas for the same block. Token-by-token streaming becomes ~25 small frames a second, and the client applies each frame in one pass. |
| **Bind to tailscale0 + loopback by default** | Anyone who can reach perchd can run commands as you. There is no auth layer; the tailnet is the boundary. |

## Components

```
cmd/perchd/        main: flags, backend choice, listeners, shutdown
internal/engine/  the backend contract: Backend, Engine, Sink; transcript Message
internal/pi/      pi backend: RPC client, session files (Store), busy check, settings
internal/claude/  Claude Agent SDK backend: bridge process client, session list cache
bridge/claude/    bridge.mjs: one SDK query per session, translated to perchd's events
internal/agy/     Antigravity ACP backend: JSON-RPC 2.0 client, SQLite session store
internal/eta/     Eta backend (stub)
internal/notify/  ntfy completion notifications
internal/server/  HTTP handlers, middleware, Agent/Manager, view models
web/              embedded templates and static assets (web.FS)
tests/            Node tests for the browser JS
```

Paths below are relative to those directories.

### `cmd/perchd/main.go`
Flags, listener setup (auto-detects the `tailscale0` IPv4 address) and graceful
shutdown. The HTTP server's `BaseContext` is the signal context, so open SSE streams
end immediately on shutdown. Then every engine is closed.

### `internal/engine/engine.go`: the backend contract
- `Backend` lists stored sessions and starts an `Engine` for one (`Open`,
  `Create`, `Duplicate`).
- `Engine` is one session's commands: prompt, abort, shell, compact, rename,
  models, stats, dialog answers.
- `Sink` is how an engine reports progress: run and message lifecycle, block
  deltas, tool snapshots, status, dialogs, exit. `Agent` implements it.

The server imports only this package, never a backend. Some pi shapes remain:

- The transcript type (`engine.Message`) mirrors pi's `AgentMessage`; other
  backends translate into it.
- `server/tool_output.go` recovers full shell output from pi's
  `pi-bash-*.log` files. That belongs behind the backend, as a "full output for
  this tool result" call.
- Extension statuses, widgets and dialogs follow pi's extension UI protocol.

### `internal/pi/sessions.go`: Store
Lists sessions by scanning `<sessions>/*/*.jsonl` (pi's default per-cwd layout) and
`<sessions>/*.jsonl` (layout used with an explicit `--session-dir`). For each file
it reads the header (id, cwd, created), the latest `session_info` name, the first
user message and the user-message count. Only lines that byte-match a marker are
JSON-decoded, so large tool outputs are skipped cheaply. Results are cached by
(size, mtime). On a 40 MB / 195-session directory: about 120 ms cold, 3 ms warm.

### `internal/claude` and `bridge/claude/bridge.mjs`: Claude engine
The Agent SDK only exists for TypeScript and Python, so each open session runs
`node bridge.mjs '<config>'`, which holds one streaming-input `query()`. The
bridge, not Go, translates SDK messages into perchd's transcript shape and
`Sink`-like events (`run_start`, `block_delta`, `message`, `dialog`, …), so
`claude.Session` is the same JSONL client as `pi.RPC`, minus pi's protocol.

- Session IDs are chosen up front (`sessionId` option), so `Create` and
  `Duplicate` (`resume` + `forkSession`) know the ID before the first prompt.
- Claude Code stores one transcript entry per assistant content block; the
  bridge merges entries of one API message for history, and for live output
  collects them until the next message starts.
- A run lasts until every sent prompt's uuid has been reported by a `result`.
- `canUseTool` turns permission prompts and `AskUserQuestion` into dialogs.
- `List` runs `node bridge.mjs list` (`listSessions()`, ~150 ms) and caches it
  for 5 s, refreshing in the background.

### `internal/agy`: Antigravity ACP engine
Each open session spawns `agy_acp_server.par --uid=` with `GEMINI_HOME` set and
speaks [Agent Client Protocol](https://agentclientprotocol.com/) (JSON-RPC 2.0
over stdin/stdout) directly from Go—no Node bridge is needed.

- `initialize` identifies as `clientInfo.name: "zed"` to unlock the full CCPA
  model catalog, followed by `session/new` or `session/load`.
- During `session/load`, replayed `session/update` notifications are coalesced
  into historical `engine.Message` records before `GetMessages` returns; live
  `session/update` notifications during `session/prompt` drive `engine.Sink`
  deltas and tool snapshots.
- ACP model options that bundle reasoning effort (`<model>-low`, `-medium`,
  `-high`) are split into base model IDs and perchd thinking levels (`SetModel`
  and `SetThinking` re-combine them into `session/set_config_option`).
- `Store` scans `<GEMINI_HOME>/conversations/*.db` (plus sidecar `.perch.json`
  metadata) and supports `Duplicate` by copying the SQLite trajectory database
  and rewriting the 36-byte UUID cascade ID.

### `internal/server/backends.go`: backendSet
The configured backends as one: `List` merges their sessions (newest first,
tagged with `Engine`), `Find` checks every cached list before any backend's own
`Find`, and `get(name)` picks one, the first for "".

### `internal/server/manager.go`: Manager
Owns the running agents, keyed by session ID. Each agent remembers its backend,
which also names it in the UI and owns its model defaults and picker config
(`models.json` for pi, `models-<engine>.json` for others).

- `Attach(info)` opens an existing session in `info.Engine`'s backend, in the session's cwd.
  Concurrent attaches to the same session share one start (a hand-rolled
  singleflight).
- `Create(engine, dir, name)` starts a new session in `dir` and keys it by the session
  ID the engine reports. pi doesn't write the file until the first prompt, so the
  home page also lists live agents that have no file yet.
- A reaper stops agents that have had no viewers, no active run and no pending
  dialog for `-idle` (default 30 min).

### `internal/pi/rpc.go`: pi engine
One `pi --mode rpc` process per open session.

- **RPC plumbing.** stdout is split on LF only. Node's readline-style splitting
  would break on U+2028 inside JSON strings, as pi's docs warn. `call()` tags
  commands with an id and waits for the matching `response`. Everything else is
  either a session event or an extension UI request, translated into `Sink` calls.
- **Stopping** closes stdin; if pi hasn't exited after 5 s, its whole process
  group gets SIGKILL (pi runs with `Setpgid`, so tool subprocesses go too).
- `NewRPC` wraps any writer and takes stdout records through `HandleLine`; the
  server tests drive the real decoder this way.

### `internal/server/agent.go`: Agent
perchd's side of one open session, fed by its engine through `engine.Sink`.

- **Mirror state.** The agent keeps its own copy of the transcript (`messages`,
  seeded by `get_messages` and appended on every `message_end`), the
  in-progress assistant message (`live`), live tool output (`tools`), and status
  (streaming, activity, queue size, model, extension statuses and widgets, open dialogs). Page
  renders read this mirror instead of making an RPC round trip.
- **Event translation.** pi events, through the sink, become a small UI protocol (`UIEvent`):

  | pi event | UIEvent | browser action |
  |---|---|---|
  | `message_start` (assistant) | `live_start` + rendered `live` fragment | append placeholder `#live` |
  | `message_update` `*_start` | `block` + rendered `block` fragment | append block to `#live` |
  | `message_update` `text_delta`/`thinking_delta` | `delta` {idx, s} | append text node |
  | `message_end` | `append` + rendered `msg` fragment (+`last` text) | replace `#live` / live tool output, or append |
  | `tool_execution_start` | `append` + rendered `tool_live` fragment | append expandable tool output |
  | `tool_execution_update` | `tool_delta` {id, s}, optionally `tool_reset` {id} | append 100 lines of literal text; reset on snapshot revision |
  | `tool_execution_end` | `tool_delta` for remaining output | flush incomplete chunk before the completed result |
  | `agent_start`/`agent_settled`/tool/retry/compaction/queue events | `status` + rendered `status` fragment | replace `#status` |
  | `extension_ui_request` select/confirm/input/editor | `dialog` + rendered form | show the form; it POSTs to `/s/{id}/ui` |
  | `notify`, `extension_error` | `notice` | append notice |
  | `compaction_end` | refetch messages, then `reload` | `location.reload()` |

- **Event log.** Every UIEvent gets a sequence number and goes into a ring
  buffer (8192 entries). `Snapshot()` captures the rendered transcript and the
  current sequence number under one lock. The SSE stream resumes from exactly that
  point, so page load and stream can't race. Cursors look like `<epoch>-<seq>`.
  The epoch is random per engine start, so a browser holding a cursor from an
  earlier process gets `reload` instead of wrong deltas. A client that falls
  behind the ring also gets `reload`.
- **Tool output.** `tool_stream.go` treats partial results as replaceable snapshots
  (pi's built-in shell contract). It publishes complete 100-line chunks per tool
  call ID, buffering shorter tails until completion, settlement, or process exit.
  Revised/rolling snapshots clear old output rather than duplicating it. Page
  snapshots include only published text, so SSE replay cannot duplicate buffered
  lines. Nested tool events are left to their parent's result. Live panels open
  automatically, following new output unless the user scrolls up. Completed
  messages replace their live placeholder without the old 6000-character cut.
  `tool_output.go` restores pi's saved shell log when partial/final snapshots
  have been truncated upstream, strips terminal escapes for display, and limits
  saved-log previews to 4 MiB. A download streams the complete file without that
  preview limit. Only regular `pi-bash-*.log` files in the process's temp directory,
  referenced by the session's own completed tool results, can be downloaded;
  symlinks and client-supplied filesystem paths are rejected. History renders
  restore saved logs too; missing/expired logs fall back to the persisted result.

### `internal/engine/messages.go` and `internal/server/views.go`: view models
`engine.Message` loosely decodes pi's `AgentMessage` union (user, assistant, toolResult,
bashExecution, custom, compaction/branch summaries; unknown roles fall back to
plain text). `ViewMessage` turns it into `MsgView`/`BlockView`. All presentation decisions live here:
labels, tool-call one-liners (`bash <command>`, `read <path>`, …), tool argument
previews (6000 bytes), and which messages are hidden (system prompts, `display:false`
custom messages).

### `internal/server/server.go`: HTTP

| Route | Purpose |
|---|---|
| `GET /` | Session list (`?q=` filter, `?all=1`), with live/running indicators and quick `+` new-session buttons |
| `GET /new` | Project & directory picker (recent dirs, discovered git repos, and single-column breadcrumb folder browser) |
| `GET /draft` | Draft of a new session in `dir`: the prompt box, with an engine switch when several are configured. Nothing starts yet |
| `GET /draft/models` | `?engine=`'s models for a new session, as `<option>`s (`engine.ModelCatalog`: `pi --list-models`, or `bridge.mjs models`), filtered by that engine's picker config |
| `POST /new` | Start a session (`dir`, `name`, `engine`), switch it to `model` (`provider/id`) if given, and send it the first `message` (+ `files`), then redirect to it. Without a message, redirects to the draft |
| `GET /s/{id}` | Session page; attaches (spawns pi) on demand. Last 50 messages; “Load earlier” increases `?limit=` by 50 (`?all=1` remains supported). Ordinary text responses use gzip when accepted; SSE stays uncompressed. Includes the home page's project tree as a sidebar |
| `GET /s/{id}/events` | SSE stream of UIEvent batches. Returns 204 when no agent is running, which stops EventSource retries |
| `GET /s/{id}/tool-output/{tool}` | Download the complete saved shell log referenced by a completed tool call in this session; `{tool}` is a DOM-safe encoded call ID, never a filesystem path |
| `POST /s/{id}/prompt` | `message`, plus optional multipart `files`: saved under `-uploads` (default `~/.cache/perchd/uploads`)`/<session id>/`, never overwriting, with their paths appended to the message as "Attached files:" so pi reads them with its tools (200 MB per prompt, see `uploads.go`). Hourly, `cleanUploads` deletes the folders of sessions with no pi running whose session file and uploads are both older than `-upload-retention` (7 days). If a run is in progress it is sent with `streamingBehavior` `steer` (default) or `followUp` (`mode=followUp`, the "Queue" button). `!cmd` / `!!cmd` run a shell command via the `bash` RPC instead (`!!` sets `excludeFromContext`); pi emits no message event for it, so `Agent.RunShell` appends the result itself |
| `POST /s/{id}/abort` | `abort_bash` when only a shell command is running; otherwise `clear_queue` then `abort`. The response body is the dropped queued text, which the client puts back in the prompt box |
| `POST /s/{id}/ui` | Answer an extension dialog (`value` / `confirmed` / `cancelled`) |
| `POST /s/{id}/rename` | `name` → `set_session_name`; open pages update their header via a `name` UIEvent |
| `POST /s/{id}/compact` | Optional `instructions` → `compact`. The status line shows progress and `compaction_end` reloads open pages |
| `POST /s/{id}/duplicate` | Starts a new pi with `--fork <file>` (the original is untouched); a named session's copy gets " (copy)". Responds with the new session URL |
| `GET /s/{id}/commands` | `get_commands` as JSON, for the prompt box's `/` suggestions |
| `GET /s/{id}/stats` | `get_session_stats` rendered as the `stats` fragment for the "⋯" dialog |
| `POST /s/{id}/release` | Stops the session's pi process so it can be continued elsewhere; nothing is deleted. Responds with `/` |

POSTs from `session.js` send `X-Perchd: 1` and get 204 or an error body
(duplicate and release return the URL to go to as text instead). Plain
form posts get a 303 back to the page, so everything except live streaming works
without JavaScript.

### `web/`
- `templates/layout.html`: page shell. Pages define `title`, `nav`, `bodyclass`, `content`.
- `templates/fragments.html`: `msg`, `block`, `live`, `status`, `dialog`, `notice`. These
  are shared by full renders and live updates. `projects` / `proj-group` render the
  project tree used by both the home page and the session sidebar (`projectTree` in
  `server.go` builds its data).
- `templates/new.html`: the picker is one GET form. Each directory is a
  `<button name="path">`, so the typed session name survives navigation with no JS.
  "Go" is the first submit button, so pressing Enter in the path box uses it.
- `static/session.js`: about 150 lines. A table of handlers keyed by `UIEvent.t`,
  fetch-based form posts, Enter to send on desktop (Shift+Enter for a newline; on
  touch devices, detected via `(pointer: coarse)`, Enter is a newline and only the
  Send button sends), stick-to-bottom scrolling, and
  copy-last-response, and the "⋯" session actions dialog (handlers in `actions.go`).
  Copy uses `navigator.clipboard` when available and falls back to
  `execCommand('copy')`. Plain `http://` over a tailnet isn't a secure context, so the
  fallback is what normally runs. The copied text is kept in the page, so the copy runs
  synchronously inside the click, which iOS requires.
- `static/projects.js`: the project tree's live filter, highlighting of the open
  session (expanding any fold that hides it), and the session sidebar's drawer
  toggle. The sidebar is fixed at 1100px and wider and a drawer below that.

## Extending

- **New pi event → UI:** add a case in `Agent.handleEvent`, emit a `UIEvent`
  (usually with HTML from a new fragment), add a handler in `session.js`.
- **Change how messages look:** `fragments.html` + `app.css`; data shaping
  in `ViewMessage`.
- **New session action** (e.g. model switch, compact, fork): add a route that
  calls `a.Call(ctx, map[string]any{"type": "..."})` (see pi `docs/rpc-commands.md`)
  and a button/form in `session.html`.
- **Markdown rendering:** text is currently `white-space: pre-wrap`. Render
  in `ViewMessage` (e.g. goldmark → `template.HTML`), and for live blocks
  either re-render on `text_end` or keep pre-wrap until `message_end`
  replaces the block. That replacement already happens.

## Known limitations

- No authentication. Rely on the tailnet (and the default bind address).
- If the same session is open in a terminal pi at the same time, both processes
  append to the same file. Don't do that.
- Images in prompts aren't supported from the web UI yet (RPC `images` field).
- Tool streaming depends on tools emitting partial results, interpreted as
  replaceable snapshots. Tools using delta-only partial results need an adapter.
  Direct `!`/`!!` shell output is not streamed; completed output is shown.
