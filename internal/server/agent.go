package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"perchd/internal/engine"
)

// Renderer renders a named template fragment to HTML. Agents use it so live
// updates are rendered by the same templates as full page loads.
type Renderer interface {
	Fragment(name string, data any) string
}

// UIEvent is one update pushed to browsers. The browser side (session.js)
// only knows how to insert HTML and append text; all rendering happens here.
type UIEvent struct {
	Seq       uint64  `json:"seq"`
	T         string  `json:"t"` // append, live_start, block, delta, tool_delta, tool_reset, status, dialog, dialog_close, notice, editor, name, reload
	HTML      string  `json:"html,omitempty"`
	ID        string  `json:"id,omitempty"`
	Idx       int     `json:"idx"`
	S         string  `json:"s,omitempty"`
	Streaming *bool   `json:"streaming,omitempty"`
	Last      *string `json:"last,omitempty"`
}

// liveMsg is the assistant message currently being streamed.
type liveMsg struct {
	Label  string // who is speaking: the backend's name
	Blocks []BlockView
}

func (l *liveMsg) block(idx int) *BlockView {
	for i := range l.Blocks {
		if l.Blocks[i].Idx == idx {
			return &l.Blocks[i]
		}
	}
	return nil
}

// statusView feeds the "status" template.
type statusView struct {
	TaskTime  string
	Tokens    string
	Streaming bool
	Busy      bool // streaming, compacting or running a shell command
	Shell     bool // a shell command is running; offer to stop it
	Activity  string
	Queue     int
	Model     string
	Thinking  string
	Ext       []extStatus
	Widgets   [][]string
	Exited    bool
	ExitErr   string
}

// PromptState is the prompt border's semantic color, shared by page renders and SSE.
func (s statusView) PromptState() string {
	switch {
	case s.Exited:
		return "exited"
	case s.Busy && s.Activity == "responding":
		return "responding"
	case s.Busy:
		return "busy"
	default:
		return "idle"
	}
}

// PromptLabel preserves activity details for hover text and screen readers.
func (s statusView) PromptLabel() string {
	switch {
	case s.Exited:
		if s.ExitErr != "" {
			return s.ExitErr
		}
		return "pi exited"
	case s.Busy:
		if s.Activity != "" {
			return s.Activity
		}
		return "working"
	default:
		return "idle"
	}
}

// extStatus is one extension's status-bar entry (ctx.ui.setStatus).
type extStatus struct {
	Key  string // usually the extension name
	Text string
}

// dialogView feeds the "dialog" template (extension UI select/confirm/input/editor).
type dialogView struct {
	Session     string
	ID          string
	Method      string
	Title       string
	Message     string
	Placeholder string
	Prefill     string
	Options     []string
}

type noticeView struct {
	Kind string // info, warning, error
	Text string
}

const ringSize = 8192

// Agent is perchd's side of one open session: a mirror of the transcript and
// status that page renders read, and the UI events that keep open pages in
// sync. The engine that runs the session reports to it as an engine.Sink.
type Agent struct {
	ID    string
	Epoch string // changes per engine start; lets browsers detect a restart
	Cwd   string

	engine  engine.Engine
	backend engine.Backend       // the backend the engine belongs to
	notify  func(string, string) // guarded by mu; invoked asynchronously
	render  Renderer

	done chan struct{} // closed when the engine has stopped

	nameMu      sync.Mutex // serializes manual and generated name writes
	mu          sync.Mutex
	nameVersion uint64 // invalidates suggestions after a rename, including clearing a name
	naming      bool
	file        string
	name        string
	model       string
	modelKey    modelKey
	modelLabels map[modelKey]string // presentation only; never sent to the engine or saved as defaults
	thinking    string
	messages    []engine.Message
	live        *liveMsg
	tools       []*liveTool
	streaming   bool
	taskStarted time.Time
	taskTime    string
	taskRead    int
	taskWritten int
	compacting  bool   // also true for manual compaction, which runs outside a turn
	shell       string // command of a running `!`/`!!` shell command
	activity    string
	queue       int
	extStatus   map[string]string
	hideExt     map[string]bool // status keys not to show (TUI-only extensions)
	widgets     map[string][]string
	dialogs     []dialogView
	exited      bool
	exitErr     string

	seq      uint64
	ring     []UIEvent
	subs     map[chan struct{}]struct{}
	lastSeen time.Time
}

