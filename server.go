package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

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
	base := template.Must(template.New("").Funcs(funcs).ParseFS(webFS, "web/templates/layout.html", "web/templates/fragments.html"))
	t := &Templates{base: base, pages: map[string]*template.Template{}}
	for _, page := range []string{"home", "new", "session", "error"} {
		t.pages[page] = template.Must(template.Must(base.Clone()).ParseFS(webFS, "web/templates/"+page+".html"))
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
	store        *Store
	mgr          *Manager
	root         string
	uploads      string // where prompt attachments are saved, per session
	tmpl         *Templates
	allowedHosts []string // additional exact DNS names, never wildcard domains
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	// not in Go's built-in MIME table; would otherwise be served as text/plain
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
	static, _ := fs.Sub(webFS, "web/static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /new", s.newForm)
	mux.HandleFunc("POST /new", s.create)
	mux.HandleFunc("GET /s/{id}", s.session)
	mux.HandleFunc("GET /s/{id}/events", s.events)
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
	return browserHeaders(bodyDeadlines(requestProtection(requestBodies(mux), s.allowedHosts)))
}

func (s *Server) fail(w http.ResponseWriter, status int, msg string) {
	s.tmpl.Page(w, status, "error", map[string]any{"Status": status, "Message": msg})
}

// ---------------------------------------------------------------------------
// Home

type sessionRow struct {
	SessionInfo
	Live      bool
	Streaming bool
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
	disk := s.store.List()
	onDisk := make(map[string]int, len(disk))
	rows := make([]sessionRow, 0, len(disk))
	for i, info := range disk {
		onDisk[info.ID] = i
		rows = append(rows, sessionRow{SessionInfo: info})
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
			fresh = append(fresh, sessionRow{SessionInfo: info, Live: true, Streaming: streaming})
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
	browse := path != ""
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
	entries, isGit, dirErr := listDir(path, hidden)
	parent := ""
	if path != "/" {
		parent = filepath.Dir(path)
	}
	s.tmpl.Page(w, status, "new", map[string]any{
		"Path":     path,
		"Parent":   parent,
		"IsGit":    isGit,
		"Name":     name,
		"Hidden":   hidden,
		"Browse":   browse,
		"Error":    errMsg,
		"DirErr":   dirErr,
		"Crumbs":   breadcrumbs(s.root, path),
		"Entries":  entries,
		"Projects": discoverProjects(s.root, s.store.List()),
	})
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	dir := r.FormValue("dir")
	if strings.HasPrefix(dir, "~") {
		home, _ := os.UserHomeDir()
		dir = home + dir[1:]
	}
	dir = filepath.Clean(dir)
	name := strings.TrimSpace(r.FormValue("name"))
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		s.renderNew(w, http.StatusBadRequest, dir, name, false, "not a directory: "+dir)
		return
	}
	a, err := s.mgr.Create(dir, name)
	if err != nil {
		s.renderNew(w, http.StatusInternalServerError, dir, name, false, "could not start pi: "+err.Error())
		return
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
	info, ok := s.store.Find(id)
	if !ok {
		return nil, nil
	}
	return s.mgr.Attach(info)
}

const pageMessages = 200

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	a, err := s.agent(r, true)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrSessionBusy) {
			status = http.StatusConflict
		}
		s.fail(w, status, "could not start pi: "+err.Error())
		return
	}
	if a == nil {
		s.fail(w, http.StatusNotFound, "no such session")
		return
	}
	limit := pageMessages
	if r.FormValue("all") != "" {
		limit = 0
	}
	s.tmpl.Page(w, http.StatusOK, "session", struct {
		Snapshot
		projectTree
	}{a.Snapshot(limit), s.projectTree("", false)})
}

