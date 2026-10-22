package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Files attached to a prompt (the "+" pill, drag and drop, or paste) are saved
// under <uploads>/<session id>/ and the prompt gets their paths appended, so
// pi reads them with its own tools. pi's read tool handles images too.

const maxUploadBytes = 200 << 20 // per prompt, all files together

func defaultUploadsDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "pircd", "uploads")
}

// parseUploads parses a multipart prompt with a size limit and returns its
// "files" parts. It must run before any other form access.
func parseUploads(w http.ResponseWriter, r *http.Request) ([]*multipart.FileHeader, error) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return nil, nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, fmt.Errorf("attachments are limited to %d MB per message", maxUploadBytes>>20)
		}
		return nil, err
	}
	var files []*multipart.FileHeader
	for _, fh := range r.MultipartForm.File["files"] {
		// An empty file input still submits one nameless, empty part.
		if fh.Filename != "" || fh.Size > 0 {
			files = append(files, fh)
		}
	}
	return files, nil
}

// saveUploads stores files under the session's upload directory and returns
// their paths.
func (s *Server) saveUploads(session string, headers []*multipart.FileHeader) ([]string, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	dir := filepath.Join(s.uploads, filepath.Base(session))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var paths []string
	for _, fh := range headers {
		p, err := saveUpload(dir, fh)
		if err != nil {
			return paths, fmt.Errorf("saving %s: %w", fh.Filename, err)
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// saveUpload writes one file into dir, adding -1, -2… before the extension
// rather than overwriting an earlier upload with the same name.
func saveUpload(dir string, fh *multipart.FileHeader) (string, error) {
	name := filepath.Base(strings.ReplaceAll(fh.Filename, "\\", "/"))
	if name == "." || name == "/" || strings.HasPrefix(name, ".") {
		name = "upload" + name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	src, err := fh.Open()
	if err != nil {
		return "", err
	}
	defer src.Close()
	for i := 0; ; i++ {
		p := filepath.Join(dir, name)
		if i > 0 {
			p = filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
		}
		dst, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = io.Copy(dst, src)
		if cerr := dst.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(p)
			return "", err
		}
		return p, nil
	}
}

// withAttachments appends the saved files' paths to a prompt.
func withAttachments(msg string, paths []string) string {
	if len(paths) == 0 {
		return msg
	}
	var b strings.Builder
	if msg != "" {
		b.WriteString(msg + "\n\n")
	}
	b.WriteString("Attached files:")
	for _, p := range paths {
		b.WriteString("\n- " + p)
	}
	return b.String()
}

// cleanUploadsLoop removes old sessions' uploads now and then hourly, until
// ctx is done. retention <= 0 disables it.
func (s *Server) cleanUploadsLoop(ctx context.Context, retention time.Duration) {
	if retention <= 0 {
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		s.cleanUploads(retention, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// cleanUploads deletes the upload directories of sessions unused for longer
// than retention: neither the session file nor its uploads have changed since,
// and no pi is running for it. Directories whose session no longer exists age
// out by their uploads alone.
func (s *Server) cleanUploads(retention time.Duration, now time.Time) {
	ents, err := os.ReadDir(s.uploads)
	if err != nil {
		return // nothing uploaded yet
	}
	modified := map[string]time.Time{}
	for _, info := range s.store.List() {
		modified[info.ID] = info.Modified
	}
	for _, a := range s.mgr.All() {
		info, _ := a.Info() // Modified is now: running sessions are in use
		modified[info.ID] = info.Modified
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(s.uploads, e.Name())
		last := modified[e.Name()]
		if t := newestMod(dir); t.After(last) {
			last = t
		}
		if now.Sub(last) <= retention {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("uploads: removing %s: %v", dir, err)
		} else {
			log.Printf("uploads: removed %s (unused since %s)", dir, last.Format(time.DateOnly))
		}
	}
}

// newestMod returns the latest modification time of dir and its files.
func newestMod(dir string) time.Time {
	var newest time.Time
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest
}
