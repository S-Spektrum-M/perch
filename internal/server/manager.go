package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"perchd/internal/engine"
	"perchd/internal/notify"
)

// Manager owns the open sessions' agents, keyed by session ID.
type Manager struct {
	backends backendSet
	idle     time.Duration

	hideStatus map[string]bool // extension status keys to hide

	render   Renderer         // set by the server before use
	notifier *notify.Notifier // configured before starting agents

	mu       sync.Mutex
	agents   map[string]*Agent
	starting map[string]*startCall
}

type startCall struct {
	done  chan struct{}
	agent *Agent
	err   error
}

func NewManager(backends backendSet, idle time.Duration, hideStatus map[string]bool) *Manager {
	m := &Manager{
		backends: backends, idle: idle, hideStatus: hideStatus,
		agents:   map[string]*Agent{},
		starting: map[string]*startCall{},
	}
	go m.reap()
	return m
}

// Get returns the running agent for a session, if any.
func (m *Manager) Get(id string) *Agent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.agents[id]
}

// Attach returns the agent for an existing session, opening it in the
// backend named by info.Engine if needed. Concurrent callers share one start.
func (m *Manager) Attach(info engine.SessionInfo) (*Agent, error) {
	b, err := m.backend(info.Engine)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if a := m.agents[info.ID]; a != nil {
		m.mu.Unlock()
		return a, nil
	}
	if c := m.starting[info.ID]; c != nil {
		m.mu.Unlock()
		<-c.done
		return c.agent, c.err
	}
	c := &startCall{done: make(chan struct{})}
	m.starting[info.ID] = c
	m.mu.Unlock()

	info.Cwd = dirOrHome(info.Cwd)
	c.agent, c.err = m.start(b, info.Cwd, func(a *Agent) (engine.Engine, error) { return b.Open(info, a) })
	if c.err == nil && c.agent.ID != info.ID {
		log.Printf("warning: session %s reopened with id %s", info.ID, c.agent.ID)
	}

	m.mu.Lock()
	delete(m.starting, info.ID)
	if c.err == nil {
		m.agents[info.ID] = c.agent
	}
	m.mu.Unlock()
	close(c.done)
	return c.agent, c.err
}

// Create starts a brand-new session in dir with the named backend, or the
// default one for "".
func (m *Manager) Create(engineName, dir, name string) (*Agent, error) {
	b, err := m.backend(engineName)
	if err != nil {
		return nil, err
	}
	a, err := m.start(b, dir, func(a *Agent) (engine.Engine, error) { return b.Create(dir, name, a) })
	if err != nil {
		return nil, err
	}
	if a.ID == "" {
		a.Close()
		return nil, fmt.Errorf("%s did not report a session id", b.Name())
	}
	m.mu.Lock()
	m.agents[a.ID] = a
	m.mu.Unlock()
	return a, nil
}

// Duplicate starts a new session copied from info, in the same backend.
func (m *Manager) Duplicate(info engine.SessionInfo) (*Agent, error) {
	b, err := m.backend(info.Engine)
	if err != nil {
		return nil, err
	}
	info.Cwd = dirOrHome(info.Cwd)
	a, err := m.start(b, info.Cwd, func(a *Agent) (engine.Engine, error) { return b.Duplicate(info, a) })
	if err != nil {
		return nil, err
	}
	if a.ID == "" || a.ID == info.ID {
		a.Close()
		return nil, fmt.Errorf("%s did not report a new session id", b.Name())
	}
	m.mu.Lock()
	m.agents[a.ID] = a
	m.mu.Unlock()
	return a, nil
}

// backend returns the backend called name, or the default for "".
func (m *Manager) backend(name string) (engine.Backend, error) {
	if b := m.backends.get(name); b != nil {
		return b, nil
	}
	return nil, fmt.Errorf("unknown engine %q", name)
}

// start starts an engine of backend b for an agent in dir and loads its state.
func (m *Manager) start(b engine.Backend, dir string, open func(*Agent) (engine.Engine, error)) (*Agent, error) {
	a := newAgent(dir, m.render, m.hideStatus)
	a.backend = b
	eng, err := open(a)
	if err != nil {
		return nil, fmt.Errorf("could not start %s: %w", b.Name(), err)
	}
	a.engine = eng
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.load(ctx); err != nil {
		a.Close()
		return nil, fmt.Errorf("could not start %s: %w", b.Name(), err)
	}
	if m.notifier != nil {
		a.mu.Lock()
		a.notify = func(id, label string) {
			m.notifier.Send(id, label, b.Name())
		}
		a.notifyQuestion = func(id, label string) {
			m.notifier.Question(id, label, b.Name())
		}
		a.mu.Unlock()
	}
	log.Printf("agent %s: started %s in %s", a.ID, b.Name(), dir)
	go func() {
		<-a.done
		m.mu.Lock()
		if m.agents[a.ID] == a {
			delete(m.agents, a.ID)
		}
		m.mu.Unlock()
	}()
	return a, nil
}

// All returns the running agents.
func (m *Manager) All() []*Agent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Agent, 0, len(m.agents))
	for _, a := range m.agents {
		out = append(out, a)
	}
	return out
}

// reap stops agents nobody has looked at for a while.
func (m *Manager) reap() {
	for range time.Tick(time.Minute) {
		for _, a := range m.All() {
			if a.Idle(m.idle) {
				log.Printf("agent %s: idle, stopping", a.ID)
				go a.Close()
			}
		}
	}
}

func (m *Manager) CloseAll() {
	var wg sync.WaitGroup
	for _, a := range m.All() {
		wg.Add(1)
		go func() { defer wg.Done(); a.Close() }()
	}
	wg.Wait()
}

func dirOrHome(dir string) string {
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		return dir
	}
	home, _ := os.UserHomeDir()
	return home
}
