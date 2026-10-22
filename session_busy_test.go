package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionProcess(t *testing.T) {
	for _, mode := range []string{"fd", "absolute", "relative", "equals", "unrelated", "self"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			file := filepath.Join(root, "session.jsonl")
			if err := os.WriteFile(file, nil, 0600); err != nil {
				t.Fatal(err)
			}
			proc := filepath.Join(root, "123")
			if err := os.MkdirAll(filepath.Join(proc, "fd"), 0700); err != nil {
				t.Fatal(err)
			}
			link := func(old, new string) {
				t.Helper()
				if err := os.Symlink(old, new); err != nil {
					t.Fatal(err)
				}
			}
			args := "node\x00pi\x00--session\x00" + file + "\x00"
			self := 0
			switch mode {
			case "fd":
				link(file, filepath.Join(proc, "fd", "3"))
				args = "node\x00pi\x00"
			case "relative":
				link(root, filepath.Join(proc, "cwd"))
				args = "pi\x00--session\x00session.jsonl\x00"
			case "equals":
				args = "pi\x00--session=" + file + "\x00"
			case "unrelated":
				args = "pi\x00--session\x00missing.jsonl\x00"
			case "self":
				self = 123
			}
			if err := os.WriteFile(filepath.Join(proc, "cmdline"), []byte(args), 0600); err != nil {
				t.Fatal(err)
			}
			want := 123
			if mode == "unrelated" || mode == "self" {
				want = 0
			}
			if got := sessionProcessIn(root, file, self); got != want {
				t.Fatalf("got PID %d, want %d", got, want)
			}
		})
	}
}

func TestSessionProcessMissingProc(t *testing.T) {
	if got := sessionProcessIn(filepath.Join(t.TempDir(), "missing"), "missing", 0); got != 0 {
		t.Fatalf("got PID %d", got)
	}
}
