package main

import (
	"encoding/json"
	"testing"
)

type metricsRenderer struct{}

func (metricsRenderer) Fragment(string, any) string { return "" }

func TestTaskTokenTotals(t *testing.T) {
	a := &Agent{render: metricsRenderer{}, streaming: true}
	line := []byte(`{"message":{"role":"assistant","content":[{"type":"text","text":"Done"}],"usage":{"input":10,"output":7,"cacheRead":20,"cacheWrite":30}}}`)
	a.handleEvent("message_end", line)
	a.handleEvent("message_end", line)
	if got := a.statusLocked().Tokens; got != "" {
		t.Fatalf("counts visible during task: %q", got)
	}
	a.streaming = false
	a.taskTime = "2s"
	if got := a.statusLocked().Tokens; got != "Tokens read: 120 · written: 14" {
		t.Fatalf("unexpected totals: %q", got)
	}
	var e struct {
		Message Message `json:"message"`
	}
	if err := json.Unmarshal(line, &e); err != nil {
		t.Fatal(err)
	}
	if e.Message.Usage.Output != 7 {
		t.Fatal("usage not decoded")
	}
}
