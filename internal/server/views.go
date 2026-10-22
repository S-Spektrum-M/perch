package server

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"pircd/internal/engine"
)

// // View models. Templates render these; keep presentation decisions here so
// web/templates stays mostly markup.

// MsgView is a renderable message.
type MsgView struct {
	Role      string // css class + label: user, assistant, tool, bash, summary, custom, error
	Label     string
	Blocks    []BlockView
	Body      string     // preformatted body for non-assistant roles
	Note      string     // trailing note, e.g. "truncated" or an error
	Fold      bool       // render the body collapsed
	Diff      []DiffLine // authoritative edit result, when supplied by the tool
	Time      time.Time
	OutputURL string // download the complete saved shell log, when available
}

// BlockView is one block of an assistant message.
type BlockView struct {
	Kind    string // text, thinking, tool
	Text    string
	Summary string // tool: one-line description of the call
	Idx     int
	Diff    []DiffLine
}

const maxToolArgs = 6000

func truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	// cut on a rune boundary
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n], true
}

// ViewMessage converts a message to its view. ok is false for messages that
// should not be shown (system prompts, hidden extension messages).
func ViewMessage(m engine.Message) (v MsgView, ok bool) {
	v.Time = time.UnixMilli(m.Timestamp)
	switch m.Role {
	case "system":
		return v, false

	case "user":
		v.Role, v.Label = "user", "you"
		v.Body = m.PlainText()
		for _, b := range m.Blocks() {
			if b.Type == "image" {
				v.Note = "+ image"
			}
		}

	case "assistant":
		v.Role, v.Label = "assistant", "pi"
		for i, b := range m.Blocks() {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					v.Blocks = append(v.Blocks, BlockView{Kind: "text", Text: b.Text, Idx: i})
				}
			case "thinking":
				if b.Thinking != "" {
					v.Blocks = append(v.Blocks, BlockView{Kind: "thinking", Text: b.Thinking, Idx: i})
				}
			case "toolCall":
				v.Blocks = append(v.Blocks, BlockView{
					Kind: "tool", Idx: i,
					Summary: toolSummary(b.Name, b.Arguments),
					Text:    prettyJSON(b.Arguments),
					Diff:    editPreview(b.Name, b.Arguments),
				})
			}
		}
		switch m.StopReason {
		case "error":
			v.Note = "error: " + m.ErrorMessage
		case "aborted":
			v.Note = "aborted"
		}
		if len(v.Blocks) == 0 && v.Note == "" {
			return v, false
		}

	case "toolResult":
		v.Role, v.Label, v.Fold = "tool", m.ToolName+" result", true
		if m.IsError {
			v.Role, v.Label = "tool error", m.ToolName+" error"
		}
		v.Body, v.Note = toolDisplayOutput(m)
		v.Diff = diffLines(m.Details.Diff)

	case "bashExecution":
		v.Role, v.Label, v.Fold = "bash", "$ "+m.Command, true
		v.Body = cleanToolOutput(m.Output)
		if m.ExitCode != nil && *m.ExitCode != 0 {
			v.Note = strings.TrimSpace(fmt.Sprintf("exit %d %s", *m.ExitCode, v.Note))
		}

	case "compactionSummary", "branchSummary":
		v.Role, v.Label, v.Fold = "summary", m.Role, true
		v.Body = m.Summary

	case "custom":
		if m.Display != nil && !*m.Display {
			return v, false
		}
		v.Role, v.Label = "custom", m.CustomType
		v.Body = m.PlainText()

	default:
		v.Role, v.Label = "custom", m.Role
		v.Body = m.PlainText()
	}
	return v, true
}

// toolSummary produces a one-line description of a tool call, using the
// argument most tools are identified by.
func toolSummary(name string, raw json.RawMessage) string {
	var args map[string]any
	_ = json.Unmarshal(raw, &args)
	for _, k := range []string{"command", "path", "file_path", "pattern", "query", "url"} {
		if s, ok := args[k].(string); ok && s != "" {
			return name + " " + engine.OneLine(s, 160)
		}
	}
	if len(args) == 0 {
		return name
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return name + " (" + strings.Join(keys, ", ") + ")"
}

func prettyJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	s, _ := truncate(string(b), maxToolArgs)
	return s
}
