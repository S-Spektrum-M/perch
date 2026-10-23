package claude

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

// bridgeConfig is bridge.mjs's command-line configuration.
type bridgeConfig struct {
	Mode           string `json:"mode"` // create, open, fork
	Cwd            string `json:"cwd"`
	SessionID      string `json:"sessionId,omitempty"`
	Name           string `json:"name,omitempty"`
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	PermissionMode string `json:"permissionMode,omitempty"`
	Auth           string `json:"auth,omitempty"`
}

type response struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error"`
	Data  json.RawMessage `json:"data"`
}

// Session is one bridge process bound to one Claude session.
type Session struct {
	sink  engine.Sink
	cmd   *exec.Cmd
	stdin io.WriteCloser
	wmu   sync.Mutex

	pmu     sync.Mutex
	pending map[string]chan response
	nextID  atomic.Uint64

	done     chan struct{}
	exitOnce sync.Once

	smu    sync.Mutex
	stderr []byte
}

var _ engine.Engine = (*Session)(nil)

func startSession(node, bridge string, c bridgeConfig, sink engine.Sink) (*Session, error) {
	arg, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(node, bridge, string(arg))
	cmd.Dir = c.Cwd
	// Own process group, so stopping the session also stops Claude Code and its tools.
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
		return nil, fmt.Errorf("start claude bridge: %w", err)
	}
	s := &Session{sink: sink, cmd: cmd, stdin: stdin, pending: map[string]chan response{}, done: make(chan struct{})}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); s.readLoop(stdout) }()
	go func() { defer readers.Done(); s.stderrLoop(stderr) }()
	go func() {
		readers.Wait()
		s.onExit(cmd.Wait())
	}()
	return s, nil
}

// ---------------------------------------------------------------------------
// Plumbing

func (s *Session) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err = s.stdin.Write(append(b, '\n'))
	return err
}

// call sends a command and decodes its response data into dst, if set.
func (s *Session) call(ctx context.Context, cmd map[string]any, dst any) error {
	id := fmt.Sprintf("perchd-%d", s.nextID.Add(1))
	cmd["id"] = id
	ch := make(chan response, 1)
	s.pmu.Lock()
	s.pending[id] = ch
	s.pmu.Unlock()
	defer func() {
		s.pmu.Lock()
		delete(s.pending, id)
		s.pmu.Unlock()
	}()
	if err := s.send(cmd); err != nil {
		return err
	}
	select {
	case r := <-ch:
		if !r.OK {
			return errors.New(cmp(r.Error, "command failed"))
		}
		if dst == nil || len(r.Data) == 0 {
			return nil
		}
		return json.Unmarshal(r.Data, dst)
	case <-s.done:
		return errors.New("claude bridge exited")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readLoop splits stdout on LF only (JSON strings may contain U+2028).
func (s *Session) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := br.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			s.handleLine(line)
		}
		if err != nil {
			return
		}
	}
}

func (s *Session) stderrLoop(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s.smu.Lock()
			s.stderr = append(s.stderr, buf[:n]...)
			if len(s.stderr) > 4096 {
				s.stderr = s.stderr[len(s.stderr)-4096:]
			}
			s.smu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (s *Session) onExit(err error) {
	s.exitOnce.Do(func() {
		reason := "claude bridge exited"
		if err != nil {
			reason += ": " + err.Error()
		}
		s.smu.Lock()
		if tail := strings.TrimSpace(string(s.stderr)); tail != "" {
			reason += "\n" + tail
		}
		s.smu.Unlock()
		s.sink.Stopped(reason)
		close(s.done)
	})
}

// Close closes the bridge's stdin and kills it if it does not exit.
func (s *Session) Close() {
	_ = s.stdin.Close()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-s.done
	}
}

// ---------------------------------------------------------------------------
// Events

// event is the union of the bridge's events; see bridge.mjs.
type event struct {
	Type     string           `json:"type"`
	ID       string           `json:"id"`
	Ev       string           `json:"ev"`
	Idx      int              `json:"idx"`
	Kind     string           `json:"kind"`
	ToolName string           `json:"toolName"`
	Text     string           `json:"text"`
	Message  engine.Message   `json:"message"`
	Messages []engine.Message `json:"messages"`
	CallID   string           `json:"callId"`
	Name     string           `json:"name"`
	Args     json.RawMessage  `json:"args"`
	Attempt  int              `json:"attempt"`
	Max      int              `json:"max"`
	Reason   string           `json:"reason"`
	N        int              `json:"n"`
	Level    string           `json:"level"`
	Key      string           `json:"key"`
	Dialog   struct {
		ID      string   `json:"id"`
		Method  string   `json:"method"`
		Title   string   `json:"title"`
		Message string   `json:"message"`
		Options []string `json:"options"`
	} `json:"dialog"`
}

