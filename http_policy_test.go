package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestBodyLimits(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body string
		status                  int
	}{
		{"valid", "application/x-www-form-urlencoded", "message=hello", 204},
		{"oversized", "application/x-www-form-urlencoded", "message=" + strings.Repeat("x", maxFormBytes), 413},
		{"malformed", "multipart/form-data; boundary=missing", "broken", 400},
		{"boundary", "multipart/form-data", "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := requestBodies(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
			r := httptest.NewRequest("POST", "/s/test/prompt", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("got %d, want %d", w.Code, tc.status)
			}
		})
	}
}
