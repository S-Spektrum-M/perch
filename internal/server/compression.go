package server

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
)

// gzipResponses compresses ordinary responses, but never buffers live SSE or
// interferes with byte ranges. Unwrap preserves ResponseController support.
func gzipResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if r.Method == http.MethodHead || r.Header.Get("Range") != "" || strings.HasSuffix(r.URL.Path, "/events") {
			next.ServeHTTP(w, r)
			return
		}
		if !acceptsGzip(r.Header.Values("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer func() {
			if gw.gzip != nil {
				_ = gw.gzip.Close()
			}
		}()
		next.ServeHTTP(gw, r)
	})
}

func acceptsGzip(values []string) bool {
	wildcard := false
	for _, value := range values {
		for _, entry := range strings.Split(value, ",") {
			coding, params, _ := strings.Cut(strings.TrimSpace(entry), ";")
			quality := 1.0
			for _, param := range strings.Split(params, ";") {
				key, value, _ := strings.Cut(strings.TrimSpace(param), "=")
				if strings.EqualFold(key, "q") {
					q, err := strconv.ParseFloat(value, 64)
					quality = 0
					if err == nil && q >= 0 && q <= 1 {
						quality = q
					}
				}
			}
			if strings.EqualFold(coding, "gzip") {
				return quality > 0
			}
			if coding == "*" {
				wildcard = quality > 0
			}
		}
	}
	return wildcard
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gzip  *gzip.Writer
	wrote bool
}

func (w *gzipResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *gzipResponseWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	if status >= 100 && status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.wrote = true
	h := w.Header()
	media, _, _ := strings.Cut(h.Get("Content-Type"), ";")
	compressible := strings.HasPrefix(media, "text/") || media == "application/javascript" ||
		media == "application/json" || media == "application/manifest+json" || media == "image/svg+xml"
	if compressible && media != "text/event-stream" && h.Get("Content-Encoding") == "" &&
		status != http.StatusNoContent && status != http.StatusNotModified && status != http.StatusPartialContent {
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
		w.gzip = gzip.NewWriter(w.ResponseWriter)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", http.DetectContentType(p))
		}
		w.WriteHeader(http.StatusOK)
	}
	if w.gzip != nil {
		return w.gzip.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

func (w *gzipResponseWriter) FlushError() error {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if w.gzip != nil {
		if err := w.gzip.Flush(); err != nil {
			return err
		}
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}