func (s *Session) handleLine(line []byte) {
	if line[0] != '{' {
		log.Printf("claude bridge: %s", line)
		return
	}
	var e event
	var r response
	if err := json.Unmarshal(line, &e); err != nil {
		log.Printf("claude bridge: bad record: %v", err)
		return
	}
	if e.Type == "response" {
		_ = json.Unmarshal(line, &r)
		s.pmu.Lock()
		ch := s.pending[e.ID]
		s.pmu.Unlock()
		if ch != nil {
			ch <- r
		}
		return
	}
	switch e.Ev {
	case "run_start":
		s.sink.RunStarted()
	case "run_end":
		s.sink.RunSettled()
	case "assistant_start":
		s.sink.AssistantStarted()
	case "block_start":
		s.sink.BlockStarted(e.Idx, engine.BlockKind(e.Kind), e.ToolName)
	case "block_delta":
		s.sink.BlockDelta(e.Idx, e.Text)
	case "message":
		s.sink.MessageEnded(e.Message)
	case "tool_start":
		s.sink.ToolStarted(e.CallID, e.Name, e.Args)
	case "tool_end":
		s.sink.ToolEnded(e.CallID)
	case "retry":
		s.sink.Retrying(e.Attempt, e.Max, e.Reason)
	case "compaction_start":
		s.sink.CompactionStarted()
	case "compaction_end":
		s.sink.CompactionEnded()
	case "transcript":
		s.sink.TranscriptReplaced(e.Messages)
	case "queue":
		s.sink.QueueChanged(e.N)
	case "name":
		s.sink.NameChanged(e.Name)
	case "thinking":
		s.sink.ThinkingChanged(e.Level)
	case "notice":
		s.sink.Notice(e.Kind, e.Text)
	case "status":
		s.sink.StatusChanged(e.Key, e.Text)
	case "dialog":
		d := e.Dialog
		s.sink.DialogOpened(engine.Dialog{ID: d.ID, Method: d.Method, Title: d.Title, Message: d.Message, Options: d.Options})
	case "dialog_closed":
		s.sink.DialogClosed(e.ID)
	}
}

// ---------------------------------------------------------------------------
// Commands

func (s *Session) State(ctx context.Context) (engine.State, error) {
	var st engine.State
	var raw struct {
		SessionID   string        `json:"sessionId"`
		SessionFile string        `json:"sessionFile"`
		SessionName string        `json:"sessionName"`
		Thinking    string        `json:"thinking"`
		Model       *engine.Model `json:"model"`
	}
	if err := s.call(ctx, map[string]any{"type": "state"}, &raw); err != nil {
		return st, fmt.Errorf("state: %w", err)
	}
	return engine.State{SessionID: raw.SessionID, SessionFile: raw.SessionFile, SessionName: raw.SessionName, Thinking: raw.Thinking, Model: raw.Model}, nil
}

func (s *Session) Messages(ctx context.Context) ([]engine.Message, error) {
	var msgs []engine.Message
	err := s.call(ctx, map[string]any{"type": "messages"}, &msgs)
	return msgs, err
}

// Prompt sends a message. The SDK has one way to send during a run: the
// CLI folds the message into the current turn or runs it next, so both
// modes behave the same.
func (s *Session) Prompt(ctx context.Context, text string, mode engine.PromptMode) error {
	return s.call(ctx, map[string]any{"type": "prompt", "text": text}, nil)
}

func (s *Session) Abort(ctx context.Context) ([]string, error) {
	var queued []string
	err := s.call(ctx, map[string]any{"type": "abort"}, &queued)
	return queued, err
}

var errNoShell = errors.New("the claude backend has no direct shell; ask Claude to run the command")

func (s *Session) Shell(ctx context.Context, command string, exclude bool) (engine.ShellResult, error) {
	return engine.ShellResult{}, errNoShell
}

func (s *Session) AbortShell(ctx context.Context) error { return errNoShell }

func (s *Session) Compact(ctx context.Context, instructions string) error {
	return s.call(ctx, map[string]any{"type": "compact", "instructions": instructions}, nil)
}

func (s *Session) SetName(ctx context.Context, name string) error {
	return s.call(ctx, map[string]any{"type": "rename", "name": name}, nil)
}

func (s *Session) Commands(ctx context.Context) ([]engine.Command, error) {
	var cmds []engine.Command
	err := s.call(ctx, map[string]any{"type": "commands"}, &cmds)
	return cmds, err
}

func (s *Session) Stats(ctx context.Context) (engine.Stats, error) {
	var st struct {
		UserMessages, AssistantMessages, ToolCalls int
		Input, Output, CacheRead, CacheWrite       int64
		Cost                                       float64
		Account                                    string
	}
	if err := s.call(ctx, map[string]any{"type": "stats"}, &st); err != nil {
		return engine.Stats{}, err
	}
	return engine.Stats{
		UserMessages: st.UserMessages, AssistantMessages: st.AssistantMessages, ToolCalls: st.ToolCalls,
		Input: st.Input, Output: st.Output, CacheRead: st.CacheRead, CacheWrite: st.CacheWrite,
		Total: st.Input + st.Output + st.CacheRead + st.CacheWrite, Cost: st.Cost,
		Account: st.Account,
	}, nil
}

func (s *Session) Models(ctx context.Context) ([]engine.Model, error) {
	var models []engine.Model
	err := s.call(ctx, map[string]any{"type": "models"}, &models)
	return models, err
}

func (s *Session) SetModel(ctx context.Context, provider, id string) error {
	return s.call(ctx, map[string]any{"type": "set_model", "model": id}, nil)
}

func (s *Session) ThinkingLevels(ctx context.Context) ([]string, error) {
	var levels []string
	err := s.call(ctx, map[string]any{"type": "levels"}, &levels)
	return levels, err
}

func (s *Session) SetThinking(ctx context.Context, level string) error {
	return s.call(ctx, map[string]any{"type": "set_effort", "level": level}, nil)
}

func (s *Session) RespondUI(id string, resp engine.UIResponse) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.call(ctx, map[string]any{
		"type": "ui", "dialogId": id,
		"cancelled": resp.Cancelled, "confirmed": resp.Confirmed, "value": resp.Value,
	}, nil)
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
