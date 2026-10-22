package server

import (
	"encoding/hex"
	"encoding/json"
	"strings"

	"pircd/internal/engine"
)

const toolChunkLines = 100

// liveTool mirrors one tool's published output. output also includes the tail
// waiting for a full chunk; Text contains exactly what browsers have received.
// Partial results are replaceable snapshots, as used by pi's built-in shell
// tools, not deltas. In particular, a truncated snapshot can drop older lines.
type liveTool struct {
	ID      string
	Summary string
	Text    string
	output  string
}

func toolOutputID(callID string) string {
	// Tool IDs may contain arbitrary characters. Use a stable, DOM-safe ID.
	return "tool-" + hex.EncodeToString([]byte(callID))
}

func (a *Agent) toolLocked(callID string) *liveTool {
	id := toolOutputID(callID)
	for _, t := range a.tools {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (a *Agent) startToolLocked(callID, name string, args json.RawMessage) *liveTool {
	if callID == "" {
		return nil
	}
	if t := a.toolLocked(callID); t != nil {
		return t
	}
	t := &liveTool{ID: toolOutputID(callID), Summary: toolSummary(name, args)}
	a.tools = append(a.tools, t)
	a.emitLocked(UIEvent{T: "append", ID: t.ID, HTML: a.render.Fragment("tool_live", t)})
	return t
}

func (a *Agent) removeToolLocked(callID string) {
	id := toolOutputID(callID)
	for i, t := range a.tools {
		if t.ID == id {
			a.tools = append(a.tools[:i], a.tools[i+1:]...)
			return
		}
	}
}

func toolResultText(result engine.Message) string {
	var parts []string
	for _, b := range result.Blocks() {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			parts = append(parts, "[image "+b.MimeType+"]")
		}
	}
	return strings.Join(parts, "\n")
}

// publishToolLocked emits complete 100-line chunks. A completion, cancellation,
// or process exit also flushes the remaining lines and any unterminated line.
func (a *Agent) publishToolLocked(t *liveTool, output string, flush bool) {
	t.output = output
	if !strings.HasPrefix(output, t.Text) {
		// Tools can revise results or send a rolling tail after truncation.
		// Clear the previous snapshot rather than append duplicate/stale output.
		t.Text = ""
		a.emitLocked(UIEvent{T: "tool_reset", ID: t.ID})
	}
	pending := output[len(t.Text):]
	start, lines := 0, 0
	for i := 0; i < len(pending); i++ {
		if pending[i] != '\n' {
			continue
		}
		lines++
		if lines == toolChunkLines {
			a.emitToolChunkLocked(t, pending[start:i+1])
			start, lines = i+1, 0
		}
	}
	if flush && start < len(pending) {
		a.emitToolChunkLocked(t, pending[start:])
	}
}

func (a *Agent) emitToolChunkLocked(t *liveTool, chunk string) {
	t.Text += chunk
	a.emitLocked(UIEvent{T: "tool_delta", ID: t.ID, S: chunk})
}

func (a *Agent) flushToolsLocked() {
	for _, t := range a.tools {
		a.publishToolLocked(t, t.output, true)
	}
}
