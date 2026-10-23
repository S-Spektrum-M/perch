package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"perchd/internal/engine"
	"perchd/internal/notify"
	"perchd/web"
)

// Templates holds the parsed page and fragment templates. Fragments
// (web/templates/fragments.html) are shared by full-page renders and by the
// live updates agents push to browsers.
type Templates struct {
	base  *template.Template
	pages map[string]*template.Template
}

func loadTemplates() *Templates {
	home, _ := os.UserHomeDir()
	funcs := template.FuncMap{
		"ago":      ago,
		"shortAgo": shortAgo,
		"tilde": func(p string) string {
			if home != "" && strings.HasPrefix(p, home) {
				return "~" + strings.TrimPrefix(p, home)
			}
			return p
		},
		"base": filepath.Base,
	}
	base := template.Must(template.New("").Funcs(funcs).ParseFS(web.FS, "templates/layout.html", "templates/fragments.html"))
	t := &Templates{base: base, pages: map[string]*template.Template{}}
	for _, page := range []string{"home", "new", "session", "draft", "error"} {
		t.pages[page] = template.Must(template.Must(base.Clone()).ParseFS(web.FS, "templates/"+page+".html"))
	}
	return t
}

func (t *Templates) Fragment(name string, data any) string {
	var b strings.Builder
	if err := t.base.ExecuteTemplate(&b, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
	return b.String()
}

func (t *Templates) Page(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := t.pages[name].ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render page %s: %v", name, err)
	}
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2 Jan 2006")
	}
}

func shortAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dy", int(d.Hours()/(24*365)))
	}
}

// ---------------------------------------------------------------------------

type Server struct {
	backends     backendSet // the first is the default for new sessions
	mgr          *Manager
	root         string
	uploads      string // where prompt attachments are saved, per session
	modelsConfig string // optional curated model picker configuration
	tmpl         *Templates
	allowedHosts []string // additional exact DNS names, never wildcard domains
}

// Config configures a Server.
type Config struct {
	Backends     []engine.Backend // run and store sessions; the first is the default for new ones
	Idle         time.Duration    // stop engines idle this long
	HideStatus   map[string]bool  // extension status keys to hide
	Root         string           // root of the new-session directory picker
	Uploads      string           // where prompt attachments are saved, per session
	ModelsConfig string           // optional curated model picker configuration
	AllowedHosts []string         // additional exact DNS names, never wildcard domains
	Notifier     *notify.Notifier
}

func New(cfg Config) *Server {
	mgr := NewManager(cfg.Backends, cfg.Idle, cfg.HideStatus)
	mgr.notifier = cfg.Notifier
	s := &Server{
		backends:     cfg.Backends,
		mgr:          mgr,
		root:         filepath.Clean(cfg.Root),
		uploads:      cfg.Uploads,
		modelsConfig: cfg.ModelsConfig,
		tmpl:         loadTemplates(),
		allowedHosts: cfg.AllowedHosts,
	}
	mgr.render = s.tmpl
	return s
}

// Handler returns the HTTP handler with all routes and middleware.
func (s *Server) Handler() http.Handler { return s.routes() }

// Close stops every running pi process.
func (s *Server) Close() { s.mgr.CloseAll() }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	// not in Go's built-in MIME table; would otherwise be served as text/plain
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
	static, _ := fs.Sub(web.FS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /new", s.newForm)
	mux.HandleFunc("GET /new/paths", s.completePaths)
	mux.HandleFunc("POST /new", s.create)
	mux.HandleFunc("GET /draft", s.draft)
	mux.HandleFunc("GET /draft/models", s.draftModels)
	mux.HandleFunc("GET /s/{id}", s.session)
	mux.HandleFunc("GET /s/{id}/events", s.events)
	mux.HandleFunc("GET /s/{id}/tool-output/{tool}", s.toolOutput)
	mux.HandleFunc("POST /s/{id}/prompt", s.prompt)
	mux.HandleFunc("POST /s/{id}/abort", s.abort)
	mux.HandleFunc("POST /s/{id}/ui", s.respondUI)
	mux.HandleFunc("POST /s/{id}/rename", s.rename)
	mux.HandleFunc("POST /s/{id}/compact", s.compact)
	mux.HandleFunc("POST /s/{id}/duplicate", s.duplicate)
	mux.HandleFunc("POST /s/{id}/release", s.release)
	mux.HandleFunc("GET /s/{id}/commands", s.commands)
	mux.HandleFunc("GET /s/{id}/stats", s.stats)
	mux.HandleFunc("GET /s/{id}/model", s.modelSettings)
	mux.HandleFunc("POST /s/{id}/model", s.modelSettings)
	return browserHeaders(gzipResponses(bodyDeadlines(requestProtection(requestBodies(mux), s.allowedHosts))))
}

