package main

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestAcceptsGzip(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false}, {"br", false}, {"gzip", true}, {"br, gzip", true},
		{"GZip; q=0.5", true}, {"gzip;q=0", false}, {"gzip;q=bogus", false},
		{"gzip;q=2", false}, {"gzip;q=NaN", false}, {"*;q=0.5", true},
		{"*;q=1, gzip;q=0", false}, {"gzip;q=0, *;q=1", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			if got := acceptsGzip([]string{tc.value}); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	if !acceptsGzip([]string{"br", "gzip"}) {
		t.Fatal("multiple header lines not accepted")
	}
}

func TestGzipResponses(t *testing.T) {
	body := strings.Repeat("compressible response\n", 100)
	for _, tc := range []struct {
		name, method, path, accept, media, encoding, rangeHeader string
		status                                                   int
		gzip                                                     bool
	}{
		{name: "html", media: "text/html; charset=utf-8", gzip: true},
		{name: "sniffed text", gzip: true},
		{name: "css", media: "text/css", gzip: true},
		{name: "javascript", media: "application/javascript", gzip: true},
		{name: "json", media: "application/json", gzip: true},
		{name: "unaccepted", accept: "br", media: "text/html"},
		{name: "disabled", accept: "gzip;q=0", media: "text/html"},
		{name: "head", method: "HEAD", media: "text/html"},
		{name: "event route", path: "/s/test/events", media: "text/plain"},
		{name: "event media", media: "text/event-stream"},
		{name: "binary", media: "image/png"},
		{name: "encoded", media: "text/html", encoding: "br"},
		{name: "range", media: "text/html", rangeHeader: "bytes=0-9"},
		{name: "partial", media: "text/html", status: 206},
		{name: "not modified", media: "text/html", status: 304},
		{name: "no content", media: "text/html", status: 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method, path, accept := tc.method, tc.path, tc.accept
			if method == "" {
				method = "GET"
			}
			if path == "" {
				path = "/"
			}
			if accept == "" {
				accept = "gzip"
			}
			r := httptest.NewRequest(method, path, nil)
			r.Header.Set("Accept-Encoding", accept)
			r.Header.Set("Range", tc.rangeHeader)
			w := httptest.NewRecorder()
			gzipResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.media != "" {
					w.Header().Set("Content-Type", tc.media)
				}
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				if method != "HEAD" && tc.status != 204 && tc.status != 304 {
					_, _ = io.WriteString(w, body)
				}
			})).ServeHTTP(w, r)
			res := w.Result()
			defer res.Body.Close()
			if got := res.Header.Get("Content-Encoding") == "gzip"; got != tc.gzip {
				t.Fatalf("gzip = %v, want %v", got, tc.gzip)
			}
			if !tc.gzip {
				return
			}
			if res.Header.Get("Content-Length") != "" {
				t.Fatal("uncompressed length retained")
			}
			if !strings.Contains(res.Header.Get("Vary"), "Accept-Encoding") {
				t.Fatal("missing Vary")
			}
			gz, err := gzip.NewReader(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			defer gz.Close()
			got, err := io.ReadAll(gz)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != body {
				t.Fatal("body did not round trip")
			}
		})
	}
}

func TestGzipRoutes(t *testing.T) {
	s := &Server{tmpl: loadTemplates()}
	for _, path := range []string{"/static/app.css", "/static/session.js", "/static/vendor/markdown-it.min.js", "/static/manifest.webmanifest"} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://localhost"+path, nil)
			r.Header.Set("Accept-Encoding", "gzip")
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, r)
			if w.Code != 200 || w.Header().Get("Content-Encoding") != "gzip" {
				t.Fatalf("status=%d encoding=%q", w.Code, w.Header().Get("Content-Encoding"))
			}
		})
	}
}

func TestGzipSSEFlush(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/s/test/events", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	gzipResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Fatal("SSE lost Flusher")
		}
		if err := writeSSE(w, "epoch", 1, nil); err != nil {
			t.Fatal(err)
		}
	})).ServeHTTP(w, r)
	if !w.Flushed || w.Header().Get("Content-Encoding") != "" {
		t.Fatal("SSE compressed or not flushed")
	}
	if !strings.Contains(w.Body.String(), "id: epoch-1") {
		t.Fatal("missing event")
	}
}