var _ engine.Sink = (*Agent)(nil)

// newAgent returns an agent for a session whose engine is about to start in dir.
func newAgent(dir string, render Renderer, hideStatus map[string]bool) *Agent {
	epoch := make([]byte, 4)
	_, _ = rand.Read(epoch)
	return &Agent{
		Epoch:     hex.EncodeToString(epoch),
		Cwd:       dir,
		render:    render,
		done:      make(chan struct{}),
		extStatus: map[string]string{},
		hideExt:   hideStatus,
		widgets:   map[string][]string{},
		subs:      map[chan struct{}]struct{}{},
		lastSeen:  time.Now(),
	}
}

// load reads the session's state and transcript from the engine.
func (a *Agent) load(ctx context.Context) error {
	st, err := a.engine.State(ctx)
	if err != nil {
		return err
	}
	msgs, err := a.engine.Messages(ctx)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.ID, a.file, a.name, a.thinking = st.SessionID, st.SessionFile, st.SessionName, st.Thinking
	if st.Model != nil {
		a.model = cmp(st.Model.Name, st.Model.ID)
		a.modelKey = modelKey{st.Model.Provider, st.Model.ID}
	}
	a.messages = msgs
	return nil
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// SetModel switches the session's model and refreshes the status line.
func (a *Agent) SetModel(ctx context.Context, provider, id string) error {
	if err := a.engine.SetModel(ctx, provider, id); err != nil {
		return err
	}
	st, err := a.engine.State(ctx)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.thinking = st.Thinking
	if st.Model != nil {
		a.model = cmp(st.Model.Name, st.Model.ID)
		a.modelKey = modelKey{st.Model.Provider, st.Model.ID}
	}
	a.emitStatusLocked()
	return nil
}

// Close stops the engine.
func (a *Agent) Close() {
	a.engine.Close()
}

// ---------------------------------------------------------------------------
// Commands that also update the mirror

// RunShell runs a direct shell command (`!cmd` in the TUI) and appends its
// result to the transcript. Unless exclude is set, the engine hands the
// output to the model with the next prompt.
func (a *Agent) RunShell(command string, exclude bool) {
	a.mu.Lock()
	if a.shell != "" {
		a.noticeLocked("error", "a shell command is already running")
		a.mu.Unlock()
		return
	}
	a.shell = command
	if !a.streaming {
		a.activity = "$ " + engine.OneLine(command, 120)
	}
	a.emitStatusLocked()
	a.mu.Unlock()

	res, err := a.engine.Shell(context.Background(), command, exclude)

	a.mu.Lock()
	defer a.mu.Unlock()
	a.shell = ""
	if !a.streaming {
		a.activity = ""
	}
	a.emitStatusLocked()
	if err != nil {
		a.noticeLocked("error", "$ "+command+": "+err.Error())
		return
	}
	if res.Cancelled {
		res.Output = strings.TrimRight(res.Output, "\n") + "\n[cancelled]"
	}
	m := engine.Message{Role: "bashExecution", Command: command, Output: res.Output, ExitCode: res.ExitCode, Timestamp: time.Now().UnixMilli()}
	a.messages = append(a.messages, m)
	if v, ok := a.viewMessageLocked(m); ok {
		a.emitLocked(UIEvent{T: "append", HTML: a.render.Fragment("msg", v)})
	}
}

// ShellRunning reports whether a direct shell command is running.
func (a *Agent) ShellRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.shell != ""
}

// SetName renames the session.
func (a *Agent) SetName(ctx context.Context, name string) error {
	a.nameMu.Lock()
	defer a.nameMu.Unlock()
	a.mu.Lock()
	a.nameVersion++
	a.mu.Unlock()
	if err := a.engine.SetName(ctx, name); err != nil {
		return err
	}
	a.NameChanged(name)
	return nil
}

// RespondUI answers a dialog and closes it on open pages.
func (a *Agent) RespondUI(id string, resp engine.UIResponse) error {
	err := a.engine.RespondUI(id, resp)
	a.DialogClosed(id)
	return err
}

