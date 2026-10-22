package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestModelDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	path := filepath.Join(dir, "settings.json")
	original := `{"theme":"dark","unknown":{"keep":true},"modelThinkingLevels":{"p/m":"low","other/model":"medium"}}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := saveModelDefaults(context.Background(), modelOption{Provider: "p", ID: "m"}, "high"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"defaultProvider": "p", "defaultModel": "m", "defaultThinkingLevel": "high", "theme": "dark"} {
		if got[key] != value {
			t.Errorf("%s = %v, want %s", key, got[key], value)
		}
	}
	if got["unknown"].(map[string]any)["keep"] != true {
		t.Fatal("lost unknown setting")
	}
	levels := got["modelThinkingLevels"].(map[string]any)
	if levels["p/m"] != "high" || levels["other/model"] != "medium" {
		t.Fatalf("levels: %v", levels)
	}
}

func TestModelDefaultsMissingAndInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent", "settings.json")
	model := modelOption{Provider: "p", ID: "m"}
	if err := writeModelDefaults(context.Background(), path, model, "low"); err != nil {
		t.Fatal(err)
	}
	for _, original := range []string{"invalid JSON", "null", "[]"} {
		if err := os.WriteFile(path, []byte(original), 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeModelDefaults(context.Background(), path, model, "high"); err == nil {
			t.Fatal("expected invalid settings error")
		}
		data, _ := os.ReadFile(path)
		if string(data) != original {
			t.Fatal("overwrote invalid settings")
		}
	}
}

func TestModelDefaultsLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.Mkdir(path+".lock", 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := writeModelDefaults(ctx, path, modelOption{Provider: "p", ID: "m"}, "low"); err == nil {
		t.Fatal("expected lock timeout")
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatal("removed another writer's lock")
	}
}
