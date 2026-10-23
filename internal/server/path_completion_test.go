package server

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestPathCompletion(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"catalyst", "catalyst-tui", ".hidden"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "cat-file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	for _, tc := range []struct {
		query      string
		count      int
		completion string
	}{
		{root + "/cat", 2, root + "/catalyst"},
		{root + "/catalyst-", 1, root + "/catalyst-tui/"},
		{root + "/", 2, root + "/catalyst"},
		{root + "/.", 1, root + "/.hidden/"},
		{root + "/missing", 0, ""},
	} {
		t.Run(tc.query, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.completePaths(w, httptest.NewRequest("GET", "/new/paths?q="+url.QueryEscape(tc.query), nil))
			if w.Code != 200 {
				t.Fatalf("status %d", w.Code)
			}
			var result pathCompletion
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Entries) != tc.count || result.Completion != tc.completion {
				t.Fatalf("got %+v", result)
			}
		})
	}
	w := httptest.NewRecorder()
	s.completePaths(w, httptest.NewRequest("GET", "/new/paths?q=relative", nil))
	if w.Code != 400 {
		t.Fatalf("relative path: status %d", w.Code)
	}
}
