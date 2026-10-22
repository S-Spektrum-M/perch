package main

import (
	"errors"
	"mime"
	"net/http"
	"strings"
	"time"
)

// browserHeaders apply to successful responses and rejections alike.
func browserHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

const maxFormBytes = 1 << 20

// requestBodies bounds parsing before handlers can silently ignore form errors.
func requestBodies(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			http.Error(w, "invalid content type", 400)
			return
		}
		limit := int64(maxFormBytes)
		multipart := media == "multipart/form-data"
		if multipart && strings.HasSuffix(r.URL.Path, "/prompt") {
			limit = maxUploadBytes
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		if multipart {
			err = r.ParseMultipartForm(32 << 20)
			defer func() {
				if r.MultipartForm != nil {
					_ = r.MultipartForm.RemoveAll()
				}
			}()
		} else {
			err = r.ParseForm()
		}
		if err != nil {
			status := http.StatusBadRequest
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, http.StatusText(status), status)
			return
		}
		if multipart && r.MultipartForm != nil {
			count := 0
			for _, files := range r.MultipartForm.File {
				count += len(files)
			}
			if count > 32 {
				http.Error(w, "at most 32 attachments per prompt", 400)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// bodyDeadlines bound stalled request bodies without limiting response streams.
func bodyDeadlines(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			timeout := 30 * time.Second
			if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
				timeout = 5 * time.Minute
			}
			rc := http.NewResponseController(w)
			_ = rc.SetReadDeadline(time.Now().Add(timeout))
			defer rc.SetReadDeadline(time.Time{})
		}
		next.ServeHTTP(w, r)
	})
}
