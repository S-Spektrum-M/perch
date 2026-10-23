package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"perchd/internal/engine"
)

type rpcResponse struct {
	Success bool            `json:"success"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`
}

// RPC is one `pi --mode rpc` child process bound to one session. It
// translates pi's events (docs/json.md, docs/rpc-extension-ui.md) for the
// sink and perchd's commands into pi's (docs/rpc-commands.md).
type RPC struct {
	sink  engine.Sink
	cmd   *exec.Cmd // nil for connections made by NewRPC
	stdin io.WriteCloser
	wmu   sync.Mutex

	pmu     sync.Mutex
	pending map[string]chan rpcResponse
	nextID  atomic.Uint64

	done     chan struct{} // closed when the process has exited
	exitOnce sync.Once

	smu    sync.Mutex
	stderr []byte // tail of stderr, for diagnostics
}

var _ engine.Engine = (*RPC)(nil)

// NewRPC wraps a connection to pi whose stdout records are passed to
// HandleLine, e.g. a recording or a test fixture.
func NewRPC(stdin io.WriteCloser, sink engine.Sink) *RPC {
	return &RPC{sink: sink, stdin: stdin, pending: map[string]chan rpcResponse{}, done: make(chan struct{})}
}

// startRPC launches pi in RPC mode in dir.
func startRPC(bin string, args []string, dir string, sink engine.Sink) (*RPC, error) {
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

	p := NewRPC(stdin, sink)
	p.cmd = cmd
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); p.readLoop(stdout) }()
	go func() { defer readers.Done(); p.stderrLoop(stderr) }()
	go func() {
		readers.Wait()
		p.onExit(cmd.Wait())
	}()
	return p, nil
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// RPC plumbing

func (p *RPC) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

// call sends a command and waits for its response.
func (p *RPC) call(ctx context.Context, cmd map[string]any) (json.RawMessage, error) {
	id := fmt.Sprintf("perchd-%d", p.nextID.Add(1))
	cmd["id"] = id
	ch := make(chan rpcResponse, 1)
	p.pmu.Lock()
	p.pending[id] = ch
	p.pmu.Unlock()
	defer func() {
		p.pmu.Lock()
		delete(p.pending, id)
		p.pmu.Unlock()
	}()

	if err := p.send(cmd); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if !r.Success {
			return nil, errors.New(cmp(r.Error, "command failed"))
		}
		return r.Data, nil
	case <-p.done:
		return nil, errors.New("pi process exited")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// callInto sends a command and decodes its response data into dst, if set.
func (p *RPC) callInto(ctx context.Context, cmd map[string]any, dst any) error {
	raw, err := p.call(ctx, cmd)
	if err != nil || dst == nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}

// readLoop splits stdout on LF only (JSON strings may contain U+2028).
func (p *RPC) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := br.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			p.HandleLine(line)
		}
		if err != nil {
			return
		}
	}
}

func (p *RPC) stderrLoop(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			p.smu.Lock()
			p.stderr = append(p.stderr, buf[:n]...)
			if len(p.stderr) > 4096 {
				p.stderr = p.stderr[len(p.stderr)-4096:]
			}
			p.smu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// HandleLine processes one record from pi's stdout.
func (p *RPC) HandleLine(line []byte) {
	var env struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		rpcResponse
	}
	if err := json.Unmarshal(line, &env); err != nil {
		log.Printf("pi: bad record: %v", err)
		return
	}
	switch env.Type {
	case "response":
		p.pmu.Lock()
		ch := p.pending[env.ID]
		p.pmu.Unlock()
		if ch != nil {
			ch <- env.rpcResponse
		}
	case "extension_ui_request":
		p.handleUIRequest(line)
	default:
		p.handleEvent(env.Type, line)
	}
}

func (p *RPC) onExit(err error) {
	p.exitOnce.Do(func() { p.exit(err) })
}