func (s *Server) fail(w http.ResponseWriter, status int, msg string) {
	s.tmpl.Page(w, status, "error", map[string]any{"Status": status, "Message": msg})
}

// ---------------------------------------------------------------------------
// Home

type sessionRow struct {
	engine.SessionInfo
	Live       bool
	Streaming  bool
	ShowEngine bool // several backends are configured; label the session with its own
}

type projectGroup struct {
	Name     string
	Path     string
	Sessions []sessionRow
	Overflow []sessionRow
	LastMod  time.Time
}

const (
	maxSessionsPerProject = 3
	maxTopProjects        = 3
)

// projectTree is the grouped project/session list shown on the home page and
// in the session page's sidebar.
type projectTree struct {
	Groups     []*projectGroup
	MoreGroups []*projectGroup
	Query      string
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	s.tmpl.Page(w, http.StatusOK, "home", s.projectTree(r.FormValue("q"), r.FormValue("all") != ""))
}

func (s *Server) projectTree(rawQ string, showAll bool) projectTree {
	disk := s.backends.List()
	multi := len(s.backends) > 1
	onDisk := make(map[string]int, len(disk))
	rows := make([]sessionRow, 0, len(disk))
	for i, info := range disk {
		onDisk[info.ID] = i
		rows = append(rows, sessionRow{SessionInfo: info, ShowEngine: multi})
	}
	// Live agents: mark their rows, and prepend sessions that have no file
	// yet (created but never prompted).
	var fresh []sessionRow
	for _, a := range s.mgr.All() {
		info, streaming := a.Info()
		if i, ok := onDisk[info.ID]; ok {
			rows[i].Live, rows[i].Streaming = true, streaming
			if info.Name != "" {
				rows[i].Name = info.Name
			}
		} else {
			if info.Modified.IsZero() {
				info.Modified = time.Now()
			}
			fresh = append(fresh, sessionRow{SessionInfo: info, Live: true, Streaming: streaming, ShowEngine: multi})
		}
	}
	rows = append(fresh, rows...)

	q := strings.ToLower(strings.TrimSpace(rawQ))

	byPath := map[string]*projectGroup{}
	var groups []*projectGroup

	for _, row := range rows {
		if q != "" && !strings.Contains(strings.ToLower(row.Title()+" "+row.Cwd), q) {
			continue
		}
		cwd := filepath.Clean(row.Cwd)
		g, ok := byPath[cwd]
		if !ok {
			name := filepath.Base(cwd)
			if cwd == "." || cwd == "/" {
				name = cwd
			}
			g = &projectGroup{
				Name:    name,
				Path:    cwd,
				LastMod: row.Modified,
			}
			byPath[cwd] = g
			groups = append(groups, g)
		}
		if row.Modified.After(g.LastMod) {
			g.LastMod = row.Modified
		}
		if showAll || q != "" || len(g.Sessions) < maxSessionsPerProject {
			g.Sessions = append(g.Sessions, row)
		} else {
			g.Overflow = append(g.Overflow, row)
		}
	}

	// Disambiguate projects that share the same basename by including parent/base.
	nameCount := map[string]int{}
	for _, g := range groups {
		nameCount[g.Name]++
	}
	for _, g := range groups {
		if nameCount[g.Name] > 1 {
			parent := filepath.Base(filepath.Dir(g.Path))
			if parent != "" && parent != "." && parent != "/" {
				g.Name = parent + "/" + g.Name
			}
		}
	}

	topGroups := groups
	var moreGroups []*projectGroup
	if !showAll && q == "" && len(groups) > maxTopProjects {
		topGroups = groups[:maxTopProjects]
		moreGroups = groups[maxTopProjects:]
	}

	return projectTree{Groups: topGroups, MoreGroups: moreGroups, Query: rawQ}
}

// ---------------------------------------------------------------------------
// New session