// ---------------------------------------------------------------------------
// engine.Sink: engine progress -> mirror and UI events

func (a *Agent) RunStarted() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.streaming, a.activity = true, "working"
	a.emitStatusLocked()
}

func (a *Agent) RunSettled() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.flushToolsLocked()
	if !a.taskStarted.IsZero() {
		a.taskTime = time.Since(a.taskStarted).Round(100 * time.Millisecond).String()
		a.taskStarted = time.Time{}
	}
	if a.streaming && a.notify != nil {
		label := a.name
		if label == "" {
			label = filepath.Base(a.Cwd)
		}
		go a.notify(a.ID, label)
	}
	a.streaming, a.activity = false, ""
	a.live = nil
	a.emitStatusLocked()
}

func (a *Agent) AssistantStarted() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.live = &liveMsg{Label: a.EngineName()}
	a.activity = "responding"
	a.emitLocked(UIEvent{T: "live_start", HTML: a.render.Fragment("live", a.live)})
	a.emitStatusLocked()
}

// liveLocked returns the streaming message, starting one if the engine
// skipped AssistantStarted.
func (a *Agent) liveLocked() *liveMsg {
	if a.live == nil {
		a.live = &liveMsg{Label: a.EngineName()}
		a.emitLocked(UIEvent{T: "live_start", HTML: a.render.Fragment("live", a.live)})
	}
	return a.live
}

func (a *Agent) BlockStarted(idx int, kind engine.BlockKind, toolName string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	live := a.liveLocked()
	b := BlockView{Kind: string(kind), Idx: idx}
	if kind == engine.BlockTool {
		b.Summary = toolName + " …"
	}
	live.Blocks = append(live.Blocks, b)
	a.emitLocked(UIEvent{T: "block", Idx: idx, HTML: a.render.Fragment("block", b)})
}

func (a *Agent) BlockDelta(idx int, text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if b := a.liveLocked().block(idx); b != nil {
		b.Text += text
		a.emitLocked(UIEvent{T: "delta", Idx: idx, S: text})
	}
}

func (a *Agent) MessageEnded(m engine.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if m.Role == "assistant" && m.Usage != nil {
		a.taskRead += m.Usage.Input + m.Usage.CacheRead + m.Usage.CacheWrite
		a.taskWritten += m.Usage.Output
	}
	a.messages = append(a.messages, m)
	ev := UIEvent{T: "append"}
	if m.Role == "toolResult" {
		if t := a.toolLocked(m.ToolCallID); t != nil {
			a.publishToolLocked(t, t.output, true)
			ev.ID = t.ID // replaces the live tool output, not a second result
			a.removeToolLocked(m.ToolCallID)
		}
	}
	if v, ok := a.viewMessageLocked(m); ok {
		ev.HTML = a.render.Fragment("msg", v)
	}
	if m.Role == "assistant" {
		ev.ID = "live" // replaces the streaming placeholder
		a.live = nil
		if t := m.PlainText(); t != "" {
			ev.Last = &t
		}
	}
	a.emitLocked(ev)
}

func (a *Agent) ToolStarted(callID, name string, args json.RawMessage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.startToolLocked(callID, name, args)
	a.activity = "running " + toolSummary(name, args)
	a.emitStatusLocked()
}

func (a *Agent) ToolUpdated(callID, name string, args json.RawMessage, partial engine.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t := a.startToolLocked(callID, name, args); t != nil {
		output, _ := toolDisplayOutput(partial)
		a.publishToolLocked(t, output, false)
	}
}

func (a *Agent) ToolEnded(callID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t := a.toolLocked(callID); t != nil {
		a.publishToolLocked(t, t.output, true)
	}
	a.activity = "working"
	a.emitStatusLocked()
}

func (a *Agent) Retrying(attempt, max int, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.activity = fmt.Sprintf("retrying (%d/%d): %s", attempt, max, engine.OneLine(reason, 120))
	a.emitStatusLocked()
}

func (a *Agent) RetryEnded() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.activity = "working"
	a.emitStatusLocked()
}

