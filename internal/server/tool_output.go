package server

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"pircd/internal/pi"
)

// Keep inline logs bounded, but always offer the original file for larger logs.
const maxInlineToolOutput = 4 * 1024 * 1024

var ansiOutput = regexp.MustCompile("\x1b(?:\\[[0-?]*[ -/]*[@-~]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\))")

func cleanToolOutput(text string) string {
	return ansiOutput.ReplaceAllString(text, "")
}

// Only pi's saved bash logs are eligible. Never accept a client-supplied path,
// or follow a tool-supplied symlink to an unrelated file.
func shellOutputPath(m pi.Message) string {
	path := m.Details.FullOutputPath
	base := filepath.Base(path)
	if m.ToolName != "bash" || !filepath.IsAbs(path) || filepath.Dir(path) != filepath.Clean(os.TempDir()) || !strings.HasPrefix(base, "pi-bash-") || !strings.HasSuffix(base, ".log") {
		return ""
	}
	return path
}

func openShellOutput(m pi.Message) (*os.File, error) {
	path := shellOutputPath(m)
	if path == "" {
		return nil, os.ErrNotExist
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return nil, os.ErrNotExist
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		f.Close()
		return nil, os.ErrNotExist
	}
	return f, nil
}

// toolDisplayOutput prefers the saved log to pi's rolling, truncated snapshot.
// This also reconstructs complete results from session history after a restart.
func toolDisplayOutput(m pi.Message) (text, note string) {
	text = toolResultText(m)
	if shellOutputPath(m) != "" {
		f, err := openShellOutput(m)
		if err == nil {
			data, readErr := io.ReadAll(io.LimitReader(f, maxInlineToolOutput+1))
			f.Close()
			if readErr == nil {
				var cut bool
				text, cut = truncate(string(data), maxInlineToolOutput)
				if cut {
					note = "preview limited to 4 MiB; download full output"
				} else {
					// pi appends failure diagnostics after the captured shell log.
					result := toolResultText(m)
					if i := strings.LastIndex(result, "\n\nCommand "); m.IsError && i >= 0 {
						text += result[i:]
					}
				}
			} else {
				note = "saved full output could not be read; showing tool result"
			}
		} else {
			note = "saved full output unavailable; showing tool result"
		}
	}
	return cleanToolOutput(text), note
}

func (a *Agent) viewMessageLocked(m pi.Message) (MsgView, bool) {
	v, ok := ViewMessage(m)
	if ok && shellOutputPath(m) != "" {
		v.OutputURL = "/s/" + url.PathEscape(a.ID) + "/tool-output/" + toolOutputID(m.ToolCallID)
	}
	return v, ok
}

// toolOutput serves only a log referenced by this session's completed messages.
// The URL contains a tool ID, never a path, and large downloads aren't read into
// memory or held under the agent lock.
func (s *Server) toolOutput(w http.ResponseWriter, r *http.Request) {
	a, err := s.agent(r, true)
	if err != nil || a == nil {
		http.NotFound(w, r)
		return
	}
	var message pi.Message
	a.mu.Lock()
	for _, m := range a.messages {
		if m.Role == "toolResult" && toolOutputID(m.ToolCallID) == r.PathValue("tool") {
			message = m
			break
		}
	}
	a.mu.Unlock()
	f, err := openShellOutput(message)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "could not read saved output", http.StatusInternalServerError)
		}
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "could not read saved output", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="tool-output.log"`)
	http.ServeContent(w, r, "tool-output.log", st.ModTime(), f)
}
