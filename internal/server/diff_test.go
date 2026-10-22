package server

import (
	"encoding/json"
	"strings"
	"testing"

	"pircd/internal/pi"
)

func TestEditPreview(t *testing.T) {
	for _, args := range []string{
		`{"oldText":"a\nb\nc\n","newText":"a\nB\nc\n"}`,
		`{"edits":[{"oldText":"a\nb\nc\n","newText":"a\nB\nc\n"}]}`,
	} {
		lines := editPreview("edit", json.RawMessage(args))
		var added, removed bool
		for _, line := range lines {
			added = added || line.Kind == "add" && line.Text == "+B"
			removed = removed || line.Kind == "remove" && line.Text == "-b"
		}
		if !added || !removed {
			t.Fatalf("missing replacement: %#v", lines)
		}
	}
	if lines := editPreview("write", json.RawMessage(`{"oldText":"a","newText":"b"}`)); lines != nil {
		t.Fatal("non-edit preview")
	}
	if lines := editPreview("edit", json.RawMessage(`{"oldText":"same","newText":"same"}`)); len(lines) != 0 {
		t.Fatal("unchanged preview")
	}
}

func TestToolResultDiff(t *testing.T) {
	var message pi.Message
	if err := json.Unmarshal([]byte(`{"role":"toolResult","toolName":"edit","content":[{"type":"text","text":"Successfully replaced text"}],"details":{"diff":"-12 old\n+12 <script>alert(1)</script>\n 13 unchanged"}}`), &message); err != nil {
		t.Fatal(err)
	}
	view, ok := ViewMessage(message)
	if !ok || len(view.Diff) != 3 {
		t.Fatalf("missing result diff: %#v", view)
	}
	html := loadTemplates().Fragment("msg", view)
	if strings.Contains(html, "<script>") || !strings.Contains(html, "&lt;script&gt;") {
		t.Fatal("diff not escaped")
	}
	if !strings.Contains(html, `diff-line add`) || !strings.Contains(html, `diff-line remove`) {
		t.Fatal("missing diff styles")
	}
	if !strings.Contains(html, "Successfully replaced text") {
		t.Fatal("result body lost")
	}
}

func TestAssistantEditPreview(t *testing.T) {
	message := pi.Message{Role: "assistant", Content: json.RawMessage(`[{"type":"toolCall","name":"edit","arguments":{"path":"a.txt","oldText":"old","newText":"new"}}]`)}
	view, _ := ViewMessage(message)
	html := loadTemplates().Fragment("msg", view)
	if !strings.Contains(html, "Requested edit") || !strings.Contains(html, "&#43;new") {
		t.Fatalf("missing assistant edit preview: %s", html)
	}
}