func (s *Server) newForm(w http.ResponseWriter, r *http.Request) {
	path := r.FormValue("path")
	if r.FormValue("nav") == "goto" {
		path = r.FormValue("goto")
	}
	s.renderNew(w, http.StatusOK, path, r.FormValue("name"),
		(r.FormValue("hidden") != "") != (r.FormValue("togglehidden") != ""), "")
}

func (s *Server) renderNew(w http.ResponseWriter, status int, path, name string, hidden bool, errMsg string) {
	if strings.HasPrefix(path, "~") {
		home, _ := os.UserHomeDir()
		path = home + path[1:]
	}
	if path == "" {
		path = s.root
	}
	path = filepath.Clean(path)
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		if errMsg == "" {
			errMsg = "not a directory: " + path
		}
		path = s.root
	}
	s.tmpl.Page(w, status, "new", map[string]any{
		"Path": path,
		"Query": func() string {
			if path != s.root {
				return path
			}
			return ""
		}(),
		"Name":     name,
		"Error":    errMsg,
		"Projects": discoverProjects(s.root, s.backends.List()),
	})
}

// draftView feeds the draft page: a session that has not started yet.
type draftView struct {
	projectTree
	Cwd, Name string
	Engine    string   // preselected engine; "" lets the page pick (last used, else the first)
	Model     string   // "provider/id" to put back after a failed start
	Engines   []string // in -engine order
	Message   string   // text to put back after a failed start
	Error     string
}

// draft shows the prompt box for a new session in dir. Nothing runs until
// the first message, which says which engine to start.
func (s *Server) draft(w http.ResponseWriter, r *http.Request) {
	s.renderDraft(w, http.StatusOK, draftView{
		Cwd: r.FormValue("dir"), Name: strings.TrimSpace(r.FormValue("name")), Engine: r.FormValue("engine"),
	})
}

func (s *Server) renderDraft(w http.ResponseWriter, status int, v draftView) {
	dir, ok := expandDir(v.Cwd)
	if !ok {
		s.renderNew(w, http.StatusBadRequest, v.Cwd, v.Name, false, "not a directory: "+dir)
		return
	}
	v.Cwd = dir
	if s.backends.get(v.Engine) == nil {
		v.Engine = ""
	}
	v.Engines = s.backends.names()
	v.projectTree = s.projectTree("", false)
	s.tmpl.Page(w, status, "draft", v)
}

