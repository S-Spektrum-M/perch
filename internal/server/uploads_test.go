package server

import (
	"bytes"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"perchd/internal/pi"
)

func TestUploads(t *testing.T) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("message", "look at these")
	for _, f := range []struct{ name, data string }{
		{"shot.png", "png-1"},
		{"shot.png", "png-2"},
		{"../../etc/evil.txt", "evil"},
		{"", ""}, // what an empty file input submits
	} {
		w, _ := mw.CreateFormFile("files", f.name)
		_, _ = w.Write([]byte(f.data))
	}
	_ = mw.Close()

	r := httptest.NewRequest("POST", "/s/x/prompt", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	headers, err := parseUploads(httptest.NewRecorder(), r)
	if err != nil {
		t.Fatal(err)
	}
	if r.FormValue("message") != "look at these" {
		t.Errorf("message = %q", r.FormValue("message"))
	}
	s := &Server{uploads: t.TempDir()}
	paths, err := s.saveUploads("sess-1", headers)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.uploads, "sess-1")
	want := []string{filepath.Join(dir, "shot.png"), filepath.Join(dir, "shot-1.png"), filepath.Join(dir, "evil.txt")}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i, data := range []string{"png-1", "png-2", "evil"} {
		if b, _ := os.ReadFile(paths[i]); string(b) != data {
			t.Errorf("%s = %q, want %q", paths[i], b, data)
		}
	}

	got := withAttachments("look at these", paths[:1])
	if got != "look at these\n\nAttached files:\n- "+paths[0] {
		t.Errorf("withAttachments = %q", got)
	}
	if got := withAttachments("", paths[:1]); got != "Attached files:\n- "+paths[0] {
		t.Errorf("withAttachments without text = %q", got)
	}
}

func TestCleanUploads(t *testing.T) {
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour)
	sessions := t.TempDir()
	backend := pi.NewBackend(pi.Config{SessionsDir: sessions})
	s := &Server{
		backends: backendSet{backend},
		mgr:      NewManager(backendSet{backend}, time.Hour, nil),
		uploads:  t.TempDir(),
	}
	session := func(id string, mod time.Time) {
		p := filepath.Join(sessions, id+".jsonl")
		header := `{"type":"session","id":"` + id + `","cwd":"/tmp","timestamp":"2026-01-01T00:00:00Z"}` + "\n"
		if err := os.WriteFile(p, []byte(header), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, mod, mod)
	}
	upload := func(id string, mod time.Time) {
		dir := filepath.Join(s.uploads, id)
		_ = os.MkdirAll(dir, 0o700)
		p := filepath.Join(dir, "shot.png")
		_ = os.WriteFile(p, []byte("png"), 0o600)
		_ = os.Chtimes(p, mod, mod)
		_ = os.Chtimes(dir, mod, mod)
	}

	session("stale", old)
	upload("stale", old) // session and uploads both old: removed
	session("active", now.Add(-24*time.Hour))
	upload("active", old)     // session used recently: kept
	upload("orphan-old", old) // no session, old uploads: removed
	upload("orphan-new", now) // no session file yet (just uploaded): kept

	s.cleanUploads(7*24*time.Hour, now)

	for id, kept := range map[string]bool{"stale": false, "active": true, "orphan-old": false, "orphan-new": true} {
		_, err := os.Stat(filepath.Join(s.uploads, id))
		if exists := err == nil; exists != kept {
			t.Errorf("%s: exists = %v, want %v", id, exists, kept)
		}
	}
}
