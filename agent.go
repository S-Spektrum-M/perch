package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
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

type rpcResponse struct {
	Success bool            `json:"success"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`
}

const ringSize = 8192

// Agent is one `pi --mode rpc` child process bound to one session.
type Agent struct {
	ID    string
	Epoch string // changes per process; lets browsers detect a restart
	Cwd   string

	notify func(string, string) // guarded by mu; invoked asynchronously
	render Renderer
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	wmu    sync.Mutex

	pmu     sync.Mutex
	pending map[string]chan rpcResponse
	nextID  atomic.Uint64

	done chan struct{} // closed when the process has exited

	mu          sync.Mutex
	file        string
	name        string
	model       string
	thinking    string
	messages    []Message
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
	stderr      []byte // tail of stderr, for diagnostics

	seq      uint64
	ring     []UIEvent
	subs     map[chan struct{}]struct{}
	lastSeen time.Time
}

// startAgent launches pi in RPC mode and loads its initial state.
func startAgent(bin string, args []string, dir string, render Renderer, hideStatus map[string]bool) (*Agent, error) {
	cmd := exec.Command(bin, append([]string{"--mode", "rpc"}, args...)...)
	cmd.Dir = dir
	// Own process group, so stopping the agent also stops its tool processes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start pi: %w", err)
	}

	epoch := make([]byte, 4)
	_, _ = rand.Read(epoch)
	a := &Agent{
		Epoch:     hex.EncodeToString(epoch),
		Cwd:       dir,
		render:    render,
		cmd:       cmd,
		stdin:     stdin,
		pending:   map[string]chan rpcResponse{},
		done:      make(chan struct{}),
		extStatus: map[string]string{},
		hideExt:   hideStatus,
		widgets:   map[string][]string{},
		subs:      map[chan struct{}]struct{}{},
		lastSeen:  time.Now(),
	}

	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); a.readLoop(stdout) }()
	go func() { defer readers.Done(); a.stderrLoop(stderr) }()
	go func() {
		readers.Wait()
		a.onExit(cmd.Wait())
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.loadState(ctx); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

func (a *Agent) loadState(ctx context.Context) error {
	raw, err := a.Call(ctx, map[string]any{"type": "get_state"})
	if err != nil {
		return fmt.Errorf("get_state: %w", err)
	}
	var st struct {
		SessionFile   string `json:"sessionFile"`
		SessionID     string `json:"sessionId"`
		SessionName   string `json:"sessionName"`
		ThinkingLevel string `json:"thinkingLevel"`
		IsStreaming   bool   `json:"isStreaming"`
		Model         *struct {
			Name string `json:"name"`
			ID   string `json:"id"`
		} `json:"model"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	msgs, err := a.fetchMessages(ctx)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.ID, a.file, a.name, a.thinking = st.SessionID, st.SessionFile, st.SessionName, st.ThinkingLevel
	if st.Model != nil {
		a.model = cmp(st.Model.Name, st.Model.ID)
	}
	a.messages = msgs
	return nil
}

func (a *Agent) fetchMessages(ctx context.Context) ([]Message, error) {
	raw, err := a.Call(ctx, map[string]any{"type": "get_messages"})
	if err != nil {
		return nil, fmt.Errorf("get_messages: %w", err)
	}
	var data struct {
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	return data.Messages, nil
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// RPC plumbing

func (a *Agent) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	a.wmu.Lock()
	defer a.wmu.Unlock()
	_, err = a.stdin.Write(append(b, '\n'))
	return err
}

// Call sends a command and waits for its response.
func (a *Agent) Call(ctx context.Context, cmd map[string]any) (json.RawMessage, error) {
	id := fmt.Sprintf("pircd-%d", a.nextID.Add(1))
	cmd["id"] = id
	ch := make(chan rpcResponse, 1)
	a.pmu.Lock()
	a.pending[id] = ch
	a.pmu.Unlock()
	defer func() {
		a.pmu.Lock()
		delete(a.pending, id)
		a.pmu.Unlock()
	}()

	if err := a.send(cmd); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if !r.Success {
			return nil, errors.New(cmp(r.Error, "command failed"))
		}
		return r.Data, nil
	case <-a.done:
		return nil, errors.New("pi process exited")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// readLoop splits stdout on LF only (JSON strings may contain U+2028).
func (a *Agent) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := br.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			a.handleLine(line)
		}
		if err != nil {
			return
		}
	}
}

func (a *Agent) stderrLoop(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			a.mu.Lock()
			a.stderr = append(a.stderr, buf[:n]...)
			if len(a.stderr) > 4096 {
				a.stderr = a.stderr[len(a.stderr)-4096:]
			}
			a.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (a *Agent) handleLine(line []byte) {
	var env struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		rpcResponse
	}
	if err := json.Unmarshal(line, &env); err != nil {
		log.Printf("agent %s: bad record: %v", a.ID, err)
		return
	}
	switch env.Type {
	case "response":
		a.pmu.Lock()
		ch := a.pending[env.ID]
		a.pmu.Unlock()
		if ch != nil {
			ch <- env.rpcResponse
		}
	case "extension_ui_request":
		a.handleUIRequest(line)
	default:
		a.handleEvent(env.Type, line)
	}
}

func (a *Agent) onExit(err error) {
	a.mu.Lock()
	a.flushToolsLocked()
	a.exited = true
	a.streaming = false
	a.live = nil
	a.exitErr = "pi exited"
	if err != nil {
		a.exitErr += ": " + err.Error()
	}
	if tail := strings.TrimSpace(string(a.stderr)); tail != "" {
		a.exitErr += "\n" + tail
	}
	a.emitStatusLocked()
	a.mu.Unlock()
	close(a.done)
	log.Printf("agent %s: %s", a.ID, a.exitErr)
}

// Close asks pi to shut down (by closing stdin) and kills it if it does not.
func (a *Agent) Close() {
	_ = a.stdin.Close()
	select {
	case <-a.done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-a.cmd.Process.Pid, syscall.SIGKILL)
		<-a.done
	}
}

// ---------------------------------------------------------------------------
// Session events -> UI events

// handleEvent updates the agent's mirror of the session and emits UI events.
// See pi's docs/json.md for the event shapes.
func (a *Agent) handleEvent(typ string, line []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch typ {
	case "agent_start":
		a.streaming, a.activity = true, "working"
		a.emitStatusLocked()

	case "agent_settled":
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

	case "message_start":
		var e struct {
			Message Message `json:"message"`
		}
		if json.Unmarshal(line, &e) == nil && e.Message.Role == "assistant" {
			a.live = &liveMsg{}
			a.activity = "responding"
			a.emitLocked(UIEvent{T: "live_start", HTML: a.render.Fragment("live", a.live)})
			a.emitStatusLocked()
		}

	case "message_update":
		var e struct {
			E struct {
				Type         string `json:"type"`
				ContentIndex int    `json:"contentIndex"`
				Delta        string `json:"delta"`
				ToolName     string `json:"toolName"`
			} `json:"assistantMessageEvent"`
		}
		if json.Unmarshal(line, &e) != nil {
			return
		}
		if a.live == nil {
			a.live = &liveMsg{}
			a.emitLocked(UIEvent{T: "live_start", HTML: a.render.Fragment("live", a.live)})
		}
		idx := e.E.ContentIndex
		switch e.E.Type {
		case "text_start", "thinking_start", "toolcall_start":
			b := BlockView{Kind: strings.TrimSuffix(e.E.Type, "_start"), Idx: idx}
			if b.Kind == "toolcall" {
				b.Kind, b.Summary = "tool", e.E.ToolName+" …"
			}
			a.live.Blocks = append(a.live.Blocks, b)
			a.emitLocked(UIEvent{T: "block", Idx: idx, HTML: a.render.Fragment("block", b)})
		case "text_delta", "thinking_delta":
			if b := a.live.block(idx); b != nil {
				b.Text += e.E.Delta
				a.emitLocked(UIEvent{T: "delta", Idx: idx, S: e.E.Delta})
			}
		}

	case "message_end":
		var e struct {
			Message Message `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil {
			return
		}
		m := e.Message
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

	case "tool_execution_start", "tool_execution_update", "tool_execution_end":
		var e struct {
			ToolCallID       string          `json:"toolCallId"`
			ParentToolCallID string          `json:"parentToolCallId"`
			ToolName         string          `json:"toolName"`
			Args             json.RawMessage `json:"args"`
			PartialResult    Message         `json:"partialResult"`
		}
		if json.Unmarshal(line, &e) != nil || e.ParentToolCallID != "" {
			return // nested tools are represented by their parent's result
		}
		switch typ {
		case "tool_execution_start":
			a.startToolLocked(e.ToolCallID, e.ToolName, e.Args)
			a.activity = "running " + toolSummary(e.ToolName, e.Args)
			a.emitStatusLocked()
		case "tool_execution_update":
			if t := a.startToolLocked(e.ToolCallID, e.ToolName, e.Args); t != nil {
				e.PartialResult.ToolName = e.ToolName
				output, _ := toolDisplayOutput(e.PartialResult)
				a.publishToolLocked(t, output, false)
			}
		case "tool_execution_end":
			if t := a.toolLocked(e.ToolCallID); t != nil {
				a.publishToolLocked(t, t.output, true)
			}
			a.activity = "working"
			a.emitStatusLocked()
		}

	case "auto_retry_end", "summarization_retry_finished":
		a.activity = "working"
		a.emitStatusLocked()

	case "compaction_start":
		a.compacting, a.activity = true, "compacting context"
		a.emitStatusLocked()

	case "compaction_end":
		a.compacting, a.activity = false, ""
		if a.streaming {
			a.activity = "working"
		}
		a.emitStatusLocked()
		go a.reloadMessages()

	case "auto_retry_start", "summarization_retry_scheduled":
		var e struct {
			Attempt      int    `json:"attempt"`
			MaxAttempts  int    `json:"maxAttempts"`
			ErrorMessage string `json:"errorMessage"`
		}
		if json.Unmarshal(line, &e) == nil {
			a.activity = fmt.Sprintf("retrying (%d/%d): %s", e.Attempt, e.MaxAttempts, oneLine(e.ErrorMessage, 120))
			a.emitStatusLocked()
		}

	case "queue_update":
		var e struct {
			Steering []string `json:"steering"`
			FollowUp []string `json:"followUp"`
		}
		if json.Unmarshal(line, &e) == nil {
			a.queue = len(e.Steering) + len(e.FollowUp)
			a.emitStatusLocked()
		}

	case "session_info_changed":
		var e struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(line, &e) == nil {
			a.setNameLocked(e.Name)
		}

	case "thinking_level_changed":
		var e struct {
			Level string `json:"level"`
		}
		if json.Unmarshal(line, &e) == nil {
			a.thinking = e.Level
			a.emitStatusLocked()
		}

	case "extension_error":
		var e struct {
			ExtensionPath string `json:"extensionPath"`
			Error         string `json:"error"`
		}
		if json.Unmarshal(line, &e) == nil {
			a.noticeLocked("error", "extension error ("+e.ExtensionPath+"): "+e.Error)
		}
	}
}

// RunBash runs a direct shell command (`!cmd` in the TUI) and appends its
// result to the transcript; pi stores it but emits no message event for it.
// Unless exclude is set, pi hands the output to the model with the next prompt.
func (a *Agent) RunBash(command string, exclude bool) {
	a.mu.Lock()
	if a.shell != "" {
		a.noticeLocked("error", "a shell command is already running")
		a.mu.Unlock()
		return
	}
	a.shell = command
	if !a.streaming {
		a.activity = "$ " + oneLine(command, 120)
	}
	a.emitStatusLocked()
	a.mu.Unlock()

	raw, err := a.Call(context.Background(), map[string]any{"type": "bash", "command": command, "excludeFromContext": exclude})
	var res struct {
		Output    string `json:"output"`
		ExitCode  *int   `json:"exitCode"`
		Cancelled bool   `json:"cancelled"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &res)
	}

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
	m := Message{Role: "bashExecution", Command: command, Output: res.Output, ExitCode: res.ExitCode, Timestamp: time.Now().UnixMilli()}
	a.messages = append(a.messages, m)
	if v, ok := ViewMessage(m); ok {
		a.emitLocked(UIEvent{T: "append", HTML: a.render.Fragment("msg", v)})
	}
}

// ShellRunning reports whether a direct shell command is running.
func (a *Agent) ShellRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.shell != ""
}

// setNameLocked records the session name and updates open pages' headers.
func (a *Agent) setNameLocked(name string) {
	if name == a.name {
		return
	}
	a.name = name
	a.emitLocked(UIEvent{T: "name", S: name})
}

// reloadMessages refetches the transcript (after compaction rewrites it) and
// tells browsers to reload.
func (a *Agent) reloadMessages() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msgs, err := a.fetchMessages(ctx)
	if err != nil {
		log.Printf("agent %s: reload: %v", a.ID, err)
		return
	}
	a.mu.Lock()
	a.messages = msgs
	a.emitLocked(UIEvent{T: "reload"})
	a.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Extension UI (docs/rpc-extension-ui.md)

func (a *Agent) handleUIRequest(line []byte) {
	var r struct {
		ID          string    `json:"id"`
		Method      string    `json:"method"`
		Title       string    `json:"title"`
		Message     string    `json:"message"`
		Placeholder string    `json:"placeholder"`
		Prefill     string    `json:"prefill"`
		Options     []string  `json:"options"`
		Timeout     int       `json:"timeout"`
		NotifyType  string    `json:"notifyType"`
		StatusKey   string    `json:"statusKey"`
		StatusText  *string   `json:"statusText"`
		WidgetKey   string    `json:"widgetKey"`
		WidgetLines *[]string `json:"widgetLines"`
		Text        string    `json:"text"`
	}
	if json.Unmarshal(line, &r) != nil {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	switch r.Method {
	case "select", "confirm", "input", "editor":
		d := dialogView{
			Session: a.ID, ID: r.ID, Method: r.Method, Title: r.Title, Message: r.Message,
			Placeholder: r.Placeholder, Prefill: r.Prefill, Options: r.Options,
		}
		a.dialogs = append(a.dialogs, d)
		a.emitLocked(UIEvent{T: "dialog", ID: d.ID, HTML: a.render.Fragment("dialog", d)})
		if r.Timeout > 0 {
			// pi resolves timed-out dialogs itself; just drop ours.
			time.AfterFunc(time.Duration(r.Timeout)*time.Millisecond, func() {
				a.mu.Lock()
				a.closeDialogLocked(r.ID)
				a.mu.Unlock()
			})
		}

	case "notify":
		a.noticeLocked(cmp(r.NotifyType, "info"), r.Message)

	case "setStatus":
		if a.hideExt[r.StatusKey] {
			return
		}
		old, had := a.extStatus[r.StatusKey]
		if r.StatusText == nil || *r.StatusText == "" {
			if !had {
				return
			}
			delete(a.extStatus, r.StatusKey)
		} else {
			if had && old == *r.StatusText {
				return
			}
			a.extStatus[r.StatusKey] = *r.StatusText
		}
		a.emitStatusLocked()

	case "setWidget":
		if r.WidgetLines == nil {
			delete(a.widgets, r.WidgetKey)
		} else {
			a.widgets[r.WidgetKey] = *r.WidgetLines
		}
		a.emitStatusLocked()

	case "set_editor_text":
		a.emitLocked(UIEvent{T: "editor", S: r.Text})
	}
}

// RespondUI answers an extension dialog. resp holds value/confirmed/cancelled.
func (a *Agent) RespondUI(id string, resp map[string]any) error {
	resp["type"] = "extension_ui_response"
	resp["id"] = id
	err := a.send(resp)
	a.mu.Lock()
	a.closeDialogLocked(id)
	a.mu.Unlock()
	return err
}

func (a *Agent) closeDialogLocked(id string) {
	for i, d := range a.dialogs {
		if d.ID == id {
			a.dialogs = append(a.dialogs[:i], a.dialogs[i+1:]...)
			a.emitLocked(UIEvent{T: "dialog_close", ID: id})
			return
		}
	}
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
		Model: a.model, Thinking: a.thinking, Exited: a.exited, ExitErr: a.exitErr,
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
		ID: a.ID, Epoch: a.Epoch, Seq: a.seq, Name: a.name, File: a.file, Cwd: a.Cwd,
		Status:   a.statusLocked(),
		Dialogs:  append([]dialogView(nil), a.dialogs...),
		LastText: lastAssistantText(a.messages),
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
		cp := liveMsg{Blocks: append([]BlockView(nil), a.live.Blocks...)}
		s.Live = &cp
	}
	return s
}

// Info describes the agent for the session list.
func (a *Agent) Info() (info SessionInfo, streaming bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	info = SessionInfo{ID: a.ID, File: a.file, Cwd: a.Cwd, Name: a.name, Modified: time.Now()}
	for _, m := range a.messages {
		if m.Role == "user" {
			info.Prompts++
			if info.FirstMsg == "" {
				info.FirstMsg = oneLine(m.PlainText(), 140)
			}
		}
	}
	return info, a.streaming
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
