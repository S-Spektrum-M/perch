package server

import (
	"errors"
	"log"
	"os"
	"sync"
	"time"

	"pircd/internal/notify"
	"pircd/internal/pi"
)

// Manager owns the running pi processes, keyed by session ID.
type Manager struct {
	bin  string
	args []string
	idle time.Duration

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

func NewManager(bin string, args []string, idle time.Duration, hideStatus map[string]bool) *Manager {
	m := &Manager{
		bin: bin, args: args, idle: idle, hideStatus: hideStatus,
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

// Attach returns the agent for an existing session, starting
// `pi --session <file>` if needed. Concurrent callers share one start.
func (m *Manager) Attach(info pi.SessionInfo) (*Agent, error) {
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

	c.err = pi.CheckSessionAvailable(info.File)
	if c.err == nil {
		c.agent, c.err = m.start(dirOrHome(info.Cwd), "--session", info.File)
	}
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
	var args []string
	if name != "" {
		args = append(args, "--name", name)
	}
	a, err := m.start(dir, args...)
	if err != nil {
		return nil, err
	}
	if a.ID == "" {
		a.Close()
		return nil, errors.New("pi did not report a session id")
	}
	m.mu.Lock()
	m.agents[a.ID] = a
	m.mu.Unlock()
	return a, nil
}

// Duplicate starts a new session copied from info's session file.
func (m *Manager) Duplicate(info pi.SessionInfo) (*Agent, error) {
	a, err := m.start(dirOrHome(info.Cwd), "--fork", info.File)
	if err != nil {
		return nil, err
	}
	if a.ID == "" || a.ID == info.ID {
		a.Close()
		return nil, errors.New("pi did not report a new session id")
	}
	m.mu.Lock()
	m.agents[a.ID] = a
	m.mu.Unlock()
	return a, nil
}

func (m *Manager) start(dir string, args ...string) (*Agent, error) {
	a, err := startAgent(m.bin, append(append([]string(nil), m.args...), args...), dir, m.render, m.hideStatus)
	if err != nil {
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
