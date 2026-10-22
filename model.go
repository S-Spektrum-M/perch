package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type modelOption struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

type modelPickerView struct {
	Models      []modelOption
	Model       modelOption
	Thinking    string
	Levels      []string
	MakeDefault bool
}

// modelSettings returns a fresh picker after each change so effort choices
// always reflect the selected model, including provider-specific levels.
func (s *Server) modelSettings(w http.ResponseWriter, r *http.Request) {
	a, err := s.agent(r, false)
	if err != nil || a == nil {
		http.Error(w, "session is not running", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	call := func(cmd map[string]any, dst any) error {
		raw, err := a.Call(ctx, cmd)
		if err != nil {
			return err
		}
		if dst != nil {
			return json.Unmarshal(raw, dst)
		}
		return nil
	}
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var cmd map[string]any
		switch r.FormValue("setting") {
		case "model":
			cmd = map[string]any{"type": "set_model", "provider": r.FormValue("provider"), "modelId": r.FormValue("modelId")}
		case "effort":
			cmd = map[string]any{"type": "set_thinking_level", "level": r.FormValue("level")}
		case "default":
			// Save the authoritative current state below, not client-supplied IDs.
		default:
			http.Error(w, "unknown setting", 400)
			return
		}
		if cmd != nil {
			if err := call(cmd, nil); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
		}
	}
	var state struct {
		Model    modelOption `json:"model"`
		Thinking string      `json:"thinkingLevel"`
	}
	if err := call(map[string]any{"type": "get_state"}, &state); err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	makeDefault := r.Method == http.MethodPost && r.FormValue("makeDefault") == "true"
	if makeDefault {
		if err := saveModelDefaults(ctx, state.Model, state.Thinking); err != nil {
			http.Error(w, "saving global defaults: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	a.mu.Lock()
	a.model, a.thinking = cmp(state.Model.Name, state.Model.ID), state.Thinking
	a.emitStatusLocked()
	a.mu.Unlock()
	var models struct {
		Models []modelOption `json:"models"`
	}
	if err := call(map[string]any{"type": "get_available_models"}, &models); err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	var levels struct {
		Levels []string `json:"levels"`
	}
	if err := call(map[string]any{"type": "get_available_thinking_levels"}, &levels); err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(s.tmpl.Fragment("model-picker", modelPickerView{models.Models, state.Model, state.Thinking, levels.Levels, makeDefault})))
}
