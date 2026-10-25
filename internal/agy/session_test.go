package agy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"perchd/internal/engine"
)

type recSink struct {
	engine.Sink
	calls []string
}

func (r *recSink) RunStarted()       { r.calls = append(r.calls, "run_start") }
func (r *recSink) RunSettled()       { r.calls = append(r.calls, "run_end") }
func (r *recSink) AssistantStarted() { r.calls = append(r.calls, "assistant_start") }
func (r *recSink) BlockStarted(i int, k engine.BlockKind, tool string) {
	r.calls = append(r.calls, fmt.Sprintf("block %d %s %s", i, k, tool))
}
func (r *recSink) BlockDelta(i int, s string) {
	r.calls = append(r.calls, fmt.Sprintf("delta %d %s", i, s))
}
func (r *recSink) MessageEnded(m engine.Message) {
	r.calls = append(r.calls, "message "+m.Role+" "+m.PlainText()+m.ToolCallID)
}
func (r *recSink) ToolStarted(id, name string, args json.RawMessage) {
	r.calls = append(r.calls, "tool_start "+id+" "+name+" "+string(args))
}
func (r *recSink) ToolEnded(id string) {
	r.calls = append(r.calls, "tool_end "+id)
}
func (r *recSink) DialogOpened(d engine.Dialog) {
	r.calls = append(r.calls, fmt.Sprintf("dialog %s %s %v", d.ID, d.Title, d.Options))
}
func (r *recSink) ThinkingChanged(level string) {
	r.calls = append(r.calls, "thinking "+level)
}
func (r *recSink) NameChanged(name string) {
	r.calls = append(r.calls, "name "+name)
}
func (r *recSink) QueueChanged(n int) {
	r.calls = append(r.calls, fmt.Sprintf("queue %d", n))
}
func (r *recSink) Stopped(reason string) {
	r.calls = append(r.calls, "stopped "+reason)
}

type pipeWriter struct{ lines chan []byte }

func (w pipeWriter) Write(b []byte) (int, error) {
	w.lines <- append([]byte(nil), b...)
	return len(b), nil
}
func (w pipeWriter) Close() error { return nil }

var _ io.WriteCloser = pipeWriter{}

func TestModelCatalogSplittingAndResolution(t *testing.T) {
	raw := []acpModelInfo{
		{ModelID: "gemini-4-argon-medium", Name: "Gemini 4 Argon (Medium)"},
		{ModelID: "gemini-4-argon-high", Name: "Gemini 4 Argon (High)"},
		{ModelID: "gemini-4-argon-low", Name: "Gemini 4 Argon (Low)"},
		{ModelID: "claude-opus-5-5-low", Name: "Claude Opus 5.5 (Low)"},
	}
	cat := parseModelCatalog(raw)

	wantModels := []engine.Model{
		{ID: "gemini-4-argon", Name: "Gemini 4 Argon", Provider: Provider},
		{ID: "claude-opus-5-5", Name: "Claude Opus 5.5", Provider: Provider},
	}
	if !slices.Equal(cat.models, wantModels) {
		t.Fatalf("models = %+v, want %+v", cat.models, wantModels)
	}
	if got := cat.efforts["gemini-4-argon"]; !slices.Equal(got, []string{"low", "medium", "high"}) {
		t.Fatalf("efforts[gemini-4-argon] = %v, want [low medium high]", got)
	}
	if got := cat.efforts["claude-opus-5-5"]; !slices.Equal(got, []string{"low"}) {
		t.Fatalf("efforts[claude-opus-5-5] = %v, want [low]", got)
	}

	// Resolving base model + effort
	if full, ok := cat.resolveFullID("gemini-4-argon", "high"); !ok || full != "gemini-4-argon-high" {
		t.Fatalf("resolveFullID(gemini-4-argon, high) = %q, %v", full, ok)
	}
	// Resolving when requested effort is unsupported falls back to model's first/default variant
	if full, ok := cat.resolveFullID("claude-opus-5-5", "high"); !ok || full != "claude-opus-5-5-low" {
		t.Fatalf("resolveFullID(claude-opus-5-5, high) = %q, %v", full, ok)
	}

	m, effort := cat.stateModel("gemini-4-argon-medium")
	if m == nil || m.ID != "gemini-4-argon" || m.Name != "Gemini 4 Argon" || effort != "medium" {
		t.Fatalf("stateModel = %+v, %q", m, effort)
	}
}

