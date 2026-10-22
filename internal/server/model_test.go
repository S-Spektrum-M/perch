package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"pircd/internal/pi"
)

// Exercise the real HTTP handler and Agent.Call without launching pi.
type modelRPCFixture struct {
	a       *Agent
	models  []modelOption
	current modelOption
	effort  string
	calls   []string
}

func (f *modelRPCFixture) Write(data []byte) (int, error) {
	var cmd map[string]string
	if err := json.Unmarshal(data, &cmd); err != nil {
		return 0, err
	}
	f.calls = append(f.calls, cmd["type"])
	var result any = map[string]any{}
	switch cmd["type"] {
	case "get_available_models":
		result = map[string]any{"models": f.models}
	case "get_state":
		result = map[string]any{"sessionId": "test", "model": f.current, "thinkingLevel": f.effort}
	case "get_messages":
		result = map[string]any{"messages": []pi.Message{}}
	case "get_available_thinking_levels":
		result = map[string]any{"levels": []string{"off", "low", "high"}}
	case "set_model":
		for _, model := range f.models {
			if model.Provider == cmd["provider"] && model.ID == cmd["modelId"] {
				f.current = model
			}
		}
	case "set_thinking_level":
		f.effort = cmd["level"]
	default:
		return 0, fmt.Errorf("unexpected RPC command: %s", cmd["type"])
	}
	response, err := json.Marshal(map[string]any{"type": "response", "id": cmd["id"], "success": true, "data": result})
	if err != nil {
		return 0, err
	}
	f.a.handleLine(response)
	return len(data), nil
}

func (f *modelRPCFixture) Close() error { return nil }

func modelTestServer(t *testing.T) (*Server, *modelRPCFixture) {
	t.Helper()
	templates := loadTemplates()
	f := &modelRPCFixture{models: []modelOption{
		{Provider: "p", ID: "a", Name: "Actual A"},
		{Provider: "p", ID: "b", Name: "Actual B"},
	}, effort: "high"}
	f.current = f.models[0]
	f.a = &Agent{ID: "test", Epoch: "test", Cwd: t.TempDir(), model: "Actual A", modelKey: modelKey{"p", "a"}, thinking: f.effort, render: templates,
		stdin: f, done: make(chan struct{}), pending: map[string]chan rpcResponse{}, subs: map[chan struct{}]struct{}{}}
	return &Server{mgr: &Manager{agents: map[string]*Agent{"test": f.a}}, store: pi.NewStore(t.TempDir()), tmpl: templates,
		modelsConfig: filepath.Join(t.TempDir(), "models.json")}, f
}

func modelRequest(s *Server, body url.Values) *httptest.ResponseRecorder {
	method := "GET"
	if body != nil {
		method = "POST"
	}
	r := httptest.NewRequest(method, "/s/test/model", strings.NewReader(body.Encode()))
	r.SetPathValue("id", "test")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.modelSettings(w, r)
	return w
}

