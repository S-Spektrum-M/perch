// Package claude is the backend for the Claude Agent SDK. The SDK is a
// TypeScript library, so each open session runs bridge/claude/bridge.mjs, a
// small Node program that drives one SDK query and speaks JSON lines with
// perchd. Sessions are Claude Code's own (~/.claude/projects), so they can be
// resumed with `claude --resume` too.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"perchd/internal/engine"
)

// Config configures the Claude backend.
type Config struct {
	Node           string // node executable
	Bridge         string // path to bridge.mjs
	PermissionMode string // SDK permission mode: default, acceptEdits, plan, auto, bypassPermissions
	// Auth is "subscription" to use the login stored by `claude /login`
	// (ANTHROPIC_API_KEY is hidden from the SDK), or "env" to let the SDK
	// pick up API credentials from the environment.
	Auth     string
	Defaults string // file holding the model defaults for new sessions
}

type Backend struct {
	cfg Config

	mu      sync.Mutex
	models  []engine.Model // cached catalog
	modeled time.Time
	list    []engine.SessionInfo
	listed  time.Time
	listing bool
}

var (
	_ engine.Backend      = (*Backend)(nil)
	_ engine.ModelCatalog = (*Backend)(nil)
)

func NewBackend(cfg Config) *Backend {
	return &Backend{cfg: cfg}
}

// DefaultBridge is where deploy/install.sh installs the bridge.
func DefaultBridge() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, ".local", "share", "perchd", "claude-bridge", "bridge.mjs")
}

// DefaultDefaultsPath is the default file for SaveModelDefaults.
func DefaultDefaultsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "perchd", "claude.json")
}

func (b *Backend) Name() string { return "claude" }

// listTTL bounds how stale the session list may be. Listing runs a node
// process (~150 ms), so pages are served from the cache and it is refreshed
// in the background.
const listTTL = 5 * time.Second

// List returns the stored sessions, most recently modified first.
func (b *Backend) List() []engine.SessionInfo {
	b.mu.Lock()
	if b.listed.IsZero() {
		b.mu.Unlock()
		b.refresh()
		b.mu.Lock()
	} else if time.Since(b.listed) > listTTL && !b.listing {
		b.listing = true
		go b.refresh()
	}
	defer b.mu.Unlock()
	return b.list
}

func (b *Backend) refresh() {
	list, err := b.listSessions()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listing = false
	b.listed = time.Now()
	if err != nil {
		log.Printf("claude: listing sessions: %v", err)
		return
	}
	b.list = list
}

func (b *Backend) listSessions() ([]engine.SessionInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.cfg.Node, b.cfg.Bridge, "list")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			err = fmt.Errorf("%w: %s", err, ee.Stderr)
		}
		return nil, err
	}
	var rows []struct {
		ID       string `json:"id"`
		Cwd      string `json:"cwd"`
		File     string `json:"file"`
		Name     string `json:"name"`
		FirstMsg string `json:"firstMsg"`
		Created  int64  `json:"created"`
		Modified int64  `json:"modified"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, err
	}
	list := make([]engine.SessionInfo, 0, len(rows))
	for _, r := range rows {
		list = append(list, engine.SessionInfo{
			ID: r.ID, Cwd: r.Cwd, File: r.File, Name: r.Name,
			FirstMsg: engine.OneLine(r.FirstMsg, 140),
			Created:  time.UnixMilli(r.Created), Modified: time.UnixMilli(r.Modified),
			// The SDK doesn't report prompt counts; any listed session has one.
			Prompts: 1,
		})
	}
	return list, nil
}

func (b *Backend) Find(id string) (engine.SessionInfo, bool) {
	for _, info := range b.List() {
		if info.ID == id {
			return info, true
		}
	}
	// A session created moments ago may not be in the cached list yet.
	b.refresh()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, info := range b.list {
		if info.ID == id {
			return info, true
		}
	}
	return engine.SessionInfo{}, false
}

func (b *Backend) Open(info engine.SessionInfo, sink engine.Sink) (engine.Engine, error) {
	return b.start(bridgeConfig{Mode: "open", Cwd: info.Cwd, SessionID: info.ID}, sink)
}

func (b *Backend) Create(dir, name string, sink engine.Sink) (engine.Engine, error) {
	return b.start(bridgeConfig{Mode: "create", Cwd: dir, Name: name}, sink)
}

func (b *Backend) Duplicate(info engine.SessionInfo, sink engine.Sink) (engine.Engine, error) {
	return b.start(bridgeConfig{Mode: "fork", Cwd: info.Cwd, SessionID: info.ID}, sink)
}

type modelDefaults struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// SaveModelDefaults records the model and effort for new sessions in perchd's
// own config, leaving Claude Code's settings alone.
func (b *Backend) SaveModelDefaults(ctx context.Context, model engine.Model, thinking string) error {
	if b.cfg.Defaults == "" {
		return errors.New("no defaults file configured")
	}
	data, err := json.MarshalIndent(modelDefaults{Model: model.ID, Effort: thinking}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.cfg.Defaults), 0o755); err != nil {
		return err
	}
	return os.WriteFile(b.cfg.Defaults, append(data, '\n'), 0o644)
}

func (b *Backend) defaults() modelDefaults {
	var d modelDefaults
	if data, err := os.ReadFile(b.cfg.Defaults); err == nil {
		if err := json.Unmarshal(data, &d); err != nil {
			log.Printf("claude: ignoring %s: %v", b.cfg.Defaults, err)
		}
	}
	return d
}

func (b *Backend) start(c bridgeConfig, sink engine.Sink) (engine.Engine, error) {
	if _, err := os.Stat(b.cfg.Bridge); err != nil {
		return nil, fmt.Errorf("claude bridge not installed (run deploy/install.sh): %w", err)
	}
	d := b.defaults()
	c.Model, c.Effort = d.Model, d.Effort
	c.PermissionMode, c.Auth = b.cfg.PermissionMode, b.cfg.Auth
	s, err := startSession(b.cfg.Node, b.cfg.Bridge, c, sink)
	if err != nil {
		return nil, err
	}
	// New sessions only reach the listing after their first prompt;
	// refresh so they show up promptly once they do.
	b.mu.Lock()
	b.listed = time.Time{}
	b.mu.Unlock()
	return s, nil
}

// Models lists the models a new session can use (`node bridge.mjs models`,
// which asks Claude Code), cached for a few minutes.
func (b *Backend) Models(ctx context.Context) ([]engine.Model, error) {
	b.mu.Lock()
	if time.Since(b.modeled) < 5*time.Minute {
		defer b.mu.Unlock()
		return b.models, nil
	}
	b.mu.Unlock()
	arg, err := json.Marshal(bridgeConfig{Auth: b.cfg.Auth})
	if err != nil {
		return nil, err
	}
	out, err := exec.CommandContext(ctx, b.cfg.Node, b.cfg.Bridge, "models", string(arg)).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			err = fmt.Errorf("%w: %s", err, ee.Stderr)
		}
		return nil, fmt.Errorf("listing claude models: %w", err)
	}
	var models []engine.Model
	if err := json.Unmarshal(out, &models); err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.models, b.modeled = models, time.Now()
	b.mu.Unlock()
	return models, nil
}
