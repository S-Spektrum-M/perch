package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"perchd/internal/engine"
)

// saveModelDefaults writes the defaults for new sessions to pi's settings.json.
func saveModelDefaults(ctx context.Context, model engine.Model, thinking string) error {
	dir := os.Getenv("PI_CODING_AGENT_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, ".pi", "agent")
	}
	return writeModelDefaults(ctx, filepath.Join(dir, "settings.json"), model, thinking)
}

func writeModelDefaults(ctx context.Context, path string, model engine.Model, thinking string) error {
	if model.Provider == "" || model.ID == "" || thinking == "" {
		return fmt.Errorf("current model or effort is unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	// Pi's proper-lockfile uses this same directory lock. Never overwrite a
	// concurrent settings update; time out rather than stealing its lock.
	for {
		err := os.Mkdir(path+".lock", 0700)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer os.Remove(path + ".lock")
	settings := map[string]json.RawMessage{}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return err
		}
		if settings == nil {
			return fmt.Errorf("settings must be a JSON object")
		}
	}
	for key, value := range map[string]string{"defaultProvider": model.Provider, "defaultModel": model.ID, "defaultThinkingLevel": thinking} {
		settings[key], _ = json.Marshal(value)
	}
	// A per-model override takes precedence over defaultThinkingLevel at startup.
	if raw, ok := settings["modelThinkingLevels"]; ok {
		var levels map[string]json.RawMessage
		if err := json.Unmarshal(raw, &levels); err != nil {
			return err
		}
		if levels != nil {
			levels[model.Provider+"/"+model.ID], _ = json.Marshal(thinking)
			settings["modelThinkingLevels"], _ = json.Marshal(levels)
		}
	}
	data, err = json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