// draftModels lists the models a new session of ?engine= can start with, as
// <option>s for the draft page. "" means the engine's own default.
func (s *Server) draftModels(w http.ResponseWriter, r *http.Request) {
	b := s.backends.get(r.FormValue("engine"))
	if b == nil {
		http.Error(w, "unknown engine", http.StatusNotFound)
		return
	}
	var models []modelOption
	if catalog, ok := b.(engine.ModelCatalog); ok {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		available, err := catalog.Models(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		config, err := loadModelsConfig(s.modelsConfigFor(b.Name()))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		opts := make([]modelOption, 0, len(available))
		for _, m := range available {
			opts = append(opts, modelOption{ID: m.ID, Name: m.Name, Provider: m.Provider})
		}
		for _, m := range config.options(opts) {
			// The "Default model" option already stands for an engine's default.
			if !m.Unavailable && m.ID != "default" {
				models = append(models, m)
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(s.tmpl.Fragment("model-options", models)))
}

// expandDir resolves ~ and cleans dir, and reports whether it is a directory.
func expandDir(dir string) (string, bool) {
	if strings.HasPrefix(dir, "~") {
		home, _ := os.UserHomeDir()
		dir = home + dir[1:]
	}
	dir = filepath.Clean(dir)
	st, err := os.Stat(dir)
	return dir, err == nil && st.IsDir()
}

// create starts a session with the engine picked on the draft page and sends
// it the first message. Without a message it just shows the draft page.
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	uploads, err := parseUploads(w, r)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	v := draftView{
		Cwd: r.FormValue("dir"), Name: strings.TrimSpace(r.FormValue("name")),
		Engine: r.FormValue("engine"), Model: r.FormValue("model"), Message: r.FormValue("message"),
	}
	msg := strings.TrimSpace(v.Message)
	if msg == "" && len(uploads) == 0 {
		q := url.Values{"dir": {v.Cwd}}
		if v.Name != "" {
			q.Set("name", v.Name)
		}
		http.Redirect(w, r, "/draft?"+q.Encode(), http.StatusSeeOther)
		return
	}
	dir, ok := expandDir(v.Cwd)
	if !ok {
		s.renderNew(w, http.StatusBadRequest, dir, v.Name, false, "not a directory: "+dir)
		return
	}
	a, err := s.mgr.Create(v.Engine, dir, v.Name)
	if err != nil {
		v.Error = err.Error()
		s.renderDraft(w, http.StatusInternalServerError, v)
		return
	}
	if provider, id, ok := strings.Cut(r.FormValue("model"), "/"); ok {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		err := a.SetModel(ctx, provider, id)
		cancel()
		if err != nil {
			a.Notice("error", "could not switch to "+id+": "+err.Error())
		}
	}
	if err := s.send(r.Context(), a, started, msg, uploads, false); err != nil {
		a.Notice("error", "first message not sent: "+err.Error())
	}
	http.Redirect(w, r, "/s/"+a.ID, http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Session

// agent returns the running agent for {id}, starting it if start is set.
func (s *Server) agent(r *http.Request, start bool) (*Agent, error) {
	id := r.PathValue("id")
	if a := s.mgr.Get(id); a != nil && !a.Exited() {
		return a, nil
	}
	if !start {
		return nil, nil
	}
	info, ok := s.backends.Find(id)
	if !ok {
		return nil, nil
	}
	return s.mgr.Attach(info)
}

const pageMessages = 50

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	a, err := s.agent(r, true)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, engine.ErrSessionBusy) {
			status = http.StatusConflict
		}
		s.fail(w, status, err.Error())
		return
	}
	if a == nil {
		s.fail(w, http.StatusNotFound, "no such session")
		return
	}
	// Invalid picker config must not prevent reading a session. The picker
	// itself reports the error; until fixed, show the actual current model.
	config, _ := loadModelsConfig(s.modelsConfigFor(a.EngineName()))
	a.mu.Lock()
	a.modelLabels = config.labels()
	a.mu.Unlock()
	limit := pageMessages
	// Leave room for the next page without overflowing an int.
	if n, err := strconv.Atoi(r.FormValue("limit")); err == nil && n > limit && n <= int(^uint(0)>>1)-pageMessages {
		limit = n
	}
	if r.FormValue("all") != "" {
		limit = 0 // retain old show-all links
	}
	s.tmpl.Page(w, http.StatusOK, "session", struct {
		Snapshot
		projectTree
		EarlierLimit int
	}{a.Snapshot(limit), s.projectTree("", false), limit + pageMessages})
}

// events streams UIEvents over Server-Sent Events. Events are batched: after
// a wake-up we wait briefly so a burst of token deltas goes out as one frame.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	a, _ := s.agent(r, false)
	if a == nil {
		w.WriteHeader(http.StatusNoContent) // tells EventSource to stop retrying
		return
	}
	_, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = r.FormValue("since")
	}
	epoch, seqStr, _ := strings.Cut(cursor, "-")
	since, _ := strconv.ParseUint(seqStr, 10, 64)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")

	if epoch != a.Epoch {
		_ = writeSSE(w, a.Epoch, since, []UIEvent{{T: "reload"}})
		return
	}

	wake, unsubscribe := a.Subscribe()
	defer unsubscribe()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		evs, reset := a.EventsSince(since)
		if reset {
			_ = writeSSE(w, a.Epoch, since, []UIEvent{{T: "reload"}})
			return
		}
		if len(evs) > 0 {
			since = evs[len(evs)-1].Seq
			if err := writeSSE(w, a.Epoch, since, coalesce(evs)); err != nil {
				return
			}
		}
		select {
		case <-wake:
			time.Sleep(40 * time.Millisecond)
		case <-a.done:
			// flush whatever the exit produced, then end the stream
			if evs, _ := a.EventsSince(since); len(evs) > 0 {
				_ = writeSSE(w, a.Epoch, evs[len(evs)-1].Seq, coalesce(evs))
			}
			return
		case <-ping.C:
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Second))
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func writeSSE(w http.ResponseWriter, epoch string, seq uint64, evs []UIEvent) error {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
	b, _ := json.Marshal(evs)
	if _, err := fmt.Fprintf(w, "id: %s-%d\ndata: %s\n\n", epoch, seq, b); err != nil {
		return err
	}
	return rc.Flush()
}