func (p *RPC) exit(err error) {
	reason := "pi exited"
	if err != nil {
		reason += ": " + err.Error()
	}
	p.smu.Lock()
	if tail := strings.TrimSpace(string(p.stderr)); tail != "" {
		reason += "\n" + tail
	}
	p.smu.Unlock()
	p.sink.Stopped(reason)
	close(p.done)
}

// Close asks pi to shut down (by closing stdin) and kills it if it does not.
func (p *RPC) Close() {
	_ = p.stdin.Close()
	if p.cmd == nil {
		p.onExit(nil)
		return
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	}
}

// ---------------------------------------------------------------------------
// Session events

// handleEvent translates one session event for the sink.
// See pi's docs/json.md for the event shapes.
func (p *RPC) handleEvent(typ string, line []byte) {
	switch typ {
	case "agent_start":
		p.sink.RunStarted()

	case "agent_settled":
		p.sink.RunSettled()

	case "message_start":
		var e struct {
			Message engine.Message `json:"message"`
		}
		if json.Unmarshal(line, &e) == nil && e.Message.Role == "assistant" {
			p.sink.AssistantStarted()
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
		idx := e.E.ContentIndex
		switch e.E.Type {
		case "text_start":
			p.sink.BlockStarted(idx, engine.BlockText, "")
		case "thinking_start":
			p.sink.BlockStarted(idx, engine.BlockThinking, "")
		case "toolcall_start":
			p.sink.BlockStarted(idx, engine.BlockTool, e.E.ToolName)
		case "text_delta", "thinking_delta":
			p.sink.BlockDelta(idx, e.E.Delta)
		}

	case "message_end":
		var e struct {
			Message engine.Message `json:"message"`
		}
		if json.Unmarshal(line, &e) == nil {
			p.sink.MessageEnded(e.Message)
		}

	case "tool_execution_start", "tool_execution_update", "tool_execution_end":
		var e struct {
			ToolCallID       string          `json:"toolCallId"`
			ParentToolCallID string          `json:"parentToolCallId"`
			ToolName         string          `json:"toolName"`
			Args             json.RawMessage `json:"args"`
			PartialResult    engine.Message  `json:"partialResult"`
		}
		if json.Unmarshal(line, &e) != nil || e.ParentToolCallID != "" {
			return // nested tools are represented by their parent's result
		}
		switch typ {
		case "tool_execution_start":
			p.sink.ToolStarted(e.ToolCallID, e.ToolName, e.Args)
		case "tool_execution_update":
			e.PartialResult.ToolName = e.ToolName // partial results don't carry it
			p.sink.ToolUpdated(e.ToolCallID, e.ToolName, e.Args, e.PartialResult)
		case "tool_execution_end":
			p.sink.ToolEnded(e.ToolCallID)
		}

	case "auto_retry_end", "summarization_retry_finished":
		p.sink.RetryEnded()

	case "compaction_start":
		p.sink.CompactionStarted()

	case "compaction_end":
		p.sink.CompactionEnded()
		go p.reloadMessages()

	case "auto_retry_start", "summarization_retry_scheduled":
		var e struct {
			Attempt      int    `json:"attempt"`
			MaxAttempts  int    `json:"maxAttempts"`
			ErrorMessage string `json:"errorMessage"`
		}
		if json.Unmarshal(line, &e) == nil {
			p.sink.Retrying(e.Attempt, e.MaxAttempts, e.ErrorMessage)
		}

	case "queue_update":
		var e struct {
			Steering []string `json:"steering"`
			FollowUp []string `json:"followUp"`
		}
		if json.Unmarshal(line, &e) == nil {
			p.sink.QueueChanged(len(e.Steering) + len(e.FollowUp))
		}

	case "session_info_changed":
		var e struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(line, &e) == nil {
			p.sink.NameChanged(e.Name)
		}

	case "thinking_level_changed":
		var e struct {
			Level string `json:"level"`
		}
		if json.Unmarshal(line, &e) == nil {
			p.sink.ThinkingChanged(e.Level)
		}

	case "extension_error":
		var e struct {
			ExtensionPath string `json:"extensionPath"`
			Error         string `json:"error"`
		}
		if json.Unmarshal(line, &e) == nil {
			p.sink.Notice("error", "extension error ("+e.ExtensionPath+"): "+e.Error)
		}
	}
}

// reloadMessages refetches the transcript after compaction rewrites it.
func (p *RPC) reloadMessages() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msgs, err := p.Messages(ctx)
	if err != nil {
		log.Printf("pi: reload after compaction: %v", err)
		return
	}
	p.sink.TranscriptReplaced(msgs)
}

