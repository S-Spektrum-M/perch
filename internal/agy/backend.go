// Package agy is the backend for Google Antigravity, driven over the
// Antigravity Agent Client Protocol (ACP) JSON-RPC 2.0 server
// (agy_acp_server.par). Each open session runs one ACP server process;
// sessions are stored under <GEMINI_HOME>/antigravity-acp/conversations.
package agy

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"perchd/internal/engine"
)

const Provider = "antigravity"

// Config configures the Antigravity ACP backend.
type Config struct {
	Bin      string // path to agy_acp_server.par (or antigravity-acp)
	Home     string // GEMINI_HOME directory holding antigravity-acp/
	Mode     string // ACP session mode: yolo, auto_edit, or default
	Defaults string // file holding the model defaults for new sessions
}

type Backend struct {
	cfg   Config
	store *Store

	mu      sync.Mutex
	models  []engine.Model // cached catalog
	modeled time.Time
}

var (
	_ engine.Backend      = (*Backend)(nil)
	_ engine.ModelCatalog = (*Backend)(nil)
)

func NewBackend(cfg Config) *Backend {
	cfg.Bin = cmp.Or(cfg.Bin, DefaultBin())
	cfg.Home = cmp.Or(cfg.Home, DefaultHome())
	cfg.Mode = cmp.Or(cfg.Mode, "yolo")
	cfg.Defaults = cmp.Or(cfg.Defaults, DefaultDefaultsPath())
	return &Backend{
		cfg:   cfg,
		store: NewStore(conversationsDir(cfg.Home)),
	}
}

func (b *Backend) Name() string { return "agy" }

func conversationsDir(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, "antigravity-acp", "conversations")
}

// DefaultBin locates the Antigravity ACP server binary, checking AGY_ACP_BIN,
// PATH, and ~/.t3/tools/antigravity-acp.
func DefaultBin() string {
	if env := os.Getenv("AGY_ACP_BIN"); env != "" {
		return env
	}
	for _, name := range []string{"antigravity-acp", "agy_acp_server", "agy_acp_server.par"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "agy_acp_server.par"
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".t3", "tools", "antigravity-acp", "*", "versions", "*", "agy_acp_server.par"))
	if len(matches) == 0 {
		return "agy_acp_server.par"
	}
	best := matches[0]
	var bestTime time.Time
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && st.ModTime().After(bestTime) {
			best, bestTime = m, st.ModTime()
		}
	}
	return best
}

// DefaultHome locates GEMINI_HOME for the Antigravity ACP server, checking
// GEMINI_HOME, ~/.gemini, and ~/.t3/userdata/providers/antigravity.
func DefaultHome() string {
	if env := os.Getenv("GEMINI_HOME"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	def := filepath.Join(home, ".gemini")
	if hasACPState(def) {
		return def
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".t3", "userdata", "providers", "antigravity", "*"))
	var best string
	var bestTime time.Time
	for _, m := range matches {
		if !hasACPState(m) {
			continue
		}
		st, err := os.Stat(filepath.Join(m, "antigravity-acp"))
		if err == nil && (best == "" || st.ModTime().After(bestTime)) {
			best, bestTime = m, st.ModTime()
		}
	}
	if best != "" {
		return best
	}
	return def
}

func hasACPState(dir string) bool {
	acpDir := filepath.Join(dir, "antigravity-acp")
	for _, name := range []string{"acp_token.json", "acp_business_token.json", "settings.json", "conversations"} {
		if _, err := os.Stat(filepath.Join(acpDir, name)); err == nil {
			return true
		}
	}
	return false
}

// DefaultDefaultsPath is the default file for SaveModelDefaults.
func DefaultDefaultsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "perchd", "agy.json")
}

// List returns the stored sessions, most recently modified first.
func (b *Backend) List() []engine.SessionInfo {
	return b.store.List()
}

// Find looks a stored session up by its ID.
func (b *Backend) Find(id string) (engine.SessionInfo, bool) {
	return b.store.Find(id)
}

func (b *Backend) Open(info engine.SessionInfo, sink engine.Sink) (engine.Engine, error) {
	return b.start(sessionConfig{
		Mode:      "open",
		Cwd:       info.Cwd,
		SessionID: info.ID,
		Name:      info.Name,
	}, sink)
}

func (b *Backend) Create(dir, name string, sink engine.Sink) (engine.Engine, error) {
	return b.start(sessionConfig{
		Mode: "create",
		Cwd:  dir,
		Name: name,
	}, sink)
}

func (b *Backend) Duplicate(info engine.SessionInfo, sink engine.Sink) (engine.Engine, error) {
	newID, err := b.store.Fork(info.ID)
	if err != nil {
		return nil, fmt.Errorf("duplicate %s: %w", info.ID, err)
	}
	return b.start(sessionConfig{
		Mode:      "open",
		Cwd:       info.Cwd,
		SessionID: newID,
		Name:      info.Name,
	}, sink)
}

type modelDefaults struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// SaveModelDefaults records the model and effort for new sessions in perchd's
// own config.
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
	if b.cfg.Defaults == "" {
		return d
	}
	if data, err := os.ReadFile(b.cfg.Defaults); err == nil {
		_ = json.Unmarshal(data, &d)
	}
	return d
}