func writeModelsConfig(t *testing.T, s *Server, data string) {
	t.Helper()
	if err := os.WriteFile(s.modelsConfig, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func pickerIDs(html string) []string {
	matches := regexp.MustCompile(`data-model-id="([^"]+)"`).FindAllStringSubmatch(html, -1)
	ids := []string{}
	for _, match := range matches {
		ids = append(ids, match[1])
	}
	return ids
}

func TestModelPickerConfigReload(t *testing.T) {
	s, f := modelTestServer(t)
	for _, tt := range []struct {
		config string
		ids    []string
	}{
		{"", []string{"a", "b"}},
		{`{"models":[{"provider":"p","id":"b","label":"Favorite"},{"provider":"p","id":"a"}]}`, []string{"b", "a"}},
		{`{"models":[{"provider":"p","id":"a","label":"Everyday"}]}`, []string{"a"}},
		{`{"models":[]}`, []string{}},
	} {
		if tt.config != "" {
			writeModelsConfig(t, s, tt.config)
		}
		w := modelRequest(s, nil)
		if w.Code != 200 || !reflect.DeepEqual(pickerIDs(w.Body.String()), tt.ids) {
			t.Fatalf("picker = %d %s, want %v", w.Code, w.Body.String(), tt.ids)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("picker must not cache edits")
		}
		if strings.Contains(tt.config, "Everyday") {
			if got := f.a.statusLocked().Model; got != "Everyday" {
				t.Fatalf("prompt model = %q", got)
			}
			if err := f.a.loadState(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := f.a.statusLocked().Model; got != "Everyday" {
				t.Fatalf("pi state refresh lost label: %q", got)
			}
		}
	}
	if err := os.Remove(s.modelsConfig); err != nil {
		t.Fatal(err)
	}
	w := modelRequest(s, nil)
	if !reflect.DeepEqual(pickerIDs(w.Body.String()), []string{"a", "b"}) || f.a.statusLocked().Model != "Actual A" {
		t.Fatal("removing config must restore catalog and actual name")
	}
}

func TestModelPickerUnavailableAndUnlisted(t *testing.T) {
	s, f := modelTestServer(t)
	writeModelsConfig(t, s, `{"models":[{"provider":"p","id":"missing","label":"Offline"},{"provider":"p","id":"b","label":"Favorite"}]}`)
	w := modelRequest(s, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `disabled data-model-unavailable aria-disabled="true"`) || !strings.Contains(w.Body.String(), "Unavailable in pi") {
		t.Fatalf("missing unavailable entry: %d %s", w.Code, w.Body.String())
	}
	if !reflect.DeepEqual(pickerIDs(w.Body.String()), []string{"missing", "b"}) || f.a.statusLocked().Model != "Actual A" {
		t.Fatal("unlisted current model must remain actual without being added to list")
	}
	for _, id := range []string{"missing", "a"} {
		f.calls = nil
		w := modelRequest(s, url.Values{"setting": {"model"}, "provider": {"p"}, "modelId": {id}})
		if w.Code != 400 || strings.Contains(strings.Join(f.calls, ","), "set_model") {
			t.Fatalf("invalid selection %s: %d %v", id, w.Code, f.calls)
		}
	}
	// Effort still works for a current model that isn't in the curated list.
	w = modelRequest(s, url.Values{"setting": {"effort"}, "level": {"low"}})
	if w.Code != 200 || f.a.statusLocked().Thinking != "low" {
		t.Fatalf("unlisted model effort: %d %s", w.Code, w.Body.String())
	}
	w = modelRequest(s, url.Values{"setting": {"model"}, "provider": {"p"}, "modelId": {"b"}})
	if w.Code != 200 || f.current.ID != "b" || f.a.statusLocked().Model != "Favorite" {
		t.Fatalf("configured model selection: %d %s", w.Code, w.Body.String())
	}
}

func TestModelPickerConfigErrorsBlockChanges(t *testing.T) {
	s, f := modelTestServer(t)
	writeModelsConfig(t, s, `{"models":[`)
	for _, body := range []url.Values{nil, {"setting": {"model"}, "provider": {"p"}, "modelId": {"b"}}} {
		w := modelRequest(s, body)
		if w.Code != 500 || !strings.Contains(w.Body.String(), "invalid models config") || len(f.calls) != 0 {
			t.Fatalf("invalid config must fail before RPC/side effects: %d %s %v", w.Code, w.Body.String(), f.calls)
		}
	}
}

func TestConfiguredLabelNeverSavedAsModelIdentity(t *testing.T) {
	s, _ := modelTestServer(t)
	dir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	writeModelsConfig(t, s, `{"models":[{"provider":"p","id":"b","label":"Friendly, not an ID"}]}`)
	w := modelRequest(s, url.Values{"setting": {"model"}, "provider": {"p"}, "modelId": {"b"}, "makeDefault": {"true"}})
	if w.Code != 200 {
		t.Fatalf("save defaults: %d %s", w.Code, w.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]string
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["defaultModel"] != "b" || settings["defaultProvider"] != "p" || strings.Contains(string(data), "Friendly") {
		t.Fatalf("defaults contain UI alias: %s", data)
	}
}

func TestConfiguredLabelOnInitialSessionPage(t *testing.T) {
	s, _ := modelTestServer(t)
	writeModelsConfig(t, s, `{"models":[{"provider":"p","id":"a","label":"Everyday"}]}`)
	r := httptest.NewRequest("GET", "/s/test", nil)
	r.SetPathValue("id", "test")
	w := httptest.NewRecorder()
	s.session(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `<span class="model-label">Everyday · `) {
		t.Fatalf("initial page is missing configured label: %d %s", w.Code, w.Body.String())
	}
}
