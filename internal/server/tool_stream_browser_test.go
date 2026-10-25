package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"perchd/internal/pi"
)

// This exercises real templates, session.js, SSE, and browser DOM behavior.
// Opt in with PERCHD_BROWSER_TESTS=1; CI still runs backend and lightweight JS tests.
func TestToolStreamBrowser(t *testing.T) {
	if os.Getenv("PERCHD_BROWSER_TESTS") != "1" {
		t.Skip("set PERCHD_BROWSER_TESTS=1 to run Chrome integration test")
	}
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
	a := &Agent{ID: "browser", Epoch: "test", Cwd: t.TempDir(), model: "Test Model", thinking: "high", render: templates, done: make(chan struct{}), subs: map[chan struct{}]struct{}{}}
	models := []modelOption{{ID: "test", Name: "Test Model", Provider: "fixture"}, {ID: "other", Name: "Other Model with a very long display name", Provider: "fixture"}}
	f := &modelRPCFixture{a: a, models: models, current: models[0], effort: "high"}
	f.rpc = pi.NewRPC(f, a)
	a.engine = f.rpc
	a.modelKey = modelKey{"fixture", "test"}
	a.backend = pi.NewBackend(pi.Config{SessionsDir: t.TempDir()})
	s := &Server{backends: backendSet{a.backend}, mgr: &Manager{agents: map[string]*Agent{a.ID: a}}, tmpl: templates, modelsConfig: filepath.Join(t.TempDir(), "models.json")}
	routes := s.routes()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/s/browser/model" && r.Method == http.MethodGet {
			// Delay loads to verify that a dismissed request never reopens the card.
			time.Sleep(150 * time.Millisecond)
		}
		phase, fixture := strings.CutPrefix(r.URL.Path, "/fixture/")
		if !fixture {
			routes.ServeHTTP(w, r)
			return
		}
		switch phase {
		case "models-curated", "models-edited", "models-invalid", "models-empty":
			data := map[string]string{
				"models-curated": `{"models":[{"provider":"fixture","id":"missing","label":"Unavailable favorite"},{"provider":"fixture","id":"other","label":"Favorite"},{"provider":"fixture","id":"test"}]}`,
				"models-edited":  `{"models":[{"provider":"fixture","id":"test","label":"Everyday"}]}`,
				"models-invalid": `{"models":[`,
				"models-empty":   `{"models":[]}`,
			}[phase]
			if err := os.WriteFile(s.modelsConfig, []byte(data), 0600); err != nil {
				http.Error(w, err.Error(), 500)
			}
		case "models-remove":
			if err := os.Remove(s.modelsConfig); err != nil {
				http.Error(w, err.Error(), 500)
			}
		case "working":
			piEvent(a, "agent_start", nil)
		case "responding":
			piEvent(a, "message_start", []byte(`{"message":{"role":"assistant"}}`))
		case "idle":
			piEvent(a, "agent_settled", nil)
		case "compacting":
			piEvent(a, "compaction_start", nil)
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
	cmd := exec.CommandContext(ctx, node, "../../tests/tool-stream-browser.cjs", chrome, server.URL)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser test failed: %v\n%s", err, output)
	}
	t.Log(strings.TrimSpace(string(output)))
}
