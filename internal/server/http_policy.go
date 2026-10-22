package server

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
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Preserve Origin on native same-origin POSTs. no-referrer makes
		// browsers send Origin: null, which correctly fails CSRF checks.
		w.Header().Set("Referrer-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
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
		if r.Header.Get("Content-Type") == "" && (r.Body == http.NoBody || r.ContentLength == 0) {
			next.ServeHTTP(w, r)
			return
		}
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			http.Error(w, "invalid content type", 400)
			return
		}
		if media != "multipart/form-data" && media != "application/x-www-form-urlencoded" {
			http.Error(w, "expected a form request", http.StatusUnsupportedMediaType)
			return
		}
		limit := int64(maxFormBytes)
		multipart := media == "multipart/form-data"
		if multipart && strings.HasSuffix(r.URL.Path, "/prompt") {
			limit = maxUploadBytes
		}
		if r.ContentLength > limit {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
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
			fieldBytes := 0
			for key, values := range r.MultipartForm.Value {
				fieldBytes += len(key)
				for _, value := range values {
					fieldBytes += len(value)
				}
			}
			if fieldBytes > maxFormBytes {
				http.Error(w, "form fields exceed 1 MiB", http.StatusRequestEntityTooLarge)
				return
			}
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
