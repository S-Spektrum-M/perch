package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"perchd/internal/engine"
)

func toolEvent(t *testing.T, a *Agent, typ, id, output string) {
	t.Helper()
	e := map[string]any{
		"type": typ, "toolCallId": id, "toolName": "bash",
		"args": map[string]string{"command": "test"},
	}
	if typ == "tool_execution_update" {
		e["partialResult"] = map[string]any{"content": []engine.Block{{Type: "text", Text: output}}}
	}
	if typ == "message_end" {
		e["message"] = map[string]any{
			"role": "toolResult", "toolCallId": id, "toolName": "bash",
			"content": []engine.Block{{Type: "text", Text: output}},
		}
	}
	line, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	piFeed(a, line)
}

func numberedToolLines(start, end int) string {
	var b strings.Builder
	for i := start; i <= end; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

func toolDeltas(a *Agent) []UIEvent {
	var deltas []UIEvent
	for _, ev := range a.ring {
		if ev.T == "tool_delta" {
			deltas = append(deltas, ev)
		}
	}
	return deltas
}

func TestToolStreamChunksAndCompletion(t *testing.T) {
	a := &Agent{render: loadTemplates()}
	toolEvent(t, a, "tool_execution_start", "call-1", "")
	toolEvent(t, a, "tool_execution_update", "call-1", numberedToolLines(1, 99))
	if got := toolDeltas(a); len(got) != 0 {
		t.Fatalf("published fewer than 100 lines: %+v", got)
	}
	toolEvent(t, a, "tool_execution_update", "call-1", numberedToolLines(1, 100)+"half")
	toolEvent(t, a, "tool_execution_update", "call-1", numberedToolLines(1, 250)+"last line")
	// Identical cumulative updates must not duplicate anything.
	toolEvent(t, a, "tool_execution_update", "call-1", numberedToolLines(1, 250)+"last line")
	deltas := toolDeltas(a)
	if len(deltas) != 2 || deltas[0].S != numberedToolLines(1, 100) || deltas[1].S != numberedToolLines(101, 200) {
		t.Fatalf("unexpected chunks: %+v", deltas)
	}
	toolEvent(t, a, "tool_execution_end", "call-1", "")
	deltas = toolDeltas(a)
	if len(deltas) != 3 || deltas[2].S != numberedToolLines(201, 250)+"last line" {
		t.Fatalf("completion did not flush remainder: %+v", deltas)
	}
	toolEvent(t, a, "message_end", "call-1", "authoritative result")
	last := a.ring[len(a.ring)-1]
	if last.T != "append" || last.ID != toolOutputID("call-1") || !strings.Contains(last.HTML, "authoritative result") {
		t.Fatalf("final result does not replace live output: %+v", last)
	}
	if len(a.tools) != 0 || len(a.messages) != 1 || len(a.Snapshot(0).Tools) != 0 || len(toolDeltas(a)) != 3 {
		t.Fatal("live tool retained, transcript duplicated, or remainder sent twice")
	}
}

func TestToolStreamSingleLargeUpdate(t *testing.T) {
	a := &Agent{render: metricsRenderer{}}
	// Accept updates even if no start event was observed.
	toolEvent(t, a, "tool_execution_update", "large", numberedToolLines(1, 1000))
	deltas := toolDeltas(a)
	if len(deltas) != 10 {
		t.Fatalf("got %d chunks, want 10", len(deltas))
	}
	for i, ev := range deltas {
		if ev.ID != toolOutputID("large") || ev.S != numberedToolLines(i*100+1, (i+1)*100) {
			t.Fatalf("incorrect chunk %d: %+v", i, ev)
		}
	}
}

func TestToolStreamRevisedSnapshots(t *testing.T) {
	a := &Agent{render: metricsRenderer{}}
	toolEvent(t, a, "tool_execution_update", "rolling", numberedToolLines(1, 150))
	cursor := a.seq
	// Model pi's rolling tail after it reaches its output limit.
	toolEvent(t, a, "tool_execution_update", "rolling", numberedToolLines(51, 200))
	evs, reset := a.EventsSince(cursor)
	if reset || len(evs) != 2 || evs[0].T != "tool_reset" || evs[1].T != "tool_delta" || evs[1].S != numberedToolLines(51, 150) {
		t.Fatalf("snapshot revision: %+v, reset=%v", evs, reset)
	}
	if a.Snapshot(0).Tools[0].Text != numberedToolLines(51, 150) {
		t.Fatal("snapshot contains stale output or unpublished tail")
	}
	toolEvent(t, a, "tool_execution_update", "rolling", "replacement")
	if a.Snapshot(0).Tools[0].Text != "" {
		t.Fatal("short replacement did not clear old output")
	}
	toolEvent(t, a, "tool_execution_end", "rolling", "")
	if a.Snapshot(0).Tools[0].Text != "replacement" {
		t.Fatal("replacement tail not flushed")
	}
}

func TestToolStreamSnapshotAndReplay(t *testing.T) {
	templates := loadTemplates()
	a := &Agent{render: templates}
	toolEvent(t, a, "tool_execution_update", "one", numberedToolLines(1, 150))
	snapshot := a.Snapshot(0)
	toolEvent(t, a, "tool_execution_update", "one", numberedToolLines(1, 200))
	if len(snapshot.Tools) != 1 || snapshot.Tools[0].Text != numberedToolLines(1, 100) {
		t.Fatal("snapshot mutated after subsequent update")
	}
	evs, reset := a.EventsSince(snapshot.Seq)
	if reset || len(evs) != 1 || evs[0].S != numberedToolLines(101, 200) {
		t.Fatalf("incorrect replay after snapshot: %+v, reset=%v", evs, reset)
	}
	var page bytes.Buffer
	if err := templates.pages["session"].ExecuteTemplate(&page, "content", struct {
		Snapshot
		projectTree
		EarlierLimit int
	}{Snapshot: snapshot}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.String(), `id="`+toolOutputID("one")+`"`) || !strings.Contains(page.String(), numberedToolLines(1, 100)) {
		t.Fatal("page reload does not render published tool output")
	}
}

func TestToolStreamIndependentCalls(t *testing.T) {
	a := &Agent{render: metricsRenderer{}}
	toolEvent(t, a, "tool_execution_start", "one", "")
	toolEvent(t, a, "tool_execution_start", "two", "")
	toolEvent(t, a, "tool_execution_update", "one", numberedToolLines(1, 100))
	toolEvent(t, a, "tool_execution_update", "two", "short\n")
	toolEvent(t, a, "message_end", "two", "short\n") // also flush without end event
	if len(a.tools) != 1 || a.tools[0].ID != toolOutputID("one") {
		t.Fatal("completion removed the wrong tool")
	}
	deltas := toolDeltas(a)
	if len(deltas) != 2 || deltas[0].ID != toolOutputID("one") || deltas[1].ID != toolOutputID("two") || deltas[1].S != "short\n" {
		t.Fatalf("calls mixed: %+v", deltas)
	}
}

func TestToolStreamFlushOnSettleOrExit(t *testing.T) {
	for _, exit := range []bool{false, true} {
		t.Run(fmt.Sprintf("exit=%v", exit), func(t *testing.T) {
			a := &Agent{render: metricsRenderer{}, done: make(chan struct{})}
			toolEvent(t, a, "tool_execution_update", "one", "unterminated é\r\nlast")
			if exit {
				a.Stopped("pi exited")
			} else {
				piEvent(a, "agent_settled", []byte(`{"aborted":true}`))
			}
			deltas := toolDeltas(a)
			if len(deltas) != 1 || deltas[0].S != "unterminated é\r\nlast" {
				t.Fatalf("tail lost on settle/exit: %+v", deltas)
			}
		})
	}
}

func TestToolStreamEscapingAndEmptyUpdates(t *testing.T) {
	a := &Agent{render: loadTemplates()}
	id := `call-"/><script>alert(1)</script>`
	toolEvent(t, a, "tool_execution_start", id, "")
	toolEvent(t, a, "tool_execution_start", id, "") // no duplicate placeholder
	toolEvent(t, a, "tool_execution_update", id, "")
	if len(a.tools) != 1 || len(toolDeltas(a)) != 0 {
		t.Fatal("duplicate placeholder or empty chunk")
	}
	toolEvent(t, a, "tool_execution_update", id, "<script>alert(1)</script>\n")
	toolEvent(t, a, "tool_execution_end", id, "")
	tool := a.Snapshot(0).Tools[0]
	html := a.render.Fragment("tool_live", tool)
	if strings.Contains(html, "<script>") || !strings.Contains(html, "&lt;script&gt;") || strings.Contains(tool.ID, `"`) {
		t.Fatalf("unescaped tool output: %s", html)
	}
}

func TestToolStreamPreservesLineEndingsAndContentBlocks(t *testing.T) {
	a := &Agent{render: metricsRenderer{}}
	output := strings.Repeat("é\u2028x\r\n", toolChunkLines)
	toolEvent(t, a, "tool_execution_update", "unicode", output)
	deltas := toolDeltas(a)
	if len(deltas) != 1 || deltas[0].S != output {
		t.Fatalf("line endings or Unicode changed: %+v", deltas)
	}
	result := engine.Message{Content: json.RawMessage(`[{"type":"text","text":"first"},{"type":"image","mimeType":"image/png"},{"type":"text","text":"last"}]`)}
	if got := toolResultText(result); got != "first\n[image image/png]\nlast" {
		t.Fatalf("content blocks not preserved: %q", got)
	}
}

func TestToolStreamIgnoresNestedAndMalformedEvents(t *testing.T) {
	a := &Agent{render: metricsRenderer{}}
	piFeed(a, []byte(`{"type":"tool_execution_start","toolCallId":"nested","parentToolCallId":"parent","toolName":"bash"}`))
	piEvent(a, "tool_execution_update", []byte(`{"toolCallId":123}`))
	toolEvent(t, a, "tool_execution_update", "", "ignored\n")
	if len(a.tools) != 0 || len(toolDeltas(a)) != 0 {
		t.Fatal("invalid or nested event created a live tool")
	}
}
