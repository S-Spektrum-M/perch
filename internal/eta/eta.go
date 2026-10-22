// Package eta is the backend for Eta, the libagent-based agent. It is a stub:
// every operation fails with ErrNotImplemented.
//
// The intended shape is a client for agentd, the daemon that owns Eta's
// sessions: Backend lists and opens sessions through it, and Session
// translates agentd's events into engine.Sink calls.
package eta

import (
	"context"
	"errors"
	"fmt"

	"pircd/internal/engine"
)

var ErrNotImplemented = errors.New("eta: not implemented")

// Config configures the Eta backend.
type Config struct {
	Addr string // agentd's address; unused until the client exists
}

type Backend struct {
	addr string
}

var _ engine.Backend = (*Backend)(nil)

func NewBackend(cfg Config) *Backend {
	return &Backend{addr: cfg.Addr}
}

func (b *Backend) Name() string { return "eta" }

// TODO: list sessions from agentd.
func (b *Backend) List() []engine.SessionInfo { return nil }

func (b *Backend) Find(id string) (engine.SessionInfo, bool) {
	for _, info := range b.List() {
		if info.ID == id {
			return info, true
		}
	}
	return engine.SessionInfo{}, false
}

// TODO: attach to the session in agentd and start forwarding its events.
func (b *Backend) Open(info engine.SessionInfo, sink engine.Sink) (engine.Engine, error) {
	return nil, fmt.Errorf("open %s: %w", info.ID, ErrNotImplemented)
}

func (b *Backend) Create(dir, name string, sink engine.Sink) (engine.Engine, error) {
	return nil, fmt.Errorf("create session in %s: %w", dir, ErrNotImplemented)
}

func (b *Backend) Duplicate(info engine.SessionInfo, sink engine.Sink) (engine.Engine, error) {
	return nil, fmt.Errorf("duplicate %s: %w", info.ID, ErrNotImplemented)
}

func (b *Backend) SaveModelDefaults(ctx context.Context, model engine.Model, thinking string) error {
	return ErrNotImplemented
}

// Session is one open Eta session.
type Session struct {
	sink engine.Sink
}

var _ engine.Engine = (*Session)(nil)

func (s *Session) State(ctx context.Context) (engine.State, error) {
	return engine.State{}, ErrNotImplemented
}

func (s *Session) Messages(ctx context.Context) ([]engine.Message, error) {
	return nil, ErrNotImplemented
}

func (s *Session) Prompt(ctx context.Context, text string, mode engine.PromptMode) error {
	return ErrNotImplemented
}

func (s *Session) Abort(ctx context.Context) ([]string, error) {
	return nil, ErrNotImplemented
}

func (s *Session) Shell(ctx context.Context, command string, exclude bool) (engine.ShellResult, error) {
	return engine.ShellResult{}, ErrNotImplemented
}

func (s *Session) AbortShell(ctx context.Context) error {
	return ErrNotImplemented
}

func (s *Session) Compact(ctx context.Context, instructions string) error {
	return ErrNotImplemented
}

func (s *Session) SetName(ctx context.Context, name string) error {
	return ErrNotImplemented
}

func (s *Session) Commands(ctx context.Context) ([]engine.Command, error) {
	return nil, ErrNotImplemented
}

func (s *Session) Stats(ctx context.Context) (engine.Stats, error) {
	return engine.Stats{}, ErrNotImplemented
}

func (s *Session) Models(ctx context.Context) ([]engine.Model, error) {
	return nil, ErrNotImplemented
}

func (s *Session) SetModel(ctx context.Context, provider, id string) error {
	return ErrNotImplemented
}

func (s *Session) ThinkingLevels(ctx context.Context) ([]string, error) {
	return nil, ErrNotImplemented
}

func (s *Session) SetThinking(ctx context.Context, level string) error {
	return ErrNotImplemented
}

func (s *Session) RespondUI(id string, resp engine.UIResponse) error {
	return ErrNotImplemented
}

// Close must report sink.Stopped before returning, as engine.Engine requires.
func (s *Session) Close() {
	s.sink.Stopped("eta session closed")
}