// coalesce merges runs of text deltas for the same block.
func coalesce(evs []UIEvent) []UIEvent {
	out := make([]UIEvent, 0, len(evs))
	for _, ev := range evs {
		if n := len(out); n > 0 && ev.T == "delta" && out[n-1].T == "delta" && out[n-1].Idx == ev.Idx {
			out[n-1].S += ev.S
			continue
		}
		out = append(out, ev)
	}
	return out
}

// isFetch reports whether the request came from session.js rather than a
// plain form post (which gets a redirect back to the page).
func isFetch(r *http.Request) bool { return r.Header.Get("X-Perchd") != "" }

func (s *Server) done(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case err != nil && isFetch(r):
		http.Error(w, err.Error(), http.StatusBadGateway)
	case err != nil:
		s.fail(w, http.StatusBadGateway, err.Error())
	case isFetch(r):
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Redirect(w, r, "/s/"+r.PathValue("id"), http.StatusSeeOther)
	}
}

func (s *Server) prompt(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	a, err := s.agent(r, true)
	if a == nil {
		s.done(w, r, cmpErr(err, "no such session"))
		return
	}
	uploads, err := parseUploads(w, r)
	if err != nil {
		s.done(w, r, err)
		return
	}
	msg := strings.TrimSpace(r.FormValue("message"))
	if msg == "" && len(uploads) == 0 {
		s.done(w, r, nil)
		return
	}
	s.done(w, r, s.send(r.Context(), a, started, msg, uploads, r.FormValue("mode") == "followUp"))
}

// send delivers a prompt with its attachments to a's engine, or runs it as a
// shell command when it starts with "!". started is when the request came in.
func (s *Server) send(ctx context.Context, a *Agent, started time.Time, msg string, uploads []*multipart.FileHeader, followUp bool) error {
	// Like the TUI: `!cmd` runs a shell command whose output goes to the model
	// with the next prompt; `!!cmd` keeps it out of the model's context.
	if strings.HasPrefix(msg, "!") {
		if len(uploads) > 0 {
			return errors.New("files can't be attached to a shell command")
		}
		exclude := strings.HasPrefix(msg, "!!")
		if command := strings.TrimSpace(strings.TrimLeft(msg, "!")); command != "" {
			go a.RunShell(command, exclude)
		}
		return nil
	}
	files, err := s.saveUploads(a.ID, uploads)
	if err != nil {
		return err
	}
	mode := engine.PromptNow
	if a.Streaming() {
		// "steer" interjects after the current tool calls; "followUp" waits.
		mode = engine.PromptSteer
		if followUp {
			mode = engine.PromptFollowUp
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	a.mu.Lock()
	if a.taskStarted.IsZero() {
		a.taskStarted = started
		a.taskTime = ""
		a.taskRead, a.taskWritten = 0, 0
	}
	a.mu.Unlock()
	err = a.engine.Prompt(ctx, withAttachments(msg, files), mode)
	if err == nil {
		a.suggestName(a.backend, msg)
	}
	if err != nil {
		a.mu.Lock()
		if a.taskStarted.Equal(started) && !a.streaming {
			a.taskStarted = time.Time{}
		}
		a.mu.Unlock()
	}
	return err
}

func (s *Server) abort(w http.ResponseWriter, r *http.Request) {
	a, err := s.agent(r, false)
	if a == nil {
		s.done(w, r, cmpErr(err, "session is not running"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if a.ShellRunning() && !a.Streaming() {
		s.done(w, r, a.engine.AbortShell(ctx))
		return
	}
	// Like Esc in the TUI: queued messages are dropped too; hand their text
	// back for the prompt box.
	queued, err := a.engine.Abort(ctx)
	if err == nil && isFetch(r) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprint(w, strings.Join(queued, "\n\n"))
		return
	}
	s.done(w, r, err)
}

func (s *Server) respondUI(w http.ResponseWriter, r *http.Request) {
	a, err := s.agent(r, false)
	if a == nil {
		s.done(w, r, cmpErr(err, "session is not running"))
		return
	}
	var resp engine.UIResponse
	switch {
	case r.FormValue("cancelled") != "":
		resp.Cancelled = true
	case r.FormValue("confirmed") != "":
		confirmed := r.FormValue("confirmed") == "true"
		resp.Confirmed = &confirmed
	default:
		resp.Value = r.FormValue("value")
	}
	s.done(w, r, a.RespondUI(r.FormValue("id"), resp))
}

func cmpErr(err error, msg string) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("%s", msg)
}
