package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// SessionInfo is the metadata shown in the session list. It is derived from a
// session JSONL file on disk, or from a live agent whose file does not exist
// yet (pi only writes the file after the first prompt).
type SessionInfo struct {
	ID       string
	File     string
	Cwd      string
	Name     string
	FirstMsg string
	Created  time.Time
	Modified time.Time
	Prompts  int // number of user messages
}

// Title is the best human label for a session.
func (s SessionInfo) Title() string {
	switch {
	case s.Name != "":
		return s.Name
	case s.FirstMsg != "":
		return s.FirstMsg
	default:
		return "(empty session)"
	}
}

// Store scans pi's session directory. Parsed files are cached by size and
// mtime, so listing stays cheap after the first scan.
type Store struct {
	dir string

	mu    sync.Mutex
	cache map[string]cachedInfo
}

type cachedInfo struct {
	size  int64
	mtime time.Time
	info  SessionInfo
}

func NewStore(dir string) *Store {
	return &Store{dir: dir, cache: map[string]cachedInfo{}}
}

// List returns all sessions on disk, most recently modified first.
func (s *Store) List() []SessionInfo {
	// pi groups sessions into per-cwd folders by default, but writes them
	// flat into an explicit --session-dir.
	files, _ := filepath.Glob(filepath.Join(s.dir, "*", "*.jsonl"))
	flat, _ := filepath.Glob(filepath.Join(s.dir, "*.jsonl"))
	files = append(files, flat...)

	s.mu.Lock()
	defer s.mu.Unlock()

	seen := make(map[string]bool, len(files))
	out := make([]SessionInfo, 0, len(files))
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			continue
		}
		seen[f] = true
		c, ok := s.cache[f]
		if !ok || c.size != st.Size() || !c.mtime.Equal(st.ModTime()) {
			info, err := readSessionInfo(f)
			if err != nil {
				continue
			}
			info.Modified = st.ModTime()
			c = cachedInfo{size: st.Size(), mtime: st.ModTime(), info: info}
			s.cache[f] = c
		}
		out = append(out, c.info)
	}
	for f := range s.cache {
		if !seen[f] {
			delete(s.cache, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out
}

// Find looks a session up by its ID.
func (s *Store) Find(id string) (SessionInfo, bool) {
	for _, info := range s.List() {
		if info.ID == id {
			return info, true
		}
	}
	return SessionInfo{}, false
}

var (
	markSessionInfo = []byte(`"type":"session_info"`)
	markUserMsg     = []byte(`"message":{"role":"user"`)
)

// readSessionInfo reads the header and skims the entries of a session file.
// Only lines that look interesting are JSON-decoded; tool output lines can be
// large and are skipped by a cheap byte search.
func readSessionInfo(path string) (SessionInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return SessionInfo{}, err
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 64*1024)
	info := SessionInfo{File: path}

	first := true
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if first {
				first = false
				var h struct {
					Type      string    `json:"type"`
					ID        string    `json:"id"`
					Cwd       string    `json:"cwd"`
					Timestamp time.Time `json:"timestamp"`
				}
				if json.Unmarshal(line, &h) != nil || h.Type != "session" {
					return SessionInfo{}, io.ErrUnexpectedEOF
				}
				info.ID, info.Cwd, info.Created = h.ID, h.Cwd, h.Timestamp
			} else if bytes.Contains(line, markSessionInfo) {
				var e struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(line, &e) == nil {
					info.Name = e.Name
				}
			} else if bytes.Contains(line, markUserMsg) {
				info.Prompts++
				if info.FirstMsg == "" {
					var e struct {
						Message Message `json:"message"`
					}
					if json.Unmarshal(line, &e) == nil {
						info.FirstMsg = oneLine(e.Message.PlainText(), 140)
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	if info.ID == "" {
		return SessionInfo{}, io.ErrUnexpectedEOF
	}
	return info, nil
}

// oneLine collapses whitespace and truncates to n runes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
