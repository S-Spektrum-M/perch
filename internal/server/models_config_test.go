package server

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultModelsConfigPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if got := DefaultModelsConfigPath(); got != filepath.Join(dir, "perchd", "models.json") {
		t.Fatalf("path = %q", got)
	}
}

func TestLoadModelsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	if config, err := loadModelsConfig(path); config != nil || err != nil {
		t.Fatalf("missing config = %#v, %v", config, err)
	}
	for _, tt := range []struct {
		name, data, wantError string
	}{
		{"empty list", `{"models":[]}`, ""},
		{"valid", `{"models":[{"provider":" p ","id":" m ","label":" Friendly "}]}`, ""},
		{"same id different provider", `{"models":[{"provider":"p","id":"m"},{"provider":"q","id":"m"}]}`, ""},
		{"invalid JSON", `{`, "invalid models config"},
		{"null config", `null`, "models must be an array"},
		{"missing models", `{}`, "models must be an array"},
		{"null models", `{"models":null}`, "models must be an array"},
		{"wrong models type", `{"models":{}}`, "invalid models config"},
		{"missing provider", `{"models":[{"id":"m"}]}`, "entry 1 needs a provider and id"},
		{"blank id", `{"models":[{"provider":"p","id":" "}]}`, "entry 1 needs a provider and id"},
		{"null entry", `{"models":[null]}`, "entry 1 needs a provider and id"},
		{"duplicate", `{"models":[{"provider":"p","id":"m"},{"provider":" p ","id":" m "}]}`, "duplicate model p/m"},
		{"misspelled label", `{"models":[{"provider":"p","id":"m","lable":"oops"}]}`, "unknown field"},
		{"trailing object", `{"models":[]} {}`, "expected one JSON object"},
		{"trailing garbage", `{"models":[]} garbage`, "expected one JSON object"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tt.data), 0600); err != nil {
				t.Fatal(err)
			}
			config, err := loadModelsConfig(path)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) || !strings.Contains(err.Error(), path) {
					t.Fatalf("error = %v, want %q and file path", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.name == "valid" && !reflect.DeepEqual(config.Models, []configuredModel{{Provider: "p", ID: "m", Label: "Friendly"}}) {
				t.Fatalf("config = %#v", config)
			}
		})
	}
	if _, err := loadModelsConfig(t.TempDir()); err == nil {
		t.Fatal("unreadable config must not fall back to pi")
	}
}

func TestConfiguredModelOptions(t *testing.T) {
	available := []modelOption{
		{Provider: "p", ID: "a", Name: "Actual A"},
		{Provider: "p", ID: "b", Name: "Actual B"},
		{Provider: "other", ID: "missing", Name: "Different provider"},
	}
	config := &modelsConfig{Models: []configuredModel{
		{Provider: "p", ID: "b", Label: "Favorite"},
		{Provider: "p", ID: "missing", Label: "Not installed"},
		{Provider: "p", ID: "a"},
		{Provider: "p", ID: "unknown"},
	}}
	want := []modelOption{
		{Provider: "p", ID: "b", Name: "Favorite"},
		{Provider: "p", ID: "missing", Name: "Not installed", Unavailable: true},
		{Provider: "p", ID: "a", Name: "Actual A"},
		{Provider: "p", ID: "unknown", Name: "unknown", Unavailable: true},
	}
	if got := config.options(available); !reflect.DeepEqual(got, want) {
		t.Fatalf("options = %#v, want %#v", got, want)
	}
	if available[1].Name != "Actual B" {
		t.Fatal("labels must not modify pi's catalog")
	}
	var missing *modelsConfig
	if !reflect.DeepEqual(missing.options(available), available) {
		t.Fatal("missing config must preserve pi's list")
	}
	if got := (&modelsConfig{Models: []configuredModel{}}).options(available); len(got) != 0 {
		t.Fatalf("explicit empty list fell back to pi: %#v", got)
	}
}
