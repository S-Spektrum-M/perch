package server

import (
	"strings"
	"testing"
	"time"

	"perchd/internal/engine"
)

func TestQuestionNotification(t *testing.T) {
	for _, method := range []string{"select", "confirm", "input", "editor"} {
		for _, name := range []string{"", "Named session"} {
			t.Run(method+"/"+name, func(t *testing.T) {
				calls := make(chan string, 4)
				a := newAgent("/tmp/project", loadTemplates(), nil)
				a.ID, a.name = "abc", name
				a.notifyQuestion = func(id, label string) { calls <- id + ":" + label }
				d := engine.Dialog{ID: "question-1", Method: method, Title: "Private question", Options: []string{"Private choice"}}
				a.DialogOpened(d)
				a.DialogOpened(d) // duplicate delivery must not notify twice
				want := "abc:" + cmp(name, "project")
				select {
				case got := <-calls:
					if got != want {
						t.Fatalf("notification = %q, want %q", got, want)
					}
				case <-time.After(time.Second):
					t.Fatal("missing question notification")
				}
				a.DialogClosed(d.ID)
				a.RunSettled()
				select {
				case got := <-calls:
					t.Fatalf("unexpected notification: %s", got)
				case <-time.After(20 * time.Millisecond):
				}
			})
		}
	}
}

func TestQuestionNotificationDisabled(t *testing.T) {
	a := newAgent("/tmp/project", loadTemplates(), nil)
	a.DialogOpened(engine.Dialog{ID: "question-1", Method: "input"})
	if len(a.dialogs) != 1 {
		t.Fatal("dialog should still open without notifications")
	}
}

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