// ---------------------------------------------------------------------------
// Extension UI (docs/rpc-extension-ui.md)

func (p *RPC) handleUIRequest(line []byte) {
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

	switch r.Method {
	case "select", "confirm", "input", "editor":
		p.sink.DialogOpened(engine.Dialog{
			ID: r.ID, Method: r.Method, Title: r.Title, Message: r.Message,
			Placeholder: r.Placeholder, Prefill: r.Prefill, Options: r.Options,
		})
		if r.Timeout > 0 {
			// pi resolves timed-out dialogs itself; just drop ours.
			time.AfterFunc(time.Duration(r.Timeout)*time.Millisecond, func() {
				p.sink.DialogClosed(r.ID)
			})
		}

	case "notify":
		p.sink.Notice(cmp(r.NotifyType, "info"), r.Message)

	case "setStatus":
		text := ""
		if r.StatusText != nil {
			text = *r.StatusText
		}
		p.sink.StatusChanged(r.StatusKey, text)

	case "setWidget":
		var lines []string
		if r.WidgetLines != nil {
			lines = *r.WidgetLines
			if lines == nil {
				lines = []string{}
			}
		}
		p.sink.WidgetChanged(r.WidgetKey, lines)

	case "set_editor_text":
		p.sink.EditorText(r.Text)
	}
}

// RespondUI answers an extension dialog.
func (p *RPC) RespondUI(id string, resp engine.UIResponse) error {
	msg := map[string]any{"type": "extension_ui_response", "id": id}
	switch {
	case resp.Cancelled:
		msg["cancelled"] = true
	case resp.Confirmed != nil:
		msg["confirmed"] = *resp.Confirmed
	default:
		msg["value"] = resp.Value
	}
	return p.send(msg)
}

// ---------------------------------------------------------------------------
// Commands

func (p *RPC) State(ctx context.Context) (engine.State, error) {
	var st struct {
		SessionFile   string        `json:"sessionFile"`
		SessionID     string        `json:"sessionId"`
		SessionName   string        `json:"sessionName"`
		ThinkingLevel string        `json:"thinkingLevel"`
		Model         *engine.Model `json:"model"`
	}
	if err := p.callInto(ctx, map[string]any{"type": "get_state"}, &st); err != nil {
		return engine.State{}, fmt.Errorf("get_state: %w", err)
	}
	return engine.State{
		SessionID: st.SessionID, SessionFile: st.SessionFile, SessionName: st.SessionName,
		Thinking: st.ThinkingLevel, Model: st.Model,
	}, nil
}

func (p *RPC) Messages(ctx context.Context) ([]engine.Message, error) {
	var data struct {
		Messages []engine.Message `json:"messages"`
	}
	if err := p.callInto(ctx, map[string]any{"type": "get_messages"}, &data); err != nil {
		return nil, fmt.Errorf("get_messages: %w", err)
	}
	return data.Messages, nil
}

func (p *RPC) Prompt(ctx context.Context, text string, mode engine.PromptMode) error {
	cmd := map[string]any{"type": "prompt", "message": text}
	if mode != engine.PromptNow {
		cmd["streamingBehavior"] = string(mode)
	}
	_, err := p.call(ctx, cmd)
	return err
}

// Abort works like Esc in the TUI: queued messages are dropped first so the
// abort doesn't just start on them.
func (p *RPC) Abort(ctx context.Context) ([]string, error) {
	var queued struct {
		Steering []string `json:"steering"`
		FollowUp []string `json:"followUp"`
	}
	_ = p.callInto(ctx, map[string]any{"type": "clear_queue"}, &queued)
	if _, err := p.call(ctx, map[string]any{"type": "abort"}); err != nil {
		return nil, err
	}
	return append(queued.Steering, queued.FollowUp...), nil
}

