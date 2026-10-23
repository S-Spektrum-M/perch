package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"perchd/internal/engine"
)

// listBackend is a backend with a fixed session list that cannot run sessions.
type listBackend struct {
	name    string
	list    []engine.SessionInfo
	created []string // dirs passed to Create
}

func (b *listBackend) Name() string               { return b.name }
func (b *listBackend) List() []engine.SessionInfo { return b.list }
func (b *listBackend) Find(id string) (engine.SessionInfo, bool) {
	for _, info := range b.list {
		if info.ID == id {
			return info, true
		}
	}
	return engine.SessionInfo{}, false
}
func (b *listBackend) Open(engine.SessionInfo, engine.Sink) (engine.Engine, error) {
	return nil, errors.New("open: " + b.name)
}
func (b *listBackend) Create(dir, name string, sink engine.Sink) (engine.Engine, error) {
	b.created = append(b.created, dir)
	return nil, errors.New("create: " + b.name)
}
func (b *listBackend) Duplicate(engine.SessionInfo, engine.Sink) (engine.Engine, error) {
	return nil, errors.New("duplicate: " + b.name)
}
func (b *listBackend) SaveModelDefaults(context.Context, engine.Model, string) error { return nil }

func twoBackends() (backendSet, *listBackend, *listBackend) {
	now := time.Now()
	pi := &listBackend{name: "pi", list: []engine.SessionInfo{
		{ID: "p1", Cwd: "/work/a", FirstMsg: "pi new", Modified: now.Add(-1 * time.Minute)},
		{ID: "p2", Cwd: "/work/b", FirstMsg: "pi old", Modified: now.Add(-3 * time.Hour)},
	}}
	cl := &listBackend{name: "claude", list: []engine.SessionInfo{
		{ID: "c1", Cwd: "/work/b", FirstMsg: "claude mid", Modified: now.Add(-1 * time.Hour)},
	}}
	return backendSet{pi, cl}, pi, cl
}

func TestBackendSetListMergesAndTags(t *testing.T) {
	bs, _, _ := twoBackends()
	var got []string
	for _, info := range bs.List() {
		got = append(got, info.ID+":"+info.Engine)
	}
	want := "p1:pi c1:claude p2:pi"
	if strings.Join(got, " ") != want {
		t.Fatalf("List = %v, want %s", got, want)
	}
	if info, ok := bs.Find("c1"); !ok || info.Engine != "claude" {
		t.Fatalf("Find(c1) = %+v, %v", info, ok)
	}
	if _, ok := bs.Find("nope"); ok {
		t.Fatal("Find(nope) found a session")
	}
	if bs.get("") != bs[0] || bs.get("claude") != bs[1] || bs.get("eta") != nil {
		t.Fatal("get picks the wrong backend")
	}
}

func TestManagerRoutesToBackend(t *testing.T) {
	bs, pi, cl := twoBackends()
	m := &Manager{backends: bs, agents: map[string]*Agent{}, starting: map[string]*startCall{}}
	if _, err := m.Create("claude", "/x", ""); err == nil || !strings.Contains(err.Error(), "could not start claude: create: claude") {
		t.Fatalf("Create(claude) error = %v", err)
	}
	if _, err := m.Create("", "/y", ""); err == nil || !strings.Contains(err.Error(), "create: pi") {
		t.Fatalf("Create(default) error = %v", err)
	}
	if len(cl.created) != 1 || len(pi.created) != 1 {
		t.Fatalf("created: claude %v, pi %v", cl.created, pi.created)
	}
	if _, err := m.Create("eta", "/z", ""); err == nil || !strings.Contains(err.Error(), `unknown engine "eta"`) {
		t.Fatalf("Create(eta) error = %v", err)
	}
	info, _ := bs.Find("c1")
	if _, err := m.Attach(info); err == nil || !strings.Contains(err.Error(), "open: claude") {
		t.Fatalf("Attach(c1) error = %v", err)
	}
	if _, err := m.Duplicate(info); err == nil || !strings.Contains(err.Error(), "duplicate: claude") {
		t.Fatalf("Duplicate(c1) error = %v", err)
	}
}

func TestPagesShowEngines(t *testing.T) {
	bs, _, _ := twoBackends()
	s := &Server{backends: bs, mgr: &Manager{agents: map[string]*Agent{}}, tmpl: loadTemplates(), root: t.TempDir()}

	w := httptest.NewRecorder()
	s.home(w, httptest.NewRequest("GET", "/", nil))
	home := w.Body.String()
	// With several backends, every session is tagged with its own.
	if strings.Count(home, `engine-tag">pi<`) != 2 || strings.Count(home, `engine-tag">claude<`) != 1 {
		t.Errorf("home page engine tags wrong:\n%s", home)
	}
	// A project's + opens a draft; it doesn't pick an engine.
	if !strings.Contains(home, `<form class="proj-new" method="get" action="/draft">`) || strings.Contains(home, `name="engine"`) {
		t.Errorf("project + should open a draft without an engine")
	}
}

