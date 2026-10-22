package pi

import (
	"context"

	"pircd/internal/engine"
)

// Config configures the pi backend.
type Config struct {
	Bin         string   // pi executable
	Args        []string // extra arguments for every pi process
	SessionsDir string   // pi session directory
}

// Backend runs sessions with `pi --mode rpc`, one process per open session,
// and lists them from pi's session files.
type Backend struct {
	*Store
	bin  string
	args []string
}

var _ engine.Backend = (*Backend)(nil)

func NewBackend(cfg Config) *Backend {
	return &Backend{Store: NewStore(cfg.SessionsDir), bin: cfg.Bin, args: cfg.Args}
}

func (b *Backend) Name() string { return "pi" }

func (b *Backend) Open(info engine.SessionInfo, sink engine.Sink) (engine.Engine, error) {
	if err := CheckSessionAvailable(info.File); err != nil {
		return nil, err
	}
	return b.start(info.Cwd, sink, "--session", info.File)
}

func (b *Backend) Create(dir, name string, sink engine.Sink) (engine.Engine, error) {
	var args []string
	if name != "" {
		args = append(args, "--name", name)
	}
	return b.start(dir, sink, args...)
}

func (b *Backend) Duplicate(info engine.SessionInfo, sink engine.Sink) (engine.Engine, error) {
	return b.start(info.Cwd, sink, "--fork", info.File)
}

func (b *Backend) SaveModelDefaults(ctx context.Context, model engine.Model, thinking string) error {
	return saveModelDefaults(ctx, model, thinking)
}

func (b *Backend) start(dir string, sink engine.Sink, args ...string) (engine.Engine, error) {
	p, err := startRPC(b.bin, append(append([]string(nil), b.args...), args...), dir, sink)
	if err != nil {
		return nil, err
	}
	return p, nil
}