func (b *Backend) cacheModels(models []engine.Model) {
	if len(models) == 0 {
		return
	}
	cp := append([]engine.Model(nil), models...)
	b.mu.Lock()
	b.models = cp
	b.modeled = time.Now()
	b.mu.Unlock()
}

func (b *Backend) start(c sessionConfig, sink engine.Sink) (engine.Engine, error) {
	d := b.defaults()
	c.Bin = b.cfg.Bin
	c.Home = b.cfg.Home
	c.SessionMode = b.cfg.Mode
	c.DefaultModel = d.Model
	c.DefaultEffort = d.Effort
	c.Store = b.store
	c.OnModels = b.cacheModels
	return startSession(c, sink)
}

// Models returns the models a new session can use, cached for a few minutes.
func (b *Backend) Models(ctx context.Context) ([]engine.Model, error) {
	b.mu.Lock()
	if len(b.models) > 0 && time.Since(b.modeled) < 5*time.Minute {
		out := append([]engine.Model(nil), b.models...)
		b.mu.Unlock()
		return out, nil
	}
	b.mu.Unlock()

	if cached := readT3CachedModels(); len(cached) > 0 {
		b.cacheModels(cached)
		return cached, nil
	}
	return defaultCatalogModels(), nil
}

// readT3CachedModels reads ~/.t3/caches/antigravity.json if available.
func readT3CachedModels() []engine.Model {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(home, ".t3", "caches", "antigravity.json"))
	if err != nil {
		return nil
	}
	var payload struct {
		Entries map[string]struct {
			Models []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"models"`
		} `json:"entries"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil
	}
	for _, entry := range payload.Entries {
		if len(entry.Models) == 0 {
			continue
		}
		raw := make([]acpModelInfo, 0, len(entry.Models))
		for _, m := range entry.Models {
			raw = append(raw, acpModelInfo{ModelID: m.ID, Name: m.Name})
		}
		cat := parseModelCatalog(raw)
		if len(cat.models) > 0 {
			return cat.models
		}
	}
	return nil
}

func defaultCatalogModels() []engine.Model {
	cat := parseModelCatalog([]acpModelInfo{
		{ModelID: "gemini-4-argon-high", Name: "Gemini 4 Argon (High)"},
		{ModelID: "gemini-4-argon-medium", Name: "Gemini 4 Argon (Medium)"},
		{ModelID: "gemini-4-argon-low", Name: "Gemini 4 Argon (Low)"},
		{ModelID: "gemini-3.8-flash-high", Name: "Gemini 3.8 Flash (High)"},
		{ModelID: "gemini-3.8-flash-medium", Name: "Gemini 3.8 Flash (Medium)"},
		{ModelID: "gemini-3.8-flash-low", Name: "Gemini 3.8 Flash (Low)"},
		{ModelID: "claude-opus-5-5-low", Name: "Claude Opus 5.5 (Low)"},
		{ModelID: "claude-sonnet-5-5-low", Name: "Claude Sonnet 5.5 (Low)"},
		{ModelID: "gpt-oss-120b-medium", Name: "GPT-OSS 120B (Medium)"},
	})
	return cat.models
}

// ---------------------------------------------------------------------------
// Store: <GEMINI_HOME>/antigravity-acp/conversations/*.db + *.meta

// Store scans Antigravity ACP's conversation directory. Parsed files are
// cached by size and mtime so listing stays fast.
type Store struct {
	dir string

	mu    sync.Mutex
	cache map[string]cachedInfo
}

type cachedInfo struct {
	dbSize    int64
	dbMtime   time.Time
	metaMtime time.Time
	info      engine.SessionInfo
}

func NewStore(dir string) *Store {
	return &Store{dir: dir, cache: map[string]cachedInfo{}}
}

func (s *Store) dbPath(id string) string {
	if s.dir == "" {
		return ""
	}
	return filepath.Join(s.dir, id+".db")
}

func (s *Store) metaPath(id string) string {
	if s.dir == "" {
		return ""
	}
	return filepath.Join(s.dir, id+".meta")
}

