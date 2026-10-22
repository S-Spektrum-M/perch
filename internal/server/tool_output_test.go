package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pircd/internal/pi"
)

func savedShellResult(t *testing.T, output string) pi.Message {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	f, err := os.CreateTemp(os.TempDir(), "pi-bash-*.log")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(output); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	content, _ := json.Marshal([]pi.Block{{Type: "text", Text: "upstream truncated tail"}})
	m := pi.Message{Role: "toolResult", ToolName: "bash", ToolCallID: "saved", Content: content}
	m.Details.FullOutputPath = f.Name()
	return m
}

func sendToolMessage(t *testing.T, a *Agent, typ string, m pi.Message) {
	t.Helper()
	var e any
	if typ == "message_end" {
		e = map[string]any{"type": typ, "message": m}
	} else {
		e = map[string]any{"type": typ, "toolCallId": m.ToolCallID, "toolName": m.ToolName, "partialResult": m}
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	a.handleLine(data)
}

func TestCompletedToolOutputNotTruncatedAt6000(t *testing.T) {
	output := numberedToolLines(1, 1500)
	content, _ := json.Marshal([]pi.Block{{Type: "text", Text: output}})
	for _, m := range []pi.Message{
		{Role: "toolResult", ToolName: "read", Content: content},
		{Role: "bashExecution", Output: output},
	} {
		v, ok := ViewMessage(m)
		if !ok || v.Body != output || v.Note != "" {
			t.Fatalf("%s output still truncated: len=%d, note=%q", m.Role, len(v.Body), v.Note)
		}
	}
}

// Model verbose compiler/explanation output that exceeds pi's 50 KiB limit.
func verboseToolLines(start, end int) string {
	return strings.ReplaceAll(numberedToolLines(start, end), "line ", strings.Repeat("[EXPLAIN] verbose build output ", 5)+"line ")
}

func TestSavedShellLogStreamsAndSurvivesCompletion(t *testing.T) {
	first := verboseToolLines(1, 300)
	m := savedShellResult(t, first)
	a := &Agent{ID: "output", render: loadTemplates()}
	sendToolMessage(t, a, "tool_execution_update", m)
	if got := toolDeltas(a); len(got) != 3 || a.tools[0].Text != first {
		t.Fatal("saved log was not streamed in 100-line chunks")
	}
	start := a.ring[0]
	if !strings.Contains(start.HTML, ` open>`) {
		t.Fatal("live output starts collapsed")
	}
	full := verboseToolLines(1, 506)
	if len(full) <= 50*1024 {
		t.Fatal("fixture must exceed pi's upstream output limit")
	}
	if err := os.WriteFile(m.Details.FullOutputPath, []byte(full), 0600); err != nil {
		t.Fatal(err)
	}
	sendToolMessage(t, a, "tool_execution_update", m)
	if a.tools[0].Text != verboseToolLines(1, 500) {
		t.Fatal("saved-log update lost output or published an incomplete chunk")
	}
	for _, ev := range a.ring {
		if ev.T == "tool_reset" {
			t.Fatal("upstream truncation reset full saved-log output")
		}
	}
	sendToolMessage(t, a, "message_end", m)
	last := a.ring[len(a.ring)-1]
	if !strings.Contains(last.HTML, full) || !strings.Contains(last.HTML, "Download full output") || strings.Contains(last.HTML, "upstream truncated tail") {
		t.Fatal("completion replaced the full log with truncated output")
	}
	// Reconstruct from persisted messages, without any live tool state.
	reloaded := &Agent{ID: a.ID, messages: []pi.Message{m}}
	snapshot := reloaded.Snapshot(0)
	if len(snapshot.Messages) != 1 || snapshot.Messages[0].Body != full || snapshot.Messages[0].OutputURL == "" {
		t.Fatal("full log lost after reload")
	}
}

func TestSavedShellLogPreviewAndDownload(t *testing.T) {
	full := strings.Repeat("full output\n", maxInlineToolOutput/12+100)
	m := savedShellResult(t, full)
	a := &Agent{ID: "output", done: make(chan struct{}), messages: []pi.Message{m}}
	v := a.Snapshot(0).Messages[0]
	if len(v.Body) > maxInlineToolOutput || !strings.Contains(v.Note, "4 MiB") || v.OutputURL == "" {
		t.Fatal("large log preview must be bounded and offer the complete log")
	}
	s := &Server{store: pi.NewStore(t.TempDir()), mgr: &Manager{agents: map[string]*Agent{a.ID: a}}, tmpl: loadTemplates()}
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, httptest.NewRequest("GET", "http://localhost"+v.OutputURL, nil))
	if w.Code != 200 || w.Body.String() != full || w.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatalf("full-output download failed: status=%d, bytes=%d", w.Code, w.Body.Len())
	}
	for _, path := range []string{"/s/output/tool-output/tool-unknown", "/s/another-session/tool-output/" + toolOutputID(m.ToolCallID)} {
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, httptest.NewRequest("GET", "http://localhost"+path, nil))
		if w.Code != 404 {
			t.Fatalf("unknown session/tool returned %d", w.Code)
		}
	}
}

func TestSavedShellLogRejectsUnrelatedFilesAndSymlinks(t *testing.T) {
	m := savedShellResult(t, "valid\n")
	valid := m.Details.FullOutputPath
	outside := filepath.Join(t.TempDir(), "private.txt")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(os.TempDir(), "pi-bash-symlink.log")
	if err := os.Symlink(outside, symlink); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, symlink, filepath.Join(os.TempDir(), "pi-bash-missing.log"), os.TempDir()} {
		m.Details.FullOutputPath = path
		if f, err := openShellOutput(m); err == nil {
			f.Close()
			t.Fatalf("opened ineligible path %q", path)
		}
	}
	m.Details.FullOutputPath = valid
	m.ToolName = "custom"
	if f, err := openShellOutput(m); err == nil {
		f.Close()
		t.Fatal("custom tool can expose a saved file")
	}
}

func TestSavedShellLogFallbackAndFailureStatus(t *testing.T) {
	m := savedShellResult(t, "shell output\n")
	m.Content = json.RawMessage(`[{"type":"text","text":"tail\n\nCommand exited with code 1"}]`)
	m.IsError = true
	v, _ := ViewMessage(m)
	if !strings.Contains(v.Body, "shell output") || !strings.Contains(v.Body, "Command exited with code 1") {
		t.Fatal("saved log obscures final failure status")
	}
	if err := os.Remove(m.Details.FullOutputPath); err != nil {
		t.Fatal(err)
	}
	v, _ = ViewMessage(m)
	if v.Body != toolResultText(m) || !strings.Contains(v.Note, "unavailable") {
		t.Fatal("missing saved log should fall back to the original result")
	}
}

func TestToolOutputStripsTerminalEscapes(t *testing.T) {
	input := "\x1b[35mDEBUG\x1b[0m \x1b]0;window title\x07text\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\\n"
	if got := cleanToolOutput(input); got != "DEBUG textlink\n" {
		t.Fatalf("terminal escape cleanup: %q", got)
	}
}