func TestDraftPicksEngine(t *testing.T) {
	bs, pi, cl := twoBackends()
	dir := t.TempDir()
	s := &Server{backends: bs, mgr: &Manager{backends: bs, agents: map[string]*Agent{}, starting: map[string]*startCall{}}, tmpl: loadTemplates(), root: dir}

	get := func(srv *Server, target string) string {
		w := httptest.NewRecorder()
		srv.draft(w, httptest.NewRequest("GET", target, nil))
		if w.Code != 200 {
			t.Fatalf("GET %s = %d", target, w.Code)
		}
		return w.Body.String()
	}
	page := get(s, "/draft?dir="+url.QueryEscape(dir))
	for _, want := range []string{
		`<fieldset class="engine-choice" data-preset="">`,
		`<input type="radio" name="engine" value="pi" checked>`,
		`<input type="radio" name="engine" value="claude" >`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("draft page lacks %s", want)
		}
	}
	page = get(s, "/draft?engine=claude&dir="+url.QueryEscape(dir))
	if !strings.Contains(page, `data-preset="claude"`) || !strings.Contains(page, `value="claude" checked`) {
		t.Errorf("engine=claude not preselected")
	}
	single := &Server{backends: bs[:1], mgr: s.mgr, tmpl: s.tmpl, root: dir}
	if page := get(single, "/draft?dir="+url.QueryEscape(dir)); strings.Contains(page, "engine-choice") || !strings.Contains(page, `<input type="hidden" name="engine" value="pi">`) {
		t.Errorf("single-engine draft should not offer a choice")
	}

	post := func(form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/new", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		s.create(w, r)
		return w
	}
	// No message yet: nothing starts, the old POST /new links land on the draft.
	w := post(url.Values{"dir": {dir}, "name": {"n"}})
	if w.Code != 303 || w.Header().Get("Location") != "/draft?dir="+url.QueryEscape(dir)+"&name=n" {
		t.Fatalf("POST /new without message = %d %s", w.Code, w.Header().Get("Location"))
	}
	if len(pi.created)+len(cl.created) != 0 {
		t.Fatal("a session was started without a message")
	}
	// The first message starts the picked engine; a failure keeps the text.
	w = post(url.Values{"dir": {dir}, "engine": {"claude"}, "message": {"hello there"}})
	if len(cl.created) != 1 || len(pi.created) != 0 {
		t.Fatalf("created: claude %v, pi %v", cl.created, pi.created)
	}
	if body := w.Body.String(); w.Code != 500 || !strings.Contains(body, "create: claude") || !strings.Contains(body, ">hello there</textarea>") || !strings.Contains(body, `value="claude" checked`) {
		t.Errorf("failed start should re-show the draft with the message:\n%s", body)
	}
}

// catalogBackend also lists models for new sessions.
type catalogBackend struct {
	listBackend
	models []engine.Model
}

func (b *catalogBackend) Models(context.Context) ([]engine.Model, error) { return b.models, nil }

func TestDraftModels(t *testing.T) {
	bs, pi, _ := twoBackends()
	cl := &catalogBackend{listBackend: listBackend{name: "claude"}, models: []engine.Model{
		{Provider: "anthropic", ID: "default", Name: "Default (recommended)"},
		{Provider: "anthropic", ID: "opus", Name: "Opus 5.5"},
	}}
	bs[1] = cl
	s := &Server{backends: bs, tmpl: loadTemplates(), modelsConfig: filepath.Join(t.TempDir(), "models.json")}
	get := func(engine string) (int, string) {
		w := httptest.NewRecorder()
		s.draftModels(w, httptest.NewRequest("GET", "/draft/models?engine="+engine, nil))
		return w.Code, w.Body.String()
	}
	// The engine's own "default" entry is folded into "Default model".
	if code, body := get("claude"); code != 200 || body != `<option value="">Default model</option><option value="anthropic/opus">Opus 5.5</option>` {
		t.Errorf("claude models = %d %q", code, body)
	}
	// A curated list applies, per engine.
	if err := os.WriteFile(s.modelsConfigFor("claude"), []byte(`{"models":[{"provider":"anthropic","id":"opus","label":"Big"},{"provider":"anthropic","id":"gone"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, body := get("claude"); body != `<option value="">Default model</option><option value="anthropic/opus">Big</option>` {
		t.Errorf("curated claude models = %q", body)
	}
	// Without a catalog only the default is offered.
	if code, body := get(pi.name); code != 200 || body != `<option value="">Default model</option>` {
		t.Errorf("pi models = %d %q", code, body)
	}
	if code, _ := get("eta"); code != 404 {
		t.Errorf("unknown engine = %d", code)
	}
}
