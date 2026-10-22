package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type modelKey struct {
	Provider string
	ID       string
}

type configuredModel struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Label    string `json:"label,omitempty"`
}

type modelsConfig struct {
	Models []configuredModel `json:"models"`
}

// DefaultModelsConfigPath is the default model picker config location.
func DefaultModelsConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "pircd", "models.json")
}

// A missing file means use pi's catalog; an explicit empty list means show none.
// Read on each picker request so edits never require restarting pircd.
func loadModelsConfig(path string) (*modelsConfig, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading models config %s: %w", path, err)
	}
	var config modelsConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("invalid models config %s: %w", path, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("invalid models config %s: expected one JSON object", path)
	}
	if config.Models == nil {
		return nil, fmt.Errorf("invalid models config %s: models must be an array (use [] for an empty list)", path)
	}
	seen := map[modelKey]bool{}
	for i := range config.Models {
		model := &config.Models[i]
		model.Provider = strings.TrimSpace(model.Provider)
		model.ID = strings.TrimSpace(model.ID)
		model.Label = strings.TrimSpace(model.Label)
		if model.Provider == "" || model.ID == "" {
			return nil, fmt.Errorf("invalid models config %s: entry %d needs a provider and id", path, i+1)
		}
		key := modelKey{model.Provider, model.ID}
		if seen[key] {
			return nil, fmt.Errorf("invalid models config %s: duplicate model %s/%s", path, model.Provider, model.ID)
		}
		seen[key] = true
	}
	return &config, nil
}

func (c *modelsConfig) labels() map[modelKey]string {
	labels := map[modelKey]string{}
	if c != nil {
		for _, model := range c.Models {
			if model.Label != "" {
				labels[modelKey{model.Provider, model.ID}] = model.Label
			}
		}
	}
	return labels
}

func (c *modelsConfig) options(available []modelOption) []modelOption {
	if c == nil {
		return available
	}
	catalog := map[modelKey]modelOption{}
	for _, model := range available {
		catalog[modelKey{model.Provider, model.ID}] = model
	}
	models := make([]modelOption, 0, len(c.Models))
	for _, entry := range c.Models {
		model, ok := catalog[modelKey{entry.Provider, entry.ID}]
		model.ID, model.Provider = entry.ID, entry.Provider
		model.Name = cmp(entry.Label, cmp(model.Name, entry.ID))
		model.Unavailable = !ok
		models = append(models, model)
	}
	return models
}