func (a *Agent) CompactionStarted() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.compacting, a.activity = true, "compacting context"
	a.emitStatusLocked()
}

func (a *Agent) CompactionEnded() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.compacting, a.activity = false, ""
	if a.streaming {
		a.activity = "working"
	}
	a.emitStatusLocked()
}

// TranscriptReplaced swaps the transcript and tells open pages to reload.
func (a *Agent) TranscriptReplaced(msgs []engine.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = msgs
	a.emitLocked(UIEvent{T: "reload"})
}

func (a *Agent) QueueChanged(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queue = n
	a.emitStatusLocked()
}

// NameChanged records the session name and updates open pages' headers.
func (a *Agent) NameChanged(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if name == a.name {
		return
	}
	a.name = name
	a.nameVersion++
	a.emitLocked(UIEvent{T: "name", S: name})
}

func (a *Agent) ThinkingChanged(level string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.thinking = level
	a.emitStatusLocked()
}

func (a *Agent) Notice(kind, text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.noticeLocked(kind, text)
}

func (a *Agent) DialogOpened(d engine.Dialog) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := dialogView{
		Session: a.ID, ID: d.ID, Method: d.Method, Title: d.Title, Message: d.Message,
		Placeholder: d.Placeholder, Prefill: d.Prefill, Options: d.Options,
	}
	a.dialogs = append(a.dialogs, v)
	a.emitLocked(UIEvent{T: "dialog", ID: v.ID, HTML: a.render.Fragment("dialog", v)})
}

func (a *Agent) DialogClosed(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, d := range a.dialogs {
		if d.ID == id {
			a.dialogs = append(a.dialogs[:i], a.dialogs[i+1:]...)
			a.emitLocked(UIEvent{T: "dialog_close", ID: id})
			return
		}
	}
}

func (a *Agent) StatusChanged(key, text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hideExt[key] {
		return
	}
	old, had := a.extStatus[key]
	if text == "" {
		if !had {
			return
		}
		delete(a.extStatus, key)
	} else {
		if had && old == text {
			return
		}
		a.extStatus[key] = text
	}
	a.emitStatusLocked()
}

func (a *Agent) WidgetChanged(key string, lines []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if lines == nil {
		delete(a.widgets, key)
	} else {
		a.widgets[key] = lines
	}
	a.emitStatusLocked()
}

func (a *Agent) EditorText(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.emitLocked(UIEvent{T: "editor", S: text})
}

func (a *Agent) Stopped(reason string) {
	a.mu.Lock()
	if a.exited {
		a.mu.Unlock()
		return
	}
	a.flushToolsLocked()
	a.exited = true
	a.streaming = false
	a.live = nil
	a.exitErr = reason
	a.emitStatusLocked()
	a.mu.Unlock()
	close(a.done)
	log.Printf("agent %s: %s", a.ID, reason)
}

func (a *Agent) noticeLocked(kind, text string) {
	a.emitLocked(UIEvent{T: "notice", HTML: a.render.Fragment("notice", noticeView{Kind: kind, Text: text})})
}

// ---------------------------------------------------------------------------
// Event fan-out

