package notify

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNotifier(t *testing.T) {
	for _, tc := range []struct{ backend, name string }{
		{"pi", "Pi"},
		{"claude", "Claude"},
		{"agy", "Antigravity"},
	} {
		t.Run(tc.backend, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.Method != "POST" || string(body) != tc.name+" finished working in project." {
					t.Errorf("unexpected request: %s %s", r.Method, body)
				}
				if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Click") != "https://pi.example/s/abc" {
					t.Errorf("unexpected headers: %v", r.Header)
				}
				if r.Header.Get("Icon") != "https://pi.example/static/icon-192.png" {
					t.Errorf("unexpected icon: %q", r.Header.Get("Icon"))
				}
				if r.Header.Get("Title") != tc.name+" is done" {
					t.Errorf("unexpected title: %q", r.Header.Get("Title"))
				}
				if r.Header.Get("Tags") != "" {
					t.Errorf("unexpected emoji tags: %q", r.Header.Get("Tags"))
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			n, err := New(server.URL, "secret", "https://pi.example/")
			if err != nil {
				t.Fatal(err)
			}
			n.Send("abc", "project", tc.backend)
		})
	}
}

func TestQuestionNotifier(t *testing.T) {
	for _, tc := range []struct{ backend, name string }{{"pi", "Pi"}, {"claude", "Claude"}, {"agy", "Antigravity"}} {
		t.Run(tc.backend, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.Method != "POST" || string(body) != tc.name+" is waiting for your answer in project." {
					t.Errorf("unexpected request: %s %s", r.Method, body)
				}
				if r.Header.Get("Title") != tc.name+" needs your input" {
					t.Errorf("unexpected title: %q", r.Header.Get("Title"))
				}
				if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Click") != "https://pi.example/s/abc" || r.Header.Get("Icon") != "https://pi.example/static/icon-192.png" {
					t.Errorf("unexpected headers: %v", r.Header)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			n, err := New(server.URL, "secret", "https://pi.example/")
			if err != nil {
				t.Fatal(err)
			}
			n.Question("abc", "project", tc.backend)
		})
	}
}

func TestNotifierConfig(t *testing.T) {
	n, err := New("", "", "")
	if n != nil || err != nil {
		t.Fatal("notifications should be disabled")
	}
	for _, raw := range []string{"topic", "ftp://example/topic", "https://user:pass@example/topic", "https://example/topic?secret=x"} {
		if _, err := New(raw, "", ""); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}
