package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestSessionHistory(t *testing.T) {
	a := &Agent{ID: "history", Cwd: t.TempDir(), done: make(chan struct{})}
	for i := 1; i <= 125; i++ {
		content, _ := json.Marshal(fmt.Sprintf("history-message-%03d", i))
		a.messages = append(a.messages, Message{Role: "user", Content: content})
	}
	s := &Server{
		store: NewStore(t.TempDir()),
		mgr:   &Manager{agents: map[string]*Agent{a.ID: a}},
		tmpl:  loadTemplates(),
	}
	for _, tc := range []struct {
		query string
		count int
		next  int
	}{
		{"", 50, 100},
		{"?limit=100", 100, 150},
		{"?limit=150", 125, 0},
		{"?all=1", 125, 0},
		{"?limit=garbage", 50, 100},
		{"?limit=0", 50, 100},
		{"?limit=-1", 50, 100},
		{"?limit=10", 50, 100},
		{"?limit=" + strconv.Itoa(int(^uint(0)>>1)), 50, 100},
	} {
		t.Run(tc.query, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://localhost/s/history"+tc.query, nil)
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			_, body, ok := strings.Cut(w.Body.String(), `<main id="transcript" class="transcript">`)
			if !ok {
				t.Fatal("missing transcript")
			}
			body, _, _ = strings.Cut(body, "</main>")
			if got := strings.Count(body, `class="msg user"`); got != tc.count {
				t.Fatalf("got %d messages, want %d", got, tc.count)
			}
			for i := 1; i <= 125; i++ {
				want := i > 125-tc.count
				if strings.Contains(body, fmt.Sprintf("history-message-%03d", i)) != want {
					t.Fatalf("message %d presence, want %v", i, want)
				}
			}
			if tc.next == 0 {
				if strings.Contains(body, "Load earlier") {
					t.Fatal("unexpected history link")
				}
			} else {
				link := fmt.Sprintf(`href="?limit=%d#transcript">Load earlier</a>`, tc.next)
				if !strings.Contains(body, link) {
					t.Fatalf("missing %s", link)
				}
				if !strings.Contains(body, fmt.Sprintf("%d earlier messages hidden", 125-tc.count)) {
					t.Fatal("incorrect hidden count")
				}
			}
		})
	}
}
