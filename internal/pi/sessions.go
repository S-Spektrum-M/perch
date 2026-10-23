package pi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"perchd/internal/engine"
)

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
	info  engine.SessionInfo
}

func NewStore(dir string) *Store {
	return &Store{dir: dir, cache: map[string]cachedInfo{}}
}

// List returns all sessions on disk, most recently modified first.
func (s *Store) List() []engine.SessionInfo {
	// pi groups sessions into per-cwd folders by default, but writes them
	// flat into an explicit --session-dir.
	files, _ := filepath.Glob(filepath.Join(s.dir, "*", "*.jsonl"))
	flat, _ := filepath.Glob(filepath.Join(s.dir, "*.jsonl"))
	files = append(files, flat...)

	s.mu.Lock()
	defer s.mu.Unlock()

	seen := make(map[string]bool, len(files))
	out := make([]engine.SessionInfo, 0, len(files))
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
func (s *Store) Find(id string) (engine.SessionInfo, bool) {
	for _, info := range s.List() {
		if info.ID == id {
			return info, true
		}
	}
	return engine.SessionInfo{}, false
}

var (
	markSessionInfo = []byte(`"type":"session_info"`)
	markUserMsg     = []byte(`"message":{"role":"user"`)
)

// readSessionInfo reads the header and skims the entries of a session file.
// Only lines that look interesting are JSON-decoded; tool output lines can be
// large and are skipped by a cheap byte search.
func readSessionInfo(path string) (engine.SessionInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return engine.SessionInfo{}, err
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 64*1024)
	info := engine.SessionInfo{File: path}

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
					return engine.SessionInfo{}, io.ErrUnexpectedEOF
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
						Message engine.Message `json:"message"`
					}
					if json.Unmarshal(line, &e) == nil {
						info.FirstMsg = engine.OneLine(e.Message.PlainText(), 140)
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	if info.ID == "" {
		return engine.SessionInfo{}, io.ErrUnexpectedEOF
	}
	return info, nil
}
