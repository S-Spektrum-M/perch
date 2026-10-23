package engine

import (
	"strings"
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
	Prompts  int    // number of user messages
	Engine   string // name of the backend that owns the session; set by perchd
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

// OneLine collapses whitespace and truncates to n runes.
func OneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
