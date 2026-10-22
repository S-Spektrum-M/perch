package main

import (
	"bytes"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPartialUploadSaveCleanup(t *testing.T) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("files", "good.txt")
	_, _ = part.Write([]byte("saved"))
	_ = mw.Close()
	r := httptest.NewRequest("POST", "/", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	headers, err := parseUploads(httptest.NewRecorder(), r)
	if err != nil {
		t.Fatal(err)
	}
	// An unbacked header fails to open after the first file has been saved.
	headers = append(headers, &multipart.FileHeader{Filename: "missing.txt"})
	s := &Server{uploads: t.TempDir()}
	paths, err := s.saveUploads("test", headers)
	if err == nil || len(paths) != 0 {
		t.Fatalf("expected failed atomic save: %v %v", paths, err)
	}
	entries, err := os.ReadDir(filepath.Join(s.uploads, "test"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial uploads remain: %v %v", entries, err)
	}
}