// List returns all non-empty stored sessions, most recently modified first.
func (s *Store) List() []engine.SessionInfo {
	if s.dir == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(s.dir, "*.db"))

	s.mu.Lock()
	defer s.mu.Unlock()

	seen := make(map[string]bool, len(files))
	out := make([]engine.SessionInfo, 0, len(files))
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			continue
		}
		size := st.Size()
		mtime := st.ModTime()
		if walSt, err := os.Stat(f + "-wal"); err == nil {
			size += walSt.Size()
			if walSt.ModTime().After(mtime) {
				mtime = walSt.ModTime()
			}
		}
		// Brand-new sessions pre-create an empty 0-byte .db before the first prompt.
		if size == 0 {
			continue
		}
		id := strings.TrimSuffix(filepath.Base(f), ".db")
		metaFile := filepath.Join(s.dir, id+".meta")
		var metaMtime time.Time
		if mst, err := os.Stat(metaFile); err == nil {
			metaMtime = mst.ModTime()
		}

		seen[f] = true
		c, ok := s.cache[f]
		if !ok || c.dbSize != size || !c.dbMtime.Equal(mtime) || !c.metaMtime.Equal(metaMtime) {
			info := readSessionInfo(id, f, metaFile, mtime)
			c = cachedInfo{dbSize: size, dbMtime: mtime, metaMtime: metaMtime, info: info}
			s.cache[f] = c
		}
		out = append(out, c.info)
	}
	for f := range s.cache {
		if !seen[f] {
			delete(s.cache, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out
}

// Find looks a stored session up by its ID.
func (s *Store) Find(id string) (engine.SessionInfo, bool) {
	for _, info := range s.List() {
		if info.ID == id {
			return info, true
		}
	}
	return engine.SessionInfo{}, false
}

// UpdateMeta merges keys into <id>.meta atomically.
func (s *Store) UpdateMeta(id string, updates map[string]any) error {
	if s.dir == "" || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	path := s.metaPath(id)
	existing := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &existing)
	}
	for k, v := range updates {
		existing[k] = v
	}
	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// ReadMeta reads a session's sidecar metadata.
func (s *Store) ReadMeta(id string) map[string]any {
	if s.dir == "" || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var m map[string]any
	if data, err := os.ReadFile(s.metaPath(id)); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	return m
}

// Fork copies <srcID>.db (and -wal if present) and <srcID>.meta to a newly
// generated UUID, replacing embedded trajectory_id references in the SQLite
// bytes so LocalHarness attaches to the forked trajectory.
func (s *Store) Fork(srcID string) (string, error) {
	if s.dir == "" {
		return "", errors.New("no conversations directory configured")
	}
	if len(srcID) != 36 {
		return "", fmt.Errorf("invalid source session id %q", srcID)
	}
	newID := newUUID()
	oldBytes := []byte(srcID)
	newBytes := []byte(newID)

	srcDB := s.dbPath(srcID)
	data, err := os.ReadFile(srcDB)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(s.dbPath(newID), bytes.ReplaceAll(data, oldBytes, newBytes), 0o644); err != nil {
		return "", err
	}
	if walData, err := os.ReadFile(srcDB + "-wal"); err == nil {
		_ = os.WriteFile(s.dbPath(newID)+"-wal", bytes.ReplaceAll(walData, oldBytes, newBytes), 0o644)
	}
	if metaData, err := os.ReadFile(s.metaPath(srcID)); err == nil {
		_ = os.WriteFile(s.metaPath(newID), metaData, 0o644)
	}
	return newID, nil
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func readSessionInfo(id, dbFile, metaFile string, modified time.Time) engine.SessionInfo {
	info := engine.SessionInfo{
		ID:       id,
		File:     dbFile,
		Created:  modified,
		Modified: modified,
		Prompts:  1,
	}
	if data, err := os.ReadFile(metaFile); err == nil {
		var m struct {
			Cwd      string `json:"cwd"`
			Name     string `json:"name"`
			FirstMsg string `json:"firstMsg"`
		}
		if json.Unmarshal(data, &m) == nil {
			info.Cwd = m.Cwd
			info.Name = m.Name
			if m.FirstMsg != "" {
				info.FirstMsg = engine.OneLine(m.FirstMsg, 140)
			}
		}
	}
	if info.FirstMsg == "" {
		info.FirstMsg = extractFirstPromptFromDB(dbFile)
	}
	if info.Cwd == "" {
		info.Cwd = extractCwdFromDB(dbFile)
	}
	return info
}

// extractFirstPromptFromDB scans the SQLite file for a <user_request> block if
// present (e.g. sessions created in T3 Code). Sessions opened in perchd cache
// their first message directly in <id>.meta.
func extractFirstPromptFromDB(dbFile string) string {
	f, err := os.Open(dbFile)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 256*1024)
	n, _ := f.Read(buf)
	data := buf[:n]
	const startTag = "<user_request>"
	const endTag = "</user_request>"
	if i := bytes.Index(data, []byte(startTag)); i >= 0 {
		rest := data[i+len(startTag):]
		if j := bytes.Index(rest, []byte(endTag)); j >= 0 {
			return engine.OneLine(string(bytes.TrimSpace(rest[:j])), 140)
		}
	}
	return ""
}

func extractCwdFromDB(dbFile string) string {
	f, err := os.Open(dbFile)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 64*1024)
	n, _ := f.Read(buf)
	data := buf[:n]
	const prefix = "file:///"
	for len(data) > 0 {
		i := bytes.Index(data, []byte(prefix))
		if i < 0 {
			break
		}
		rest := data[i+len("file://"):]
		end := 0
		for end < len(rest) && rest[end] >= 0x20 && rest[end] < 0x7f && rest[end] != ':' && rest[end] != '"' && rest[end] != ')' && rest[end] != '>' {
			end++
		}
		if end > 1 {
			candidate := string(rest[:end])
			if st, err := os.Stat(candidate); err == nil {
				if st.IsDir() {
					return candidate
				}
				return filepath.Dir(candidate)
			}
		}
		data = rest[end:]
	}
	return ""
}
