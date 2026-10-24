package server

import (
	"context"
	"log"
	"strings"
	"time"

	"perchd/internal/engine"
)

// suggestName starts at most one background request for an unnamed session.
// Failed suggestions can be retried on the next prompt. Attachments and shell
// commands are not sent to the naming model.
func (a *Agent) suggestName(namer engine.SessionNamer, prompt string) {
	if namer == nil || strings.TrimSpace(prompt) == "" {
		return
	}
	a.mu.Lock()
	if a.name != "" || a.naming || a.exited {
		a.mu.Unlock()
		return
	}
	a.naming = true
	version := a.nameVersion
	a.mu.Unlock()
	go func() {
		defer func() {
			a.mu.Lock()
			a.naming = false
			a.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		// Release/restart must not leave an orphaned naming process.
		go func() {
			select {
			case <-a.done:
				cancel()
			case <-ctx.Done():
			}
		}()
		name, err := namer.SuggestSessionName(ctx, a.Cwd, prompt)
		if err != nil {
			log.Printf("agent %s: session name suggestion failed: %v", a.ID, err)
			return
		}
		name = strings.TrimSpace(name)
		if name == "" || ctx.Err() != nil {
			return
		}
		a.nameMu.Lock()
		defer a.nameMu.Unlock()
		a.mu.Lock()
		apply := a.name == "" && a.nameVersion == version && !a.exited
		a.mu.Unlock()
		if !apply {
			return
		}
		if err := a.engine.SetName(ctx, name); err != nil {
			log.Printf("agent %s: saving suggested session name failed: %v", a.ID, err)
			return
		}
		a.NameChanged(name)
	}()
}
