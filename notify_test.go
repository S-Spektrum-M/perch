package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNotifier(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || string(body) != "Pi finished working in project." {
			t.Errorf("unexpected request: %s %s", r.Method, body)
		}
		if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Click") != "https://pi.example/s/abc" {
			t.Errorf("unexpected headers: %v", r.Header)
		}
		if r.Header.Get("Icon") != "https://pi.example/static/icon-192.png" {
			t.Errorf("unexpected icon: %q", r.Header.Get("Icon"))
		}
		if r.Header.Get("Tags") != "" {
			t.Errorf("unexpected emoji tags: %q", r.Header.Get("Tags"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	n, err := newNotifier(server.URL, "secret", "https://pi.example/")
	if err != nil {
		t.Fatal(err)
	}
	n.send("abc", "project")
}

func TestNotifierConfig(t *testing.T) {
	n, err := newNotifier("", "", "")
	if n != nil || err != nil {
		t.Fatal("notifications should be disabled")
	}
	for _, raw := range []string{"topic", "ftp://example/topic", "https://user:pass@example/topic", "https://example/topic?secret=x"} {
		if _, err := newNotifier(raw, "", ""); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

// A settled event must not notify on attachment or duplicate delivery.
func TestDoneNotification(t *testing.T) {
	calls := make(chan string, 4)
	a := &Agent{ID: "abc", Cwd: "/tmp/project", render: loadTemplates(), subs: map[chan struct{}]struct{}{}, notify: func(id, label string) { calls <- id + ":" + label }}
	a.handleEvent("agent_settled", nil)
	a.handleEvent("agent_start", nil)
	a.handleEvent("tool_execution_end", nil)
	a.handleEvent("agent_settled", nil)
	a.handleEvent("agent_settled", nil)
	select {
	case got := <-calls:
		if !strings.HasSuffix(got, ":project") {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("missing completion")
	}
	select {
	case got := <-calls:
		t.Fatalf("duplicate notification: %s", got)
	case <-time.After(20 * time.Millisecond):
	}
}