func TestReplayHistoryAssembly(t *testing.T) {
	sink := &recSink{}
	s := newSession(sessionConfig{Mode: "open"}, sink, nil)
	s.replaying = true
	s.currentModelID = "gemini-4-argon-high"

	lines := []string{
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"<t3_code_instructions>\nblah\n</t3_code_instructions>\n\n<user_request>\ncan you run the build?\n</user_request> <runtime_info>xyz</runtime_info>"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"Thinking about build..."}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"tc-1","title":"go build ./...","kind":"execute","status":"completed","rawInput":"{\"CommandLine\":\"go build ./...\"}"}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"tc-1","status":"completed","rawOutput":{"commandLine":"go build ./...","combinedOutput":"ok\n","exitCode":0}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"The build succeeded."}}}}`,
	}
	for _, line := range lines {
		s.handleLine([]byte(line))
	}
	s.mu.Lock()
	s.finishReplayLocked()
	s.mu.Unlock()

	if len(sink.calls) != 0 {
		t.Fatalf("replay emitted live sink calls: %v", sink.calls)
	}

	msgs, err := s.Messages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("len(msgs) = %d, want 4: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].PlainText() != "can you run the build?" {
		t.Fatalf("msgs[0] = %s %q", msgs[0].Role, msgs[0].PlainText())
	}
	if msgs[1].Role != "assistant" {
		t.Fatalf("msgs[1].Role = %s, want assistant", msgs[1].Role)
	}
	blocks := msgs[1].Blocks()
	if len(blocks) != 2 || blocks[0].Type != "thinking" || blocks[1].Type != "toolCall" || blocks[1].Name != "bash" {
		t.Fatalf("msgs[1] blocks = %+v", blocks)
	}
	if msgs[2].Role != "toolResult" || msgs[2].ToolCallID != "tc-1" || strings.TrimSpace(msgs[2].PlainText()) != "ok" {
		t.Fatalf("msgs[2] = %+v, text=%q", msgs[2], msgs[2].PlainText())
	}
	if msgs[3].Role != "assistant" || msgs[3].PlainText() != "The build succeeded." {
		t.Fatalf("msgs[3] = %+v", msgs[3])
	}
}

func TestLiveStreamingAndPermissionDialog(t *testing.T) {
	w := pipeWriter{lines: make(chan []byte, 4)}
	sink := &recSink{}
	s := newSession(sessionConfig{Mode: "create"}, sink, w)
	s.sessionID = "s1"

	for _, line := range []string{
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"plan"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi\u2028there"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"ls -la","kind":"execute","status":"pending","rawInput":{"CommandLine":"ls -la"}}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{"sessionId":"s1","options":[{"optionId":"allow","name":"Allow","kind":"allow_once"},{"optionId":"deny","name":"Deny","kind":"reject_once"}],"toolCall":{"toolCallId":"t1","title":"ls -la","kind":"execute","rawInput":{"CommandLine":"ls -la"}}}}`,
	} {
		s.handleLine([]byte(line))
	}

	if err := s.RespondUI("t1", engine.UIResponse{Value: "Allow"}); err != nil {
		t.Fatalf("RespondUI: %v", err)
	}
	respLine := <-w.lines
	var rpcResp struct {
		ID     int `json:"id"`
		Result struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respLine, &rpcResp); err != nil {
		t.Fatalf("unmarshal permission response: %v", err)
	}
	if rpcResp.ID != 7 || rpcResp.Result.Outcome.Outcome != "selected" || rpcResp.Result.Outcome.OptionID != "allow" {
		t.Fatalf("permission response = %+v", rpcResp)
	}

	// Reconciled tool execution progresses to in_progress and then completed.
	for _, line := range []string{
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","title":"ls -la","kind":"execute","status":"in_progress","rawInput":{"CommandLine":"ls -la"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed","rawOutput":{"combinedOutput":"file.txt"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"done"}}}}`,
	} {
		s.handleLine([]byte(line))
	}
	s.flushLiveAssistant("end_turn")

	want := []string{
		"assistant_start",
		"block 0 thinking ",
		"delta 0 plan",
		"block 1 text ",
		"delta 1 hi\u2028there",
		"dialog t1 ls -la [Allow Deny]",
		"message assistant hi\u2028there",
		`tool_start t1 bash {"CommandLine":"ls -la","command":"ls -la"}`,
		"tool_end t1",
		"message toolResult file.txtt1",
		"assistant_start",
		"block 0 text ",
		"delta 0 done",
		"message assistant done",
	}
	if !slices.Equal(sink.calls, want) {
		t.Fatalf("calls:\n%q\nwant:\n%q", sink.calls, want)
	}
}

func TestSetModelAndThinkingCallsConfigOption(t *testing.T) {
	w := pipeWriter{lines: make(chan []byte, 2)}
	s := newSession(sessionConfig{Mode: "create"}, &recSink{}, w)
	s.sessionID = "sess-1"
	s.currentModelID = "gemini-4-argon-medium"
	s.catalog = parseModelCatalog([]acpModelInfo{
		{ModelID: "gemini-4-argon-medium", Name: "Gemini 4 Argon (Medium)"},
		{ModelID: "gemini-4-argon-high", Name: "Gemini 4 Argon (High)"},
		{ModelID: "gemini-3.8-flash-high", Name: "Gemini 3.8 Flash (High)"},
		{ModelID: "gemini-3.8-flash-low", Name: "Gemini 3.8 Flash (Low)"},
	})

	go func() {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params struct {
				ConfigID string `json:"configId"`
				Value    string `json:"value"`
			} `json:"params"`
		}
		_ = json.Unmarshal(<-w.lines, &req)
		if req.Method == "session/set_config_option" && req.Params.ConfigID == "model" && req.Params.Value == "gemini-4-argon-high" {
			s.handleLine([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"configOptions":[]}}`, req.ID)))
		}
	}()
	if err := s.SetThinking(context.Background(), "high"); err != nil {
		t.Fatalf("SetThinking: %v", err)
	}
	st, _ := s.State(context.Background())
	if st.Thinking != "high" || st.Model == nil || st.Model.ID != "gemini-4-argon" {
		t.Fatalf("State after SetThinking = %+v", st)
	}

	// Switch to gemini-3.8-flash (keeps "high" effort since gemini-3.8-flash-high exists).
	go func() {
		var req struct {
			ID     int `json:"id"`
			Params struct {
				Value string `json:"value"`
			} `json:"params"`
		}
		_ = json.Unmarshal(<-w.lines, &req)
		if req.Params.Value == "gemini-3.8-flash-high" {
			s.handleLine([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"configOptions":[]}}`, req.ID)))
		}
	}()
	if err := s.SetModel(context.Background(), Provider, "gemini-3.8-flash"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	st, _ = s.State(context.Background())
	if st.Model == nil || st.Model.ID != "gemini-3.8-flash" || st.Thinking != "high" {
		t.Fatalf("State after SetModel = %+v", st)
	}
}

func TestStoreListAndFork(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	id := "11111111-2222-3333-4444-555555555555"
	dbPath := filepath.Join(dir, id+".db")
	if err := os.WriteFile(dbPath, []byte("sqlite-header-"+id+"-<user_request>\nhello world\n</user_request>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateMeta(id, map[string]any{"cwd": "/tmp/proj", "name": "Test Session"}); err != nil {
		t.Fatal(err)
	}

	// Empty 0-byte DB should be ignored by List.
	if err := os.WriteFile(filepath.Join(dir, "empty.db"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	list := store.List()
	if len(list) != 1 {
		t.Fatalf("len(list) = %d, want 1", len(list))
	}
	if list[0].ID != id || list[0].Cwd != "/tmp/proj" || list[0].Name != "Test Session" || list[0].FirstMsg != "hello world" {
		t.Fatalf("list[0] = %+v", list[0])
	}

	forkID, err := store.Fork(id)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	forkData, err := os.ReadFile(filepath.Join(dir, forkID+".db"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(forkData), id) || !strings.Contains(string(forkData), forkID) {
		t.Fatalf("Fork did not replace embedded trajectory UUID: %q", string(forkData))
	}
}

// Opt-in tests requiring a locally installed and authenticated Antigravity backend.
// Run with PERCHD_AGY_TESTS=1 go test ./internal/agy -run TestLive.
func TestLiveBackendListAndOpen(t *testing.T) {
	if os.Getenv("PERCHD_AGY_TESTS") != "1" {
		t.Skip("set PERCHD_AGY_TESTS=1 to run Antigravity integration tests")
	}
	b := NewBackend(Config{})
	if b.cfg.Bin == "" {
		t.Skip("agy_acp_server.par not installed on this machine")
	}
	models, err := b.Models(context.Background())
	if err != nil || len(models) == 0 {
		t.Fatalf("Models: %v, %v", models, err)
	}

	sessions := b.List()
	if len(sessions) == 0 {
		t.Skip("no existing agy sessions on disk")
	}

	eng, err := b.Open(sessions[0], &recSink{})
	if err != nil {
		t.Fatalf("Open(%s): %v", sessions[0].ID, err)
	}
	defer eng.Close()

	st, err := eng.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.SessionID != sessions[0].ID || st.Model == nil {
		t.Fatalf("unexpected State: %+v", st)
	}
	msgs, err := eng.Messages(context.Background())
	if err != nil || len(msgs) == 0 {
		t.Fatalf("Messages: len=%d, err=%v", len(msgs), err)
	}
}

func TestCleanSuggestedName(t *testing.T) {
	got, err := cleanSuggestedName("  \"Fix OAuth token refresh\"  \nExtra line")
	if err != nil || got != "Fix OAuth token refresh" {
		t.Fatalf("cleanSuggestedName = %q, %v", got, err)
	}
	for _, bad := range []string{"", "   ", "bad\x1btitle"} {
		if name, err := cleanSuggestedName(bad); err == nil {
			t.Fatalf("accepted invalid %q -> %q", bad, name)
		}
	}
}

func TestLiveSuggestSessionName(t *testing.T) {
	if os.Getenv("PERCHD_AGY_TESTS") != "1" {
		t.Skip("set PERCHD_AGY_TESTS=1 to run Antigravity integration tests")
	}
	b := NewBackend(Config{})
	if b.cfg.Bin == "" {
		t.Skip("agy_acp_server.par not installed on this machine")
	}
	name, err := b.SuggestSessionName(context.Background(), t.TempDir(), "Can you add dark mode support to the settings page and persist the preference in localStorage?")
	if err != nil {
		t.Fatalf("SuggestSessionName: %v", err)
	}
	t.Logf("Suggested session name: %q", name)
	if name == "" || len(name) > 80 {
		t.Fatalf("unexpected name: %q", name)
	}
}
