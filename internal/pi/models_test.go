package pi

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestModelsParsesListModels(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "pi")
	script := `#!/bin/sh
[ "$1" = --list-models ] || exit 2
cat <<'TABLE'
provider  model                     context  max-out  thinking  images
openai    gpt-5                     400K     128K     yes       yes
openrouter anthropic/claude-x       200K     64K      yes       yes

TABLE
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	models, err := NewBackend(Config{Bin: bin}).Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range models {
		got = append(got, m.Provider+"/"+m.ID)
	}
	if want := []string{"openai/gpt-5", "openrouter/anthropic/claude-x"}; !slices.Equal(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
}
