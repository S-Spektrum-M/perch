package main

import (
	"bytes"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestRequestBodyLimits(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body string
		status                  int
	}{
		{"empty", "", "", 204},
		{"unsupported", "text/plain", "hello", 415},
		{"at limit", "application/x-www-form-urlencoded", "a=" + strings.Repeat("x", maxFormBytes-2), 204},
		{"valid", "application/x-www-form-urlencoded", "message=hello", 204},
		{"oversized", "application/x-www-form-urlencoded", "message=" + strings.Repeat("x", maxFormBytes), 413},
		{"malformed", "multipart/form-data; boundary=missing", "broken", 400},
		{"boundary", "multipart/form-data", "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := requestBodies(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
			r := httptest.NewRequest("POST", "/s/test/prompt", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.contentType)
			if tc.body != "" {
				r.ContentLength = -1
			} // exercise streaming limits too
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("got %d, want %d", w.Code, tc.status)
			}
		})
	}
}

func TestDeclaredUploadTooLarge(t *testing.T) {
	r := httptest.NewRequest("POST", "/s/test/prompt", strings.NewReader("unread"))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	r.ContentLength = maxUploadBytes + 1
	w := httptest.NewRecorder()
	requestBodies(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("oversized upload reached handler") })).ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatalf("got %d want 413", w.Code)
	}
}

func TestMultipartConstraints(t *testing.T) {
	for _, tc := range []struct {
		name         string
		files, field int
		status       int
	}{
		{"valid", 32, 0, 204}, {"too many", 33, 0, 400}, {"large field", 0, maxFormBytes + 1, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			for i := 0; i < tc.files; i++ {
				part, err := mw.CreateFormFile("files", fmt.Sprintf("file-%d", i))
				if err != nil {
					t.Fatal(err)
				}
				_, _ = part.Write([]byte("test"))
			}
			if tc.field > 0 {
				_ = mw.WriteField("message", strings.Repeat("x", tc.field))
			}
			_ = mw.Close()
			r := httptest.NewRequest("POST", "/s/test/prompt", &body)
			r.Header.Set("Content-Type", mw.FormDataContentType())
			w := httptest.NewRecorder()
			requestBodies(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("got %d want %d", w.Code, tc.status)
			}
		})
	}
}

func TestMultipartTempCleanup(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("files", "large.txt")
	_, _ = part.Write(bytes.Repeat([]byte("x"), 33<<20))
	_ = mw.Close()
	r := httptest.NewRequest("POST", "/s/test/prompt", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	requestBodies(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entries, _ := os.ReadDir(temp)
		if len(entries) == 0 {
			t.Error("expected disk-backed multipart temporary file")
		}
		w.WriteHeader(204)
	})).ServeHTTP(w, r)
	entries, err := os.ReadDir(temp)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary files remain: %v %v", entries, err)
	}
}

func TestBrowserHeaders(t *testing.T) {
	for _, path := range []string{"/", "/s/test", "/static/app.css"} {
		w := httptest.NewRecorder()
		browserHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "rejected", 403) })).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		for key, want := range map[string]string{"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer"} {
			if got := w.Header().Get(key); got != want {
				t.Errorf("%s: got %q", key, got)
			}
		}
		want := "no-store"
		if strings.HasPrefix(path, "/static/") {
			want = "no-cache"
		}
		if w.Header().Get("Cache-Control") != want {
			t.Error("incorrect cache policy")
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") {
			t.Error("missing CSP")
		}
	}
}

type failedStream struct{ http.ResponseWriter }

func (failedStream) Write([]byte) (int, error) { return 0, errors.New("disconnected") }
func TestSSEWriteFailure(t *testing.T) {
	if err := writeSSE(failedStream{httptest.NewRecorder()}, "epoch", 1, nil); err == nil {
		t.Error("write error ignored")
	}
}