// events streams UIEvents over Server-Sent Events. Events are batched: after
// a wake-up we wait briefly so a burst of token deltas goes out as one frame.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	a, _ := s.agent(r, false)
	if a == nil {
		w.WriteHeader(http.StatusNoContent) // tells EventSource to stop retrying
		return
	}
	flusher, ok := w.(http.Flusher)
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
		writeSSE(w, a.Epoch, since, []UIEvent{{T: "reload"}})
		flusher.Flush()
		return
	}

	wake, unsubscribe := a.Subscribe()
	defer unsubscribe()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		evs, reset := a.EventsSince(since)
		if reset {
			writeSSE(w, a.Epoch, since, []UIEvent{{T: "reload"}})
			flusher.Flush()
			return
		}
		if len(evs) > 0 {
			since = evs[len(evs)-1].Seq
			writeSSE(w, a.Epoch, since, coalesce(evs))
			flusher.Flush()
		}
		select {
		case <-wake:
			time.Sleep(40 * time.Millisecond)
		case <-a.done:
			// flush whatever the exit produced, then end the stream
			if evs, _ := a.EventsSince(since); len(evs) > 0 {
				writeSSE(w, a.Epoch, evs[len(evs)-1].Seq, coalesce(evs))
				flusher.Flush()
			}
			return
		case <-ping.C:
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Second))
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func writeSSE(w http.ResponseWriter, epoch string, seq uint64, evs []UIEvent) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Second))
	b, _ := json.Marshal(evs)
	fmt.Fprintf(w, "id: %s-%d\ndata: %s\n\n", epoch, seq, b)
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
func isFetch(r *http.Request) bool { return r.Header.Get("X-Pircd") != "" }

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
	// Like the TUI: `!cmd` runs a shell command whose output goes to the model
	// with the next prompt; `!!cmd` keeps it out of the model's context.
	if strings.HasPrefix(msg, "!") {
		if len(uploads) > 0 {
			s.done(w, r, errors.New("files can't be attached to a shell command"))
			return
		}
		exclude := strings.HasPrefix(msg, "!!")
		if command := strings.TrimSpace(strings.TrimLeft(msg, "!")); command != "" {
			go a.RunBash(command, exclude)
		}
		s.done(w, r, nil)
		return
	}
	files, err := s.saveUploads(a.ID, uploads)
	if err != nil {
		s.done(w, r, err)
		return
	}
	cmd := map[string]any{"type": "prompt", "message": withAttachments(msg, files)}
	if a.Streaming() {
		// "steer" interjects after the current tool calls; "followUp" waits.
		mode := r.FormValue("mode")
		if mode != "followUp" {
			mode = "steer"
		}
		cmd["streamingBehavior"] = mode
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	a.mu.Lock()
	if a.taskStarted.IsZero() {
		a.taskStarted = started
		a.taskTime = ""
		a.taskRead, a.taskWritten = 0, 0
	}
	a.mu.Unlock()
	_, err = a.Call(ctx, cmd)
	if err != nil {
		a.mu.Lock()
		if a.taskStarted.Equal(started) && !a.streaming {
			a.taskStarted = time.Time{}
		}
		a.mu.Unlock()
	}
	s.done(w, r, err)
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
		_, err = a.Call(ctx, map[string]any{"type": "abort_bash"})
		s.done(w, r, err)
		return
	}
	// Like Esc in the TUI: drop queued messages first so abort doesn't just
	// start on them, and hand their text back for the prompt box.
	var queued struct {
		Steering []string `json:"steering"`
		FollowUp []string `json:"followUp"`
	}
	if raw, err := a.Call(ctx, map[string]any{"type": "clear_queue"}); err == nil {
		_ = json.Unmarshal(raw, &queued)
	}
	if _, err = a.Call(ctx, map[string]any{"type": "abort"}); err == nil && isFetch(r) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprint(w, strings.Join(append(queued.Steering, queued.FollowUp...), "\n\n"))
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
	resp := map[string]any{}
	switch {
	case r.FormValue("cancelled") != "":
		resp["cancelled"] = true
	case r.FormValue("confirmed") != "":
		resp["confirmed"] = r.FormValue("confirmed") == "true"
	default:
		resp["value"] = r.FormValue("value")
	}
	s.done(w, r, a.RespondUI(r.FormValue("id"), resp))
}

func cmpErr(err error, msg string) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("%s", msg)
}
