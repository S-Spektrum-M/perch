TO-DO
---

1. Fork from an earlier prompt.
    - Pick an earlier user message; open a new session branched just before it with that message in the prompt box.
    - pi's `fork` RPC moves the running process to the new session file, so the manager must re-key the agent under its new session ID before redirecting attached pages; otherwise `/s/<new>` starts a second pi on the same file.
1. I'm interested in the login token idea but I'm not sure how that would work exactly.
1. Live Tool Output
    - I don't really look at tool output but sometimes I might. So this is a good idea but later

## Public-release readiness

- [x] Improve installation and compatibility guidance (recommendation 4).
  - Document required Go and pi versions, supported platforms, and working pi authentication.
  - Add a copy/paste clone → build → run quickstart, experimental status, and known limitations.
  - Update stale UI descriptions, including the README's Miller-columns directory picker description.
- [x] Choose and add a project-level license (recommendation 5): Apache-2.0.
  - Keep vendored dependency notices/licenses; public visibility alone does not grant an open-source license.
- [x] Add GitHub Actions CI (recommendation 6).
  - Run `go test ./...`, `go vet ./...`, and `node --test tests/*.test.cjs`.
  - GitHub secret scanning and push protection verified enabled on the public repository.
  - CI runs on pushes and pull requests; local equivalents passed. Hosted execution awaits pushing the workflow.

## Additional hardening

- [x] Audit and test request-body/upload limits, including oversized and malformed multipart requests.
- [x] Set HTTP header/idle/body timeouts; socket tests cover stalled SSE writers and refreshed stream deadlines.
- [x] Add browser security headers: framing protection, `X-Content-Type-Options: nosniff`, and a restrictive Referrer-Policy.
- [x] Enforce a same-origin Content Security Policy; remove inline scripts and the inline context-meter style. Headless Chrome folder-picker smoke check passed; full interactive browser verification remains advisable.
- [x] Apply and test no-store on non-static responses and revalidation on static assets. Real reverse-proxy/back-button verification remains a manual check.
- [x] Document that saving global model/effort defaults also affects pi outside pircd, subject to project overrides.
- [x] Push tested bare-URL support, release-readiness, and hardening changes. GitHub Actions CI passed (run 39724416296).

Done
---

1. Browser-request protection. ✓ Implemented (cross-origin write checks, conservative handling of missing Origin headers, and Host validation with explicit DNS-name opt-in; this is not authentication).

1. Security boundary documentation. ✓ Implemented (remote-shell warning, restricted tailnet access, no public exposure without authentication, and clarification that `-root` is not a sandbox).

1. Upload cleanup. ✓ Implemented (pircd deletes a session's uploads once neither the session nor its uploads have changed for 7 days and no pi is running for it; checked at startup and hourly, `-upload-retention` to change or disable).

1. File uploads in prompts. ✓ Implemented ("+" button, drag and drop anywhere on the page, or paste a screenshot; files are saved per session under `~/.cache/pircd/uploads` and their paths appended to the prompt for pi to read).

1. Session stats. ✓ Implemented (messages, tokens, cost and context usage with a meter at the top of the "⋯" menu).

1. Slash-commands. ✓ Implemented (typing `/` suggests pi's extension commands, prompt templates and skills; arrows/Tab/Enter or tap to insert).

1. Direct shell commands. ✓ Implemented (`!cmd` shares output with the model on the next prompt, `!!cmd` doesn't; results appear as `$ cmd` entries, with a stop button while running).

1. Session actions. ✓ Implemented (a "⋯" menu on the session page: rename, compact with optional focus instructions, duplicate into a new session, and release the pi process to continue in a terminal).

1. Fix questions UI. ✓ Implemented (select options stack full width with wrapping text instead of overflowing the dialog; tall dialogs scroll).

1. Project/session sidebar on the session page. ✓ Implemented (same tree as the home page, with filtering and the open session highlighted; fixed sidebar on wide screens, drawer from a header button on phones).

1. Fix prompt box. ✓ Implemented (Enter sends and Shift+Enter inserts a newline on desktop; on touch devices Enter is a newline and only the Send button sends).

1. Detect same session open in terminal. ✓ Best-effort Linux /proc check before attaching (open files and explicit --session arguments). Interactive selection and --resume may go undetected; this is not a lock.

1. Markdown rendering. ✓ Implemented (assistant replies and thinking, including live streaming).
    - Must have to make stuff actually readable for long output with headings and such.
2. Diffs for edits. ✓ Implemented (colored edit previews and tool-result diffs).
    - Same as #1 just a bit less important because I don't actually look at every little diff.
3. Done notifications. ✓ Implemented (optional server-side ntfy push, works with the browser closed).
    - So that i'm not glued to my phone.
4. Copy code block button. ✓ Implemented (per-block copying, including streamed code, with clipboard fallback and success/failure feedback).
    - For copying code around.

## Proposed: shared session ownership extension

**Proposal only to properly do conflict detection; not implemented.**

A small Pi extension could prevent terminal
Pi and pircd from editing the same session concurrently, without a network
service or additional UI.

The current Linux `/proc` check is advisory: it checks open file descriptors and
explicit `--session <path>` / `--session=<path>` arguments before pircd attaches.
Pi closes session files between writes, so interactive selection, `--resume`,
and ID-based selection usually go undetected. Unreadable processes, systems
without `/proc`, and races are also unprotected. Existing web agents are not
rechecked. Conversely, a file viewer or a stale command-line argument after a
session switch can cause a false positive.

The proposed extension would:

- Track the current session on startup and session changes.
- Acquire ownership atomically in a local runtime directory, keyed by the
  canonical session-file path. A heartbeat or presence file alone is not a lock.
- Record enough process identity to distinguish a live owner from stale state,
  accounting for PID reuse, and recover safely after crashes.
- Release ownership on session changes and clean shutdown.
- Use the same ownership protocol in terminal Pi and pircd's RPC children, so
  protection works in both directions. pircd would report when a terminal owns
  the requested session rather than attaching to it.

Before implementation, verify that Pi's extension lifecycle hooks can reject
startup or a session switch **before any session writes**. That determines
whether this can enforce exclusion or only provide detection and warnings.
The transition protocol must preserve ownership correctly if a switch fails.

Terminal instances without the extension would remain unprotected, and this
local protocol would not coordinate different machines sharing session storage.
Once the shared protocol works, retire the `/proc` heuristic rather than treating
it as dependable protection.
