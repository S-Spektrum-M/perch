package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Opt-in real-browser regression test: PERCHD_BROWSER_TESTS=1 go test -run TestBrowserFormOrigin.
func TestBrowserFormOrigin(t *testing.T) {
	if os.Getenv("PERCHD_BROWSER_TESTS") != "1" {
		t.Skip("set PERCHD_BROWSER_TESTS=1 to run Chrome integration test")
	}
	chrome, err := exec.LookPath("google-chrome")
	if err != nil {
		t.Fatal(err)
	}
	origins := make(chan string, 1)
	srv := httptest.NewServer(browserHeaders(requestProtection(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			origins <- r.Header.Get("Origin")
			fmt.Fprint(w, "accepted")
			return
		}
		if r.URL.Path == "/submit.js" {
			w.Header().Set("Content-Type", "text/javascript")
			fmt.Fprint(w, `document.querySelector("form").submit()`)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<form method="post" action="/new"><input name="dir" value="/tmp"></form><script src="/submit.js"></script>`)
	}), nil)))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, chrome, "--headless", "--no-sandbox", "--disable-gpu", "--user-data-dir="+t.TempDir(), "--virtual-time-budget=1000", "--dump-dom", srv.URL).Output()
	if err != nil {
		t.Fatalf("Chrome: %v", err)
	}
	if !strings.Contains(string(out), "accepted") {
		t.Fatalf("form rejected: %s", out)
	}
	select {
	case origin := <-origins:
		if origin != srv.URL {
			t.Fatalf("origin %q, want %q", origin, srv.URL)
		}
	default:
		t.Fatal("form never reached handler")
	}
}
