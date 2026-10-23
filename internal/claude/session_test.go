package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"testing"

	"pircd/internal/engine"
)

// recSink records the sink calls a test cares about; others panic.
type recSink struct {
	engine.Sink
	calls []string
}

func (r *recSink) RunStarted()       { r.calls = append(r.calls, "run_start") }
func (r *recSink) RunSettled()       { r.calls = append(r.calls, "run_end") }
func (r *recSink) AssistantStarted() { r.calls = append(r.calls, "assistant_start") }
func (r *recSink) BlockDelta(i int, s string) {
	r.calls = append(r.calls, fmt.Sprintf("delta %d %s", i, s))
}
func (r *recSink) BlockStarted(i int, k engine.BlockKind, tool string) {
	r.calls = append(r.calls, fmt.Sprintf("block %d %s %s", i, k, tool))
}
func (r *recSink) MessageEnded(m engine.Message) {
	r.calls = append(r.calls, "message "+m.Role+" "+m.PlainText()+m.ToolCallID)
}
func (r *recSink) ToolStarted(id, name string, args json.RawMessage) {
	r.calls = append(r.calls, "tool_start "+id+" "+name+" "+string(args))
}
func (r *recSink) DialogOpened(d engine.Dialog) {
	r.calls = append(r.calls, fmt.Sprintf("dialog %s %s %v", d.ID, d.Title, d.Options))
}

func TestHandleLineEvents(t *testing.T) {
	sink := &recSink{}
	s := &Session{sink: sink, pending: map[string]chan response{}, done: make(chan struct{})}
	for _, line := range []string{
		`{"type":"event","ev":"run_start"}`,
		`{"type":"event","ev":"assistant_start"}`,
		`{"type":"event","ev":"block_start","idx":0,"kind":"text","toolName":""}`,
		`{"type":"event","ev":"block_delta","idx":0,"text":"hi there"}`,
		`{"type":"event","ev":"message","message":{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"toolCall","id":"t1","name":"Bash","arguments":{"command":"ls"}}]}}`,
		`{"type":"event","ev":"tool_start","callId":"t1","name":"Bash","args":{"command":"ls"}}`,
		`{"type":"event","ev":"dialog","dialog":{"id":"d1","method":"select","title":"Allow Bash?","options":["Allow","Deny"]}}`,
		`{"type":"event","ev":"message","message":{"role":"toolResult","toolCallId":"t1","content":[{"type":"text","text":"x"}]}}`,
		`{"type":"event","ev":"run_end"}`,
	} {
		s.handleLine([]byte(line))
	}
	want := []string{
		"run_start",
		"assistant_start",
		"block 0 text ",
		"delta 0 hi there",
		"message assistant hi",
		`tool_start t1 Bash {"command":"ls"}`,
		"dialog d1 Allow Bash? [Allow Deny]",
		"message toolResult xt1",
		"run_end",
	}
	if !slices.Equal(sink.calls, want) {
		t.Fatalf("calls:\n%q\nwant:\n%q", sink.calls, want)
	}
}

type pipeWriter struct{ lines chan []byte }

func (w pipeWriter) Write(b []byte) (int, error) {
	w.lines <- append([]byte(nil), b...)
	return len(b), nil
}
func (w pipeWriter) Close() error { return nil }

var _ io.WriteCloser = pipeWriter{}

func TestCallMatchesResponse(t *testing.T) {
	w := pipeWriter{lines: make(chan []byte, 1)}
	s := &Session{sink: &recSink{}, stdin: w, pending: map[string]chan response{}, done: make(chan struct{})}
	go func() {
		var cmd struct{ ID, Type string }
		_ = json.Unmarshal(<-w.lines, &cmd)
		s.handleLine([]byte(`{"type":"response","id":"` + cmd.ID + `","ok":true,"data":["low","high"]}`))
	}()
	levels, err := s.ThinkingLevels(context.Background())
	if err != nil || !slices.Equal(levels, []string{"low", "high"}) {
		t.Fatalf("levels = %v, %v", levels, err)
	}

	// The command's own id must not clobber the model id.
	sent := make(chan string, 1)
	go func() {
		var cmd struct{ ID, Model string }
		_ = json.Unmarshal(<-w.lines, &cmd)
		sent <- cmd.Model
		s.handleLine([]byte(`{"type":"response","id":"` + cmd.ID + `","ok":false,"error":"nope"}`))
	}()
	if err := s.SetModel(context.Background(), "anthropic", "opus"); err == nil || err.Error() != "nope" {
		t.Fatalf("SetModel error = %v, want nope", err)
	}
	if m := <-sent; m != "opus" {
		t.Fatalf("set_model sent model %q, want opus", m)
	}
}
