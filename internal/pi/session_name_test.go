package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSuggestSessionName(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-pi")
	script := `#!/bin/sh
printf '%s\n' "$@" > args
pwd > cwd
tee input >/dev/null
printf '  Repair login flow\n'
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	b := NewBackend(Config{Bin: bin, Args: []string{"--session", "must-not-open"}})
	prompt := "-prompt with @secrets.txt\nand another line"
	name, err := b.SuggestSessionName(context.Background(), dir, prompt)
	if err != nil {
		t.Fatal(err)
	}
	if name != "Repair login flow" {
		t.Fatalf("name=%q", name)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--no-session", "--model\ngpt-6-luna\n", "--no-tools", "--no-context-files", "--no-extensions", "--no-mcp"} {
		if !strings.Contains(string(args), flag) {
			t.Errorf("missing %q in %s", flag, args)
		}
	}
	if strings.Contains(string(args), "must-not-open") || strings.Contains(string(args), prompt) {
		t.Fatalf("unsafe args: %s", args)
	}
	input, err := os.ReadFile(filepath.Join(dir, "input"))
	if err != nil {
		t.Fatal(err)
	}
	if string(input) != prompt {
		t.Fatalf("input=%q", input)
	}
}

func TestSuggestSessionNameRejectsInvalidOutput(t *testing.T) {
	for _, output := range []string{"", "A title\nAn explanation", "bad\x1btitle"} {
		t.Run(output, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "fake-pi")
			if err := os.WriteFile(filepath.Join(dir, "output"), []byte(output), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat output\n"), 0700); err != nil {
				t.Fatal(err)
			}
			b := NewBackend(Config{Bin: bin})
			if name, err := b.SuggestSessionName(context.Background(), dir, "Prompt"); err == nil {
				t.Fatalf("accepted %q", name)
			}
		})
	}
}
