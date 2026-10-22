package server

import (
	"strings"
	"testing"
	"time"
)

// A settled event must not notify on attachment or duplicate delivery.
func TestDoneNotification(t *testing.T) {
	calls := make(chan string, 4)
	a := &Agent{ID: "abc", Cwd: "/tmp/project", render: loadTemplates(), subs: map[chan struct{}]struct{}{}, notify: func(id, label string) { calls <- id + ":" + label }}
	piEvent(a, "agent_settled", nil)
	piEvent(a, "agent_start", nil)
	piEvent(a, "tool_execution_end", nil)
	piEvent(a, "agent_settled", nil)
	piEvent(a, "agent_settled", nil)
	select {
	case got := <-calls:
		if !strings.HasSuffix(got, ":project") {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("missing completion")
	}
	select {
	case got := <-calls:
		t.Fatalf("duplicate notification: %s", got)
	case <-time.After(20 * time.Millisecond):
	}
}