func (p *RPC) Shell(ctx context.Context, command string, exclude bool) (engine.ShellResult, error) {
	var res struct {
		Output    string `json:"output"`
		ExitCode  *int   `json:"exitCode"`
		Cancelled bool   `json:"cancelled"`
	}
	err := p.callInto(ctx, map[string]any{"type": "bash", "command": command, "excludeFromContext": exclude}, &res)
	return engine.ShellResult{Output: res.Output, ExitCode: res.ExitCode, Cancelled: res.Cancelled}, err
}

func (p *RPC) AbortShell(ctx context.Context) error {
	_, err := p.call(ctx, map[string]any{"type": "abort_bash"})
	return err
}

func (p *RPC) Compact(ctx context.Context, instructions string) error {
	cmd := map[string]any{"type": "compact"}
	if instructions != "" {
		cmd["customInstructions"] = instructions
	}
	_, err := p.call(ctx, cmd)
	return err
}

func (p *RPC) SetName(ctx context.Context, name string) error {
	_, err := p.call(ctx, map[string]any{"type": "set_session_name", "name": name})
	return err
}

func (p *RPC) Commands(ctx context.Context) ([]engine.Command, error) {
	var res struct {
		Commands []engine.Command `json:"commands"`
	}
	err := p.callInto(ctx, map[string]any{"type": "get_commands"}, &res)
	return res.Commands, err
}

func (p *RPC) Stats(ctx context.Context) (engine.Stats, error) {
	var st struct {
		UserMessages      int `json:"userMessages"`
		AssistantMessages int `json:"assistantMessages"`
		ToolCalls         int `json:"toolCalls"`
		Tokens            struct {
			Input, Output, CacheRead, CacheWrite, Total int64
		} `json:"tokens"`
		Cost         float64 `json:"cost"`
		ContextUsage *struct {
			Tokens        *int64   `json:"tokens"`
			ContextWindow int64    `json:"contextWindow"`
			Percent       *float64 `json:"percent"`
		} `json:"contextUsage"`
	}
	if err := p.callInto(ctx, map[string]any{"type": "get_session_stats"}, &st); err != nil {
		return engine.Stats{}, err
	}
	s := engine.Stats{
		UserMessages: st.UserMessages, AssistantMessages: st.AssistantMessages, ToolCalls: st.ToolCalls,
		Input: st.Tokens.Input, Output: st.Tokens.Output, CacheRead: st.Tokens.CacheRead,
		CacheWrite: st.Tokens.CacheWrite, Total: st.Tokens.Total, Cost: st.Cost,
	}
	if c := st.ContextUsage; c != nil && c.Tokens != nil && c.Percent != nil {
		s.Context = &engine.ContextUsage{Tokens: *c.Tokens, Window: c.ContextWindow, Percent: *c.Percent}
	}
	return s, nil
}

func (p *RPC) Models(ctx context.Context) ([]engine.Model, error) {
	var res struct {
		Models []engine.Model `json:"models"`
	}
	err := p.callInto(ctx, map[string]any{"type": "get_available_models"}, &res)
	return res.Models, err
}

func (p *RPC) SetModel(ctx context.Context, provider, id string) error {
	_, err := p.call(ctx, map[string]any{"type": "set_model", "provider": provider, "modelId": id})
	return err
}

func (p *RPC) ThinkingLevels(ctx context.Context) ([]string, error) {
	var res struct {
		Levels []string `json:"levels"`
	}
	err := p.callInto(ctx, map[string]any{"type": "get_available_thinking_levels"}, &res)
	return res.Levels, err
}

func (p *RPC) SetThinking(ctx context.Context, level string) error {
	_, err := p.call(ctx, map[string]any{"type": "set_thinking_level", "level": level})
	return err
}
