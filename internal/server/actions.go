package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"perchd/internal/engine"
)

// Session actions from the session page's "⋯" menu. Plain form posts get a
// 303; fetches get 204, or the URL to go to as text/plain.

func (s *Server) rename(w http.ResponseWriter, r *http.Request) {
	a, err := s.agent(r, false)
	if a == nil {
		s.done(w, r, cmpErr(err, "session is not running"))
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	s.done(w, r, a.SetName(ctx, name))
}

func (s *Server) compact(w http.ResponseWriter, r *http.Request) {
	a, err := s.agent(r, false)
	if a == nil {
		s.done(w, r, cmpErr(err, "session is not running"))
		return
	}
	// Summarizing a long session is an LLM call; progress shows in the status
	// line and the replaced transcript reloads open pages.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	s.done(w, r, a.engine.Compact(ctx, strings.TrimSpace(r.FormValue("instructions"))))
}

// duplicate copies the session into a new one and opens it. The original's
// engine, if any, is left alone.
func (s *Server) duplicate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var info engine.SessionInfo
	if a := s.mgr.Get(id); a != nil {
		info, _ = a.Info()
	} else if found, ok := s.backends.Find(id); ok {
		info = found
	} else {
		s.done(w, r, errors.New("no such session"))
		return
	}
	if info.File == "" {
		s.done(w, r, errors.New("nothing to duplicate yet: send a prompt first"))
		return
	}
	if _, err := os.Stat(info.File); err != nil {
		s.done(w, r, errors.New("nothing to duplicate yet: send a prompt first"))
		return
	}
	a, err := s.mgr.Duplicate(info)
	if err != nil {
		s.done(w, r, err)
		return
	}
	// The fork keeps the name; tell the two apart in the session list.
	if info.Name != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		_ = a.SetName(ctx, info.Name+" (copy)")
	}
	s.goTo(w, r, "/s/"+a.ID)
}

// release stops the session's engine so it can be continued elsewhere,
// e.g. in a terminal. Nothing is deleted; opening the page attaches again.
func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	if a, _ := s.agent(r, false); a != nil {
		a.Close()
	}
	s.goTo(w, r, "/")
}

func (s *Server) goTo(w http.ResponseWriter, r *http.Request, url string) {
	if isFetch(r) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(url))
		return
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

// commands lists the extension commands, prompt templates and skills that can
// be run by sending "/name" as a prompt, for the prompt box's suggestions.
func (s *Server) commands(w http.ResponseWriter, r *http.Request) {
	a, err := s.agent(r, false)
	if a == nil {
		http.Error(w, cmpErr(err, "session is not running").Error(), http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	commands, err := a.engine.Commands(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(commands)
}

type statsView struct {
	UserMessages, AssistantMessages, ToolCalls  int
	Input, Output, CacheRead, CacheWrite, Total string
	Cost                                        string
	Context                                     string // "" when unknown
	ContextPercent                              float64
	Account                                     string
}

// stats renders token, cost and context usage for the session actions dialog.
func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	a, err := s.agent(r, false)
	if a == nil {
		http.Error(w, cmpErr(err, "session is not running").Error(), http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	st, err := a.engine.Stats(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	v := statsView{
		UserMessages: st.UserMessages, AssistantMessages: st.AssistantMessages, ToolCalls: st.ToolCalls,
		Input: tokens(st.Input), Output: tokens(st.Output),
		CacheRead: tokens(st.CacheRead), CacheWrite: tokens(st.CacheWrite),
		Total:   tokens(st.Total),
		Cost:    fmt.Sprintf("$%.2f", st.Cost),
		Account: st.Account,
	}
	if c := st.Context; c != nil {
		v.Context = tokens(c.Tokens) + " / " + tokens(c.Window)
		v.ContextPercent = math.Min(c.Percent, 100)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(s.tmpl.Fragment("stats", v)))
}

// tokens formats a token count compactly: 950, 12.3k, 1.2M.
func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 1_000:
		return strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64) + "k"
	}
	return strconv.FormatInt(n, 10)
}