func (a *Agent) statusLocked() statusView {
	s := statusView{
		Streaming: a.streaming, Busy: a.streaming || a.compacting || a.shell != "", Shell: a.shell != "",
		Activity: a.activity, Queue: a.queue, TaskTime: a.taskTime,
		Model: cmp(a.modelLabels[a.modelKey], a.model), Thinking: a.thinking, Exited: a.exited, ExitErr: a.exitErr,
	}
	if a.taskTime != "" && !a.streaming {
		s.Tokens = fmt.Sprintf("Tokens read: %d · written: %d", a.taskRead, a.taskWritten)
	}
	for _, k := range sortedKeys(a.extStatus) {
		s.Ext = append(s.Ext, extStatus{Key: k, Text: a.extStatus[k]})
	}
	for _, k := range sortedKeys(a.widgets) {
		s.Widgets = append(s.Widgets, a.widgets[k])
	}
	return s
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (a *Agent) emitStatusLocked() {
	streaming := a.streaming
	a.emitLocked(UIEvent{T: "status", HTML: a.render.Fragment("status", a.statusLocked()), Streaming: &streaming})
}

func (a *Agent) emitLocked(ev UIEvent) {
	a.seq++
	ev.Seq = a.seq
	if len(a.ring) >= ringSize {
		a.ring = append(a.ring[:0], a.ring[ringSize/2:]...)
	}
	a.ring = append(a.ring, ev)
	for ch := range a.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Subscribe returns a channel that is signalled whenever new events exist.
func (a *Agent) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	a.mu.Lock()
	a.subs[ch] = struct{}{}
	a.lastSeen = time.Now()
	a.mu.Unlock()
	return ch, func() {
		a.mu.Lock()
		delete(a.subs, ch)
		a.lastSeen = time.Now()
		a.mu.Unlock()
	}
}

// EventsSince returns events after seq. reset is true when the caller is too
// far behind (or ahead) to catch up and should reload the page instead.
func (a *Agent) EventsSince(seq uint64) (evs []UIEvent, reset bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if seq > a.seq {
		return nil, true
	}
	if seq == a.seq {
		return nil, false
	}
	if len(a.ring) == 0 || a.ring[0].Seq > seq+1 {
		return nil, true
	}
	i := sort.Search(len(a.ring), func(i int) bool { return a.ring[i].Seq > seq })
	return append([]UIEvent(nil), a.ring[i:]...), false
}

// Idle reports whether nobody is watching and nothing is happening.
func (a *Agent) Idle(after time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.subs) == 0 && !a.streaming && len(a.dialogs) == 0 && time.Since(a.lastSeen) > after
}

// ---------------------------------------------------------------------------
// Page snapshot

// Snapshot is everything the session page needs, captured atomically with the
// event sequence number it corresponds to.
type Snapshot struct {
	ID, Epoch  string
	Engine     string // backend name
	Seq        uint64
	Name, File string
	Cwd        string
	Messages   []MsgView
	Hidden     int // older messages not rendered
	Live       *liveMsg
	Tools      []liveTool
	Status     statusView
	Dialogs    []dialogView
	LastText   string
}

func (a *Agent) Snapshot(limit int) Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := Snapshot{
		ID: a.ID, Epoch: a.Epoch, Engine: a.EngineName(), Seq: a.seq, Name: a.name, File: a.file, Cwd: a.Cwd,
		Status:   a.statusLocked(),
		Dialogs:  append([]dialogView(nil), a.dialogs...),
		LastText: engine.LastAssistantText(a.messages),
	}
	msgs := a.messages
	if limit > 0 && len(msgs) > limit {
		s.Hidden = len(msgs) - limit
		msgs = msgs[len(msgs)-limit:]
	}
	for _, m := range msgs {
		if v, ok := a.viewMessageLocked(m); ok {
			s.Messages = append(s.Messages, v)
		}
	}
	for _, t := range a.tools {
		s.Tools = append(s.Tools, liveTool{ID: t.ID, Summary: t.Summary, Text: t.Text})
	}
	if a.live != nil {
		cp := liveMsg{Label: a.live.Label, Blocks: append([]BlockView(nil), a.live.Blocks...)}
		s.Live = &cp
	}
	return s
}

// Info describes the agent for the session list.
func (a *Agent) Info() (info engine.SessionInfo, streaming bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	info = engine.SessionInfo{ID: a.ID, File: a.file, Cwd: a.Cwd, Name: a.name, Modified: time.Now(), Engine: a.EngineName()}
	for _, m := range a.messages {
		if m.Role == "user" {
			info.Prompts++
			if info.FirstMsg == "" {
				info.FirstMsg = engine.OneLine(m.PlainText(), 140)
			}
		}
	}
	return info, a.streaming
}

// EngineName is the name of the agent's backend, e.g. "pi".
func (a *Agent) EngineName() string {
	if a.backend == nil {
		return "agent"
	}
	return a.backend.Name()
}

// Streaming reports whether a run is in progress.
func (a *Agent) Streaming() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.streaming
}

// Exited reports whether the pi process is gone.
func (a *Agent) Exited() bool {
	select {
	case <-a.done:
		return true
	default:
		return false
	}
}
