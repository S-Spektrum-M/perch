package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This exercises real templates, session.js, SSE, and browser DOM behavior.
// CI without Chrome still runs the backend and lightweight JS handler tests.
func TestToolStreamBrowser(t *testing.T) {
	var chrome string
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser"} {
		if path, err := exec.LookPath(name); err == nil {
			chrome = path
			break
		}
	}
	if chrome == "" {
		t.Skip("Chrome/Chromium is not installed")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is not installed")
	}
	m := savedShellResult(t, "")
	templates := loadTemplates()
	a := &Agent{ID: "browser", Epoch: "test", Cwd: t.TempDir(), render: templates, done: make(chan struct{}), subs: map[chan struct{}]struct{}{}}
	s := &Server{store: NewStore(t.TempDir()), mgr: &Manager{agents: map[string]*Agent{a.ID: a}}, tmpl: templates}
	routes := s.routes()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		phase, fixture := strings.CutPrefix(r.URL.Path, "/fixture/")
		if !fixture {
			routes.ServeHTTP(w, r)
			return
		}
		switch phase {
		case "working":
			a.handleEvent("agent_start", nil)
		case "responding":
			a.handleEvent("message_start", []byte(`{"message":{"role":"assistant"}}`))
		case "idle":
			a.handleEvent("agent_settled", nil)
		case "compacting":
			a.handleEvent("compaction_start", nil)
		case "compacted":
			// No pi RPC process to reload messages from in this fixture.
			a.mu.Lock()
			a.compacting, a.activity = false, ""
			a.emitStatusLocked()
			a.mu.Unlock()
		case "exit":
			a.mu.Lock()
			a.exited, a.exitErr = true, "pi exited (test)"
			a.emitStatusLocked()
			a.mu.Unlock()
		case "start":
			toolEvent(t, a, "tool_execution_start", m.ToolCallID, "")
		case "finish":
			sendToolMessage(t, a, "message_end", m)
		default:
			n, err := strconv.Atoi(phase)
			if err != nil || n < 0 || n > 506 {
				http.Error(w, "invalid fixture phase", 400)
				return
			}
			if err := os.WriteFile(m.Details.FullOutputPath, []byte(verboseToolLines(1, n)), 0600); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			sendToolMessage(t, a, "tool_execution_update", m)
		}
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "tests/tool-stream-browser.cjs", chrome, server.URL)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser test failed: %v\n%s", err, output)
	}
	t.Log(strings.TrimSpace(string(output)))
}
