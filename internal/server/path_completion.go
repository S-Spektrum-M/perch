package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

type pathCompletion struct {
	Entries    []DirEntry `json:"entries"`
	Completion string     `json:"completion"`
	Error      string     `json:"error,omitempty"`
}

// completePaths completes directories only, including hidden directories when
// the final component starts with a dot. The filesystem is the same one used by /draft.
func (s *Server) completePaths(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	result := pathCompletion{Entries: []DirEntry{}}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if !strings.HasPrefix(q, "/") && q != "~" && !strings.HasPrefix(q, "~/") {
		http.Error(w, "use an absolute path or ~/", http.StatusBadRequest)
		return
	}
	expanded := q
	if strings.HasPrefix(q, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			http.Error(w, "home directory unavailable", 500)
			return
		}
		expanded = home + q[1:]
	}
	if q == "~" {
		expanded += "/"
		q += "/"
	}
	dir, prefix := filepath.Split(expanded)
	entries, _, dirErr := listDir(dir, strings.HasPrefix(prefix, "."))
	if dirErr != "" {
		result.Error = "Cannot read directory"
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name, prefix) {
			result.Entries = append(result.Entries, e)
		}
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Name < result.Entries[j].Name })
	if len(result.Entries) > 0 {
		common := result.Entries[0].Name
		for _, e := range result.Entries[1:] {
			for !strings.HasPrefix(e.Name, common) {
				_, size := utf8.DecodeLastRuneInString(common)
				common = common[:len(common)-size]
			}
		}
		result.Completion = q[:len(q)-len(prefix)] + common
		if len(result.Entries) == 1 {
			result.Completion += "/"
		}
	}
	if len(result.Entries) > 8 {
		result.Entries = result.Entries[:8]
	}
	_ = json.NewEncoder(w).Encode(result)
}
