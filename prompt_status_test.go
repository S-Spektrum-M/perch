package main

import (
	"html"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestPromptStatus(t *testing.T) {
	templates := loadTemplates()
	for _, tt := range []struct {
		name  string
		view  statusView
		state string
		label string
	}{
		{"idle", statusView{}, "idle", "idle"},
		{"working", statusView{Busy: true}, "busy", "working"},
		{"responding", statusView{Busy: true, Streaming: true, Activity: "responding"}, "responding", "responding"},
		{"tool", statusView{Busy: true, Activity: "running bash: ls"}, "busy", "running bash: ls"},
		{"shell", statusView{Busy: true, Shell: true, Activity: "$ echo <hello>"}, "busy", "$ echo <hello>"},
		{"compacting", statusView{Busy: true, Activity: "compacting context"}, "busy", "compacting context"},
		{"retrying", statusView{Busy: true, Activity: "retrying (1/3): busy"}, "busy", "retrying (1/3): busy"},
		{"stale activity", statusView{Activity: "responding"}, "idle", "idle"},
		{"exited", statusView{Exited: true}, "exited", "pi exited"},
		{"exited while busy", statusView{Exited: true, Busy: true, Activity: "responding", ExitErr: "exit status 1"}, "exited", "exit status 1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.view.PromptState(); got != tt.state {
				t.Fatalf("state = %q, want %q", got, tt.state)
			}
			if got := tt.view.PromptLabel(); got != tt.label {
				t.Fatalf("label = %q, want %q", got, tt.label)
			}
			fragment := templates.Fragment("status", tt.view)
			for _, want := range []string{
				`data-prompt-state="` + tt.state + `"`,
				`id="prompt-status" class="sr-only" role="status">` + html.EscapeString(tt.label) + `</span>`,
			} {
				if !strings.Contains(fragment, want) {
					t.Fatalf("status missing %q: %s", want, fragment)
				}
			}
			for _, removed := range []string{`class="idle"`, `class="activity"`, `class="spinner"`} {
				if strings.Contains(fragment, removed) {
					t.Fatalf("status still contains %s", removed)
				}
			}
			if strings.Contains(fragment, "data-stop-shell") != (tt.view.Shell && !tt.view.Streaming && !tt.view.Exited) {
				t.Fatalf("shell stop control incorrect: %s", fragment)
			}
			if strings.Contains(fragment, "data-model-picker") {
				t.Fatal("model control must live in the prompt, not the status row")
			}
			if !strings.Contains(fragment, `data-model-disabled="`+strconv.FormatBool(tt.view.Exited)+`"`) {
				t.Fatal("status must keep the inline model control's disabled state current")
			}
			if tt.view.Exited && !strings.Contains(fragment, `>restart</a>`) {
				t.Fatalf("exit restart link missing: %s", fragment)
			}

			page := httptest.NewRecorder()
			templates.Page(page, 200, "session", map[string]any{"ID": "test", "Cwd": "/tmp", "Epoch": "test", "Seq": uint64(0), "Status": tt.view})
			htmlPage := page.Body.String()
			formStart := strings.Index(htmlPage, `<form id="promptform"`)
			control := strings.Index(htmlPage, "data-model-picker")
			formEnd := formStart + strings.Index(htmlPage[formStart:], "</form>")
			if control < formStart || control > formEnd || strings.Count(htmlPage, "data-model-picker") != 1 {
				t.Fatal("expected exactly one model control inside the prompt form")
			}
			if !strings.Contains(htmlPage, `title="`+html.EscapeString(tt.label)+`"`) ||
				!strings.Contains(page.Body.String(), `aria-describedby="prompt-status"`) {
				t.Fatal("initial page is missing the prompt activity description")
			}
		})
	}
}
