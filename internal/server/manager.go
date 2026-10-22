package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"pircd/internal/engine"
	"pircd/internal/notify"
)

// Manager owns the open sessions' agents, keyed by session ID.
type Manager struct {
	backend engine.Backend
	idle    time.Duration

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

func NewManager(backend engine.Backend, idle time.Duration, hideStatus map[string]bool) *Manager {
	m := &Manager{
		backend: backend, idle: idle, hideStatus: hideStatus,
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
// backend if needed. Concurrent callers share one start.
func (m *Manager) Attach(info engine.SessionInfo) (*Agent, error) {
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
	c.agent, c.err = m.start(info.Cwd, func(a *Agent) (engine.Engine, error) { return m.backend.Open(info, a) })
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

// Create starts a brand-new session in dir.
func (m *Manager) Create(dir, name string) (*Agent, error) {
	a, err := m.start(dir, func(a *Agent) (engine.Engine, error) { return m.backend.Create(dir, name, a) })
	if err != nil {
		return nil, err
	}
	if a.ID == "" {
		a.Close()
		return nil, fmt.Errorf("%s did not report a session id", m.backend.Name())
	}
	m.mu.Lock()
	m.agents[a.ID] = a
	m.mu.Unlock()
	return a, nil
}

// Duplicate starts a new session copied from info.
func (m *Manager) Duplicate(info engine.SessionInfo) (*Agent, error) {
	info.Cwd = dirOrHome(info.Cwd)
	a, err := m.start(info.Cwd, func(a *Agent) (engine.Engine, error) { return m.backend.Duplicate(info, a) })
	if err != nil {
		return nil, err
	}
	if a.ID == "" || a.ID == info.ID {
		a.Close()
		return nil, fmt.Errorf("%s did not report a new session id", m.backend.Name())
	}
	m.mu.Lock()
	m.agents[a.ID] = a
	m.mu.Unlock()
	return a, nil
}

// start starts an engine for an agent in dir and loads its state.
func (m *Manager) start(dir string, open func(*Agent) (engine.Engine, error)) (*Agent, error) {
	a := newAgent(dir, m.render, m.hideStatus)
	eng, err := open(a)
	if err != nil {
		return nil, err
	}
	a.engine = eng
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.load(ctx); err != nil {
		a.Close()
		return nil, err
	}
	if m.notifier != nil {
		a.mu.Lock()
		a.notify = m.notifier.Send
		a.mu.Unlock()
	}
	log.Printf("agent %s: started in %s", a.ID, dir)
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
