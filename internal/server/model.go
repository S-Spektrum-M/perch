package server

import (
	"context"
	"net/http"
	"time"

	"perchd/internal/engine"
)

type modelOption struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	Unavailable bool   `json:"-"` // picker presentation, not an engine model definition
}

type modelPickerView struct {
	Engine      string // backend name
	Models      []modelOption
	Model       modelOption
	Thinking    string
	Levels      []string
	MakeDefault bool
}

// modelSettings returns a fresh picker after each change so effort choices
// always reflect the selected model, including provider-specific levels.
func (s *Server) modelSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a, err := s.agent(r, false)
	if err != nil || a == nil {
		http.Error(w, "session is not running", http.StatusNotFound)
		return
	}
	config, err := loadModelsConfig(s.modelsConfigFor(a.EngineName()))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	// The engine supplies availability, not the curated list's membership or order.
	catalog, err := a.engine.Models(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	available := make([]modelOption, 0, len(catalog))
	for _, m := range catalog {
		available = append(available, modelOption{ID: m.ID, Name: m.Name, Provider: m.Provider})
	}
	models := config.options(available)
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var err error
		switch r.FormValue("setting") {
		case "model":
			found := false
			for _, model := range models {
				if model.ID == r.FormValue("modelId") && model.Provider == r.FormValue("provider") && !model.Unavailable {
					found = true
					break
				}
			}
			if !found {
				http.Error(w, "model is not in the picker or is unavailable in "+a.EngineName(), http.StatusBadRequest)
				return
			}
			err = a.engine.SetModel(ctx, r.FormValue("provider"), r.FormValue("modelId"))
		case "effort":
			err = a.engine.SetThinking(ctx, r.FormValue("level"))
		case "default":
			// Save the authoritative current state below, not client-supplied IDs.
		default:
			http.Error(w, "unknown setting", http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	state, err := a.engine.State(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var current engine.Model
	if state.Model != nil {
		current = *state.Model
	}
	makeDefault := r.Method == http.MethodPost && r.FormValue("makeDefault") == "true"
	if makeDefault {
		if err := a.backend.SaveModelDefaults(ctx, current, state.Thinking); err != nil {
			http.Error(w, "saving global defaults: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	a.mu.Lock()
	a.model, a.thinking = cmp(current.Name, current.ID), state.Thinking
	a.modelKey = modelKey{current.Provider, current.ID}
	a.modelLabels = config.labels()
	a.emitStatusLocked()
	a.mu.Unlock()
	levels, err := a.engine.ThinkingLevels(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(s.tmpl.Fragment("model-picker", modelPickerView{
		Engine:      a.EngineName(),
		Models:      models,
		Model:       modelOption{ID: current.ID, Name: current.Name, Provider: current.Provider},
		Thinking:    state.Thinking,
		Levels:      levels,
		MakeDefault: makeDefault,
	})))
}
