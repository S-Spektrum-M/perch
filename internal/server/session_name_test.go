package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"perchd/internal/engine"
)

type namingBackend struct {
	engine.Backend
	suggest func(context.Context, string, string) (string, error)
}

func (b namingBackend) SuggestSessionName(ctx context.Context, dir, prompt string) (string, error) {
	return b.suggest(ctx, dir, prompt)
}

type namingEngine struct {
	engine.Engine
	mu    sync.Mutex
	names []string
}

func (e *namingEngine) SetName(_ context.Context, name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.names = append(e.names, name)
	return nil
}

func waitForNaming(t *testing.T, a *Agent) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		busy := a.naming
		a.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("naming did not finish")
}

func TestSuggestedSessionName(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "generated", true: "manual rename wins"}[manual], func(t *testing.T) {
			a := newAgent(t.TempDir(), nil, nil)
			e := &namingEngine{}
			a.engine = e
			started, release := make(chan struct{}), make(chan struct{})
			b := namingBackend{suggest: func(ctx context.Context, dir, prompt string) (string, error) {
				if dir != a.Cwd || prompt != "Fix login bug" {
					t.Errorf("unexpected input: %q %q", dir, prompt)
				}
				close(started)
				<-release
				return "Repair login flow", nil
			}}
			a.suggestName(b, "Fix login bug")
			<-started
			a.suggestName(b, "must not start another request")
			want := "Repair login flow"
			if manual {
				want = "My title"
				if err := a.SetName(context.Background(), want); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			waitForNaming(t, a)
			a.mu.Lock()
			got := a.name
			a.mu.Unlock()
			if got != want {
				t.Fatalf("name = %q, want %q", got, want)
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			if len(e.names) != 1 || e.names[0] != want {
				t.Fatalf("saved names = %v", e.names)
			}
		})
	}
}

func TestSessionNamingSkipsNamedAndEmpty(t *testing.T) {
	a := newAgent(t.TempDir(), nil, nil)
	b := namingBackend{suggest: func(context.Context, string, string) (string, error) {
		t.Error("unexpected naming request")
		return "", nil
	}}
	a.suggestName(b, "  ")
	a.name = "Explicit title"
	a.suggestName(b, "Some prompt")
	waitForNaming(t, a)
}

func TestSessionNamingFailureRetries(t *testing.T) {
	a := newAgent(t.TempDir(), nil, nil)
	a.engine = &namingEngine{}
	calls := 0
	b := namingBackend{suggest: func(context.Context, string, string) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("unavailable")
		}
		return "Recovered title", nil
	}}
	a.suggestName(b, "First prompt")
	waitForNaming(t, a)
	a.suggestName(b, "Second prompt")
	waitForNaming(t, a)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.name != "Recovered title" || calls != 2 {
		t.Fatalf("name=%q calls=%d", a.name, calls)
	}
}

func TestSessionNamingCancelledOnClose(t *testing.T) {
	a := newAgent(t.TempDir(), nil, nil)
	a.engine = &namingEngine{}
	started := make(chan struct{})
	b := namingBackend{suggest: func(ctx context.Context, _, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}}
	a.suggestName(b, "Prompt")
	<-started
	close(a.done)
	waitForNaming(t, a)
}
