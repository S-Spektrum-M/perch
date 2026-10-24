package agy

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pmezard/go-difflib/difflib"

	"perchd/internal/engine"
)

type sessionConfig struct {
	Mode          string // "create" or "open"
	Bin           string
	Home          string
	Cwd           string
	SessionID     string
	Name          string
	SessionMode   string // "yolo", "auto_edit", "default"
	DefaultModel  string
	DefaultEffort string
	Store         *Store
	OnModels      func([]engine.Model)
}

type rpcResult struct {
	Result json.RawMessage
	Err    *rpcError
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("acp error %d", e.Code)
}

// acpModelInfo is one model entry in ACP NewSessionResponse / LoadSessionResponse.
type acpModelInfo struct {
	ModelID     string `json:"modelId"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type modelCatalog struct {
	models      []engine.Model
	efforts     map[string][]string          // baseID -> ordered effort levels (low, medium, high)
	variantToID map[string]map[string]string // baseID -> effort -> fullModelID
	defaultFull map[string]string            // baseID -> preferred fullModelID
	rawNames    map[string]string            // fullModelID -> displayName
}

var effortOrder = []string{"low", "medium", "high"}

// splitModelEffort splits an ACP model ID and display name into a base model
// and thinking effort level if it ends in -low, -medium, or -high.
func splitModelEffort(modelID, displayName string) (baseID, baseName, effort string) {
	for _, lvl := range effortOrder {
		suffix := "-" + lvl
		if strings.HasSuffix(modelID, suffix) {
			baseID = strings.TrimSuffix(modelID, suffix)
			baseName = displayName
			for _, label := range []string{" (" + strings.ToUpper(lvl[:1]) + lvl[1:] + ")", " (" + lvl + ")", " (" + strings.ToUpper(lvl) + ")"} {
				if strings.HasSuffix(baseName, label) {
					baseName = strings.TrimSuffix(baseName, label)
					break
				}
			}
			return baseID, cmp.Or(strings.TrimSpace(baseName), baseID), lvl
		}
	}
	return modelID, cmp.Or(strings.TrimSpace(displayName), modelID), ""
}

func parseModelCatalog(raw []acpModelInfo) modelCatalog {
	cat := modelCatalog{
		efforts:     map[string][]string{},
		variantToID: map[string]map[string]string{},
		defaultFull: map[string]string{},
		rawNames:    map[string]string{},
	}
	seenBase := map[string]bool{}
	hasEffort := map[string]map[string]bool{}

	for _, m := range raw {
		if m.ModelID == "" {
			continue
		}
		cat.rawNames[m.ModelID] = cmp.Or(m.Name, m.ModelID)
		baseID, baseName, effort := splitModelEffort(m.ModelID, m.Name)
		if !seenBase[baseID] {
			seenBase[baseID] = true
			cat.models = append(cat.models, engine.Model{
				ID:       baseID,
				Name:     baseName,
				Provider: Provider,
			})
			cat.defaultFull[baseID] = m.ModelID
		}
		if effort != "" {
			if cat.variantToID[baseID] == nil {
				cat.variantToID[baseID] = map[string]string{}
				hasEffort[baseID] = map[string]bool{}
			}
			cat.variantToID[baseID][effort] = m.ModelID
			hasEffort[baseID][effort] = true
		}
	}
	for baseID, set := range hasEffort {
		for _, lvl := range effortOrder {
			if set[lvl] {
				cat.efforts[baseID] = append(cat.efforts[baseID], lvl)
			}
		}
	}
	return cat
}

func (c modelCatalog) resolveFullID(baseOrFullID, effort string) (fullID string, ok bool) {
	if variants := c.variantToID[baseOrFullID]; len(variants) > 0 {
		if effort != "" {
			if id, exists := variants[effort]; exists {
				return id, true
			}
		}
		if def := c.defaultFull[baseOrFullID]; def != "" {
			return def, true
		}
	}
	if _, exists := c.rawNames[baseOrFullID]; exists {
		return baseOrFullID, true
	}
	baseID, _, splitEff := splitModelEffort(baseOrFullID, "")
	if variants := c.variantToID[baseID]; len(variants) > 0 {
		want := cmp.Or(effort, splitEff)
		if id, exists := variants[want]; exists {
			return id, true
		}
		if def := c.defaultFull[baseID]; def != "" {
			return def, true
		}
	}
	return "", false
}

func (c modelCatalog) stateModel(fullID string) (*engine.Model, string) {
	if fullID == "" {
		return nil, ""
	}
	baseID, baseName, effort := splitModelEffort(fullID, c.rawNames[fullID])
	for _, m := range c.models {
		if m.ID == baseID {
			cp := m
			return &cp, effort
		}
	}
	return &engine.Model{ID: baseID, Name: cmp.Or(baseName, baseID), Provider: Provider}, effort
}

type pendingPermission struct {
	reqID   json.RawMessage
	options []acpPermissionOption
}

type acpPermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type trackedTool struct {
	name string
	args json.RawMessage
}

// Session is one Antigravity ACP server child process bound to one session.
type Session struct {
	sink  engine.Sink
	cfg   sessionConfig
	cmd   *exec.Cmd
	stdin io.WriteCloser
	wmu   sync.Mutex

	pmu     sync.Mutex
	pending map[string]chan rpcResult
	nextID  atomic.Uint64

	done     chan struct{}
	exitOnce sync.Once

	smu    sync.Mutex
	stderr []byte

	mu             sync.Mutex
	sessionID      string
	sessionFile    string
	sessionName    string
	currentModelID string
	catalog        modelCatalog
	commands       []engine.Command
	contextUsage   *engine.ContextUsage

	// Transcript & streaming assembly
	replaying bool
	messages  []engine.Message
	asst      *engine.Message // in-progress assistant message
	liveKind  engine.BlockKind
	liveIdx   int
	liveStart bool
	toolCalls map[string]trackedTool
	openTools map[string]bool

	// Prompt queue & active run state
	running bool
	queue   []string

	// Permission / question dialogs
	dialogs map[string]pendingPermission
}

var _ engine.Engine = (*Session)(nil)

func newSession(cfg sessionConfig, sink engine.Sink, stdin io.WriteCloser) *Session {
	return &Session{
		sink:      sink,
		cfg:       cfg,
		stdin:     stdin,
		pending:   map[string]chan rpcResult{},
		done:      make(chan struct{}),
		liveIdx:   -1,
		toolCalls: map[string]trackedTool{},
		openTools: map[string]bool{},
		dialogs:   map[string]pendingPermission{},
	}
}

func startSession(c sessionConfig, sink engine.Sink) (*Session, error) {
	if _, err := os.Stat(c.Bin); err != nil {
		if _, lpErr := exec.LookPath(c.Bin); lpErr != nil {
			return nil, fmt.Errorf("antigravity ACP binary not found at %s: %w", c.Bin, err)
		}
	}
	// Pass --uid= so the Google C++ PAR launcher does not attempt to switch to
	// group "nobody" when running as root inside a user namespace.
	cmd := exec.Command(c.Bin, "--uid=")
	if c.Cwd != "" {
		if st, err := os.Stat(c.Cwd); err == nil && st.IsDir() {
			cmd.Dir = c.Cwd
		}
	}
	env := os.Environ()
	if c.Home != "" {
		env = append(env, "GEMINI_HOME="+c.Home)
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start agy acp server: %w", err)
	}

	s := newSession(c, sink, stdin)
	s.cmd = cmd

	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); s.readLoop(stdout) }()
	go func() { defer readers.Done(); s.stderrLoop(stderr) }()
	go func() {
		readers.Wait()
		s.onExit(cmd.Wait())
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := s.initSession(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Session) initSession(ctx context.Context) error {
	// 1. initialize handshake. Sending clientInfo.name = "zed" unlocks the
	// full model catalog (including Claude and GPT-OSS models alongside Gemini).
	// Do not advertise clientCapabilities.fs so the server uses its built-in
	// host filesystem tools directly.
	err := s.call(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
		"clientInfo": map[string]any{
			"name":    "zed",
			"title":   "perchd",
			"version": "1.0.0",
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}

	var resp struct {
		SessionID string `json:"sessionId"`
		Models    struct {
			CurrentModelID  string         `json:"currentModelId"`
			AvailableModels []acpModelInfo `json:"availableModels"`
		} `json:"models"`
		Modes struct {
			CurrentModeID string `json:"currentModeId"`
		} `json:"modes"`
	}

	switch s.cfg.Mode {
	case "open":
		s.mu.Lock()
		s.replaying = true
		s.mu.Unlock()
		err = s.call(ctx, "session/load", map[string]any{
			"sessionId":  s.cfg.SessionID,
			"cwd":        cmp.Or(s.cfg.Cwd, "/tmp"),
			"mcpServers": []any{},
		}, &resp)
		s.mu.Lock()
		s.finishReplayLocked()
		s.mu.Unlock()
		if err != nil {
			return fmt.Errorf("session/load: %w", err)
		}
		resp.SessionID = cmp.Or(resp.SessionID, s.cfg.SessionID)

	default:
		err = s.call(ctx, "session/new", map[string]any{
			"cwd":        cmp.Or(s.cfg.Cwd, "/tmp"),
			"mcpServers": []any{},
		}, &resp)
		if err != nil {
			return fmt.Errorf("session/new: %w", err)
		}
	}

	cat := parseModelCatalog(resp.Models.AvailableModels)
	if len(cat.models) == 0 {
		cat = parseModelCatalog([]acpModelInfo{
			{ModelID: cmp.Or(resp.Models.CurrentModelID, "gemini-3.8-flash-high"), Name: resp.Models.CurrentModelID},
		})
	}
	if s.cfg.OnModels != nil {
		s.cfg.OnModels(cat.models)
	}

	var dbFile string
	if s.cfg.Store != nil && resp.SessionID != "" {
		dbFile = s.cfg.Store.dbPath(resp.SessionID)
	} else if s.cfg.Home != "" && resp.SessionID != "" {
		dbFile = filepath.Join(conversationsDir(s.cfg.Home), resp.SessionID+".db")
	}

	name := s.cfg.Name
	if s.cfg.Store != nil && resp.SessionID != "" {
		meta := s.cfg.Store.ReadMeta(resp.SessionID)
		if name == "" {
			if n, ok := meta["name"].(string); ok {
				name = n
			}
		}
		updates := map[string]any{}
		if name != "" && meta["name"] != name {
			updates["name"] = name
		}
		s.mu.Lock()
		firstMsg := firstUserPrompt(s.messages)
		s.mu.Unlock()
		if firstMsg != "" {
			if existing, _ := meta["firstMsg"].(string); existing == "" {
				updates["firstMsg"] = firstMsg
			}
		}
		if len(updates) > 0 {
			_ = s.cfg.Store.UpdateMeta(resp.SessionID, updates)
		}
	}

	s.mu.Lock()
	s.sessionID = resp.SessionID
	s.sessionFile = dbFile
	s.sessionName = name
	s.currentModelID = resp.Models.CurrentModelID
	s.catalog = cat
	s.mu.Unlock()

	// Set configured session mode (e.g. "yolo" or "default") if different.
	wantMode := cmp.Or(s.cfg.SessionMode, "yolo")
	if wantMode != "" && wantMode != resp.Modes.CurrentModeID {
		_ = s.call(ctx, "session/set_config_option", map[string]any{
			"sessionId": resp.SessionID,
			"configId":  "mode",
			"value":     wantMode,
		}, nil)
	}

	// Apply saved default model/effort on newly created sessions.
	if s.cfg.Mode == "create" && (s.cfg.DefaultModel != "" || s.cfg.DefaultEffort != "") {
		baseID := s.cfg.DefaultModel
		if baseID == "" {
			m, _ := cat.stateModel(resp.Models.CurrentModelID)
			if m != nil {
				baseID = m.ID
			}
		}
		if fullID, ok := cat.resolveFullID(baseID, s.cfg.DefaultEffort); ok && fullID != resp.Models.CurrentModelID {
			if err := s.setFullModel(ctx, fullID); err != nil {
				log.Printf("agy: applying default model %s: %v", fullID, err)
			}
		}
	}

	return nil
}

func firstUserPrompt(msgs []engine.Message) string {
	for _, m := range msgs {
		if m.Role == "user" {
			if t := strings.TrimSpace(m.PlainText()); t != "" {
				return engine.OneLine(t, 140)
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// JSON-RPC 2.0 Plumbing

func (s *Session) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err = s.stdin.Write(append(b, '\n'))
	return err
}

func (s *Session) notify(method string, params any) error {
	return s.send(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

func (s *Session) call(ctx context.Context, method string, params any, dst any) error {
	numID := s.nextID.Add(1)
	key := fmt.Sprintf("%d", numID)
	ch := make(chan rpcResult, 1)
	s.pmu.Lock()
	s.pending[key] = ch
	s.pmu.Unlock()
	defer func() {
		s.pmu.Lock()
		delete(s.pending, key)
		s.pmu.Unlock()
	}()

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      numID,
		"method":  method,
		"params":  params,
	}
	if err := s.send(req); err != nil {
		return err
	}
	select {
	case r := <-ch:
		if r.Err != nil {
			return r.Err
		}
		if dst == nil || len(r.Result) == 0 {
			return nil
		}
		return json.Unmarshal(r.Result, dst)
	case <-s.done:
		return errors.New("agy acp server exited")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Session) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := br.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			s.handleLine(line)
		}
		if err != nil {
			return
		}
	}
}

func (s *Session) stderrLoop(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s.smu.Lock()
			s.stderr = append(s.stderr, buf[:n]...)
			if len(s.stderr) > 4096 {
				s.stderr = s.stderr[len(s.stderr)-4096:]
			}
			s.smu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (s *Session) onExit(err error) {
	s.exitOnce.Do(func() {
		reason := "agy acp server exited"
		if err != nil {
			reason += ": " + err.Error()
		}
		s.smu.Lock()
		if tail := strings.TrimSpace(string(s.stderr)); tail != "" {
			reason += "\n" + tail
		}
		s.smu.Unlock()
		s.sink.Stopped(reason)
		close(s.done)
	})
}

// Close closes the ACP server's stdin and kills its process group if it does
// not exit promptly.
func (s *Session) Close() {
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.cmd == nil || s.cmd.Process == nil {
		s.onExit(nil)
		return
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-s.done
	}
}

// ---------------------------------------------------------------------------
// Incoming JSON-RPC Messages (Responses, Notifications, Server Requests)

type rpcEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func idKey(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	return strings.Trim(s, `"`)
}

func (s *Session) handleLine(line []byte) {
	if len(line) == 0 || line[0] != '{' {
		return
	}
	var env rpcEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		log.Printf("agy: bad json-rpc record: %v", err)
		return
	}
	// 1. Response to a perchd request (has id, no method).
	if len(env.ID) > 0 && env.Method == "" {
		key := idKey(env.ID)
		s.pmu.Lock()
		ch := s.pending[key]
		s.pmu.Unlock()
		if ch != nil {
			ch <- rpcResult{Result: env.Result, Err: env.Error}
		}
		return
	}
	// 2. Server-to-client request (has id and method).
	if len(env.ID) > 0 && env.Method != "" {
		s.handleServerRequest(env.ID, env.Method, env.Params)
		return
	}
	// 3. Server-to-client notification (method, no id).
	if env.Method == "session/update" {
		s.handleSessionUpdate(env.Params)
	}
}

// ---------------------------------------------------------------------------
// Permission & Question Requests (session/request_permission)

type requestPermissionParams struct {
	SessionID string                `json:"sessionId"`
	Options   []acpPermissionOption `json:"options"`
	ToolCall  struct {
		ToolCallID string          `json:"toolCallId"`
		Title      string          `json:"title"`
		Kind       string          `json:"kind"`
		RawInput   json.RawMessage `json:"rawInput"`
	} `json:"toolCall"`
}

func (s *Session) handleServerRequest(reqID json.RawMessage, method string, rawParams json.RawMessage) {
	if method != "session/request_permission" {
		_ = s.send(map[string]any{
			"jsonrpc": "2.0",
			"id":      reqID,
			"error":   map[string]any{"code": -32601, "message": "method not found: " + method},
		})
		return
	}
	var p requestPermissionParams
	if err := json.Unmarshal(rawParams, &p); err != nil {
		_ = s.send(map[string]any{
			"jsonrpc": "2.0",
			"id":      reqID,
			"error":   map[string]any{"code": -32602, "message": err.Error()},
		})
		return
	}
	dialogID := cmp.Or(p.ToolCall.ToolCallID, fmt.Sprintf("perm-%s", idKey(reqID)))
	options := make([]string, 0, len(p.Options))
	for _, opt := range p.Options {
		options = append(options, cmp.Or(opt.Name, opt.OptionID))
	}
	title := cmp.Or(p.ToolCall.Title, "Permission required")
	msg := describeRawInput(p.ToolCall.RawInput)

	s.mu.Lock()
	s.dialogs[dialogID] = pendingPermission{reqID: reqID, options: p.Options}
	s.mu.Unlock()

	s.sink.DialogOpened(engine.Dialog{
		ID:      dialogID,
		Method:  "select",
		Title:   title,
		Message: msg,
		Options: options,
	})
}

func describeRawInput(raw json.RawMessage) string {
	args := parseInputMap(raw)
	if len(args) == 0 {
		return ""
	}
	for _, k := range []string{"CommandLine", "command", "AbsolutePath", "TargetFile", "FilePath", "path", "file_path", "DirectoryPath", "SearchPath", "Query", "query", "Url", "url"} {
		if v, ok := args[k].(string); ok && v != "" {
			return v
		}
	}
	b, err := json.MarshalIndent(args, "", "  ")
	if err != nil {
		return ""
	}
	if len(b) > 2000 {
		return string(b[:2000])
	}
	return string(b)
}

func (s *Session) RespondUI(id string, resp engine.UIResponse) error {
	s.mu.Lock()
	perm, ok := s.dialogs[id]
	if ok {
		delete(s.dialogs, id)
	}
	s.mu.Unlock()
	if !ok {
		return errors.New("no such dialog")
	}

	if resp.Cancelled {
		return s.send(map[string]any{
			"jsonrpc": "2.0",
			"id":      perm.reqID,
			"result": map[string]any{
				"outcome": map[string]any{"outcome": "cancelled"},
			},
		})
	}

	chosenID := ""
	for _, opt := range perm.options {
		if opt.Name == resp.Value || opt.OptionID == resp.Value {
			chosenID = opt.OptionID
			break
		}
	}
	if chosenID == "" && len(perm.options) > 0 {
		chosenID = perm.options[0].OptionID
	}
	return s.send(map[string]any{
		"jsonrpc": "2.0",
		"id":      perm.reqID,
		"result": map[string]any{
			"outcome": map[string]any{
				"outcome":  "selected",
				"optionId": chosenID,
			},
		},
	})
}

// ---------------------------------------------------------------------------
// Session Updates (History Replay & Live Streaming)

type sessionUpdateParams struct {
	SessionID string          `json:"sessionId"`
	Update    sessionUpdateEv `json:"update"`
}

type sessionUpdateEv struct {
	SessionUpdate string `json:"sessionUpdate"`

	// user_message_chunk, agent_message_chunk, agent_thought_chunk
	Content json.RawMessage `json:"content"`

	// tool_call, tool_call_update
	ToolCallID string            `json:"toolCallId"`
	Title      string            `json:"title"`
	Kind       string            `json:"kind"`
	Status     string            `json:"status"`
	Locations  []acpToolLocation `json:"locations"`
	RawInput   json.RawMessage   `json:"rawInput"`
	RawOutput  json.RawMessage   `json:"rawOutput"`
	Meta       json.RawMessage   `json:"_meta"`

	// available_commands_update
	AvailableCommands []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"availableCommands"`

	// config_option_update
	ConfigOptions []struct {
		ID           string `json:"id"`
		CurrentValue string `json:"currentValue"`
	} `json:"configOptions"`

	// usage_update
	Used int64 `json:"used"`
	Size int64 `json:"size"`
}

type acpToolLocation struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
}

type acpContentBlock struct {
	Type    string           `json:"type"`
	Text    string           `json:"text"`
	Path    string           `json:"path"`
	OldText string           `json:"oldText"`
	NewText string           `json:"newText"`
	Content *acpContentBlock `json:"content"`
}

var (
	t3InstructionsRE = regexp.MustCompile(`(?s)<t3_code_instructions>.*?</t3_code_instructions>\s*`)
	t3RuntimeRE      = regexp.MustCompile(`(?s)\s*<runtime_info>.*?</runtime_info>`)
	t3PRLinkingRE    = regexp.MustCompile(`(?s)\s*<pull_request_linking>.*?</pull_request_linking>`)
	userRequestRE    = regexp.MustCompile(`(?s)<user_request>\s*(.*?)\s*</user_request>`)
)

// cleanUserText strips external harness wrapper tags (such as T3 Code's
// <t3_code_instructions> and <user_request> wrappers) when replaying history.
func cleanUserText(s string) string {
	if !strings.Contains(s, "<t3_code_instructions>") && !strings.Contains(s, "<user_request>") {
		return s
	}
	if m := userRequestRE.FindStringSubmatch(s); len(m) == 2 {
		return m[1]
	}
	s = t3InstructionsRE.ReplaceAllString(s, "")
	s = t3RuntimeRE.ReplaceAllString(s, "")
	s = t3PRLinkingRE.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

func extractChunkText(raw json.RawMessage) string {
	var b acpContentBlock
	if json.Unmarshal(raw, &b) == nil {
		return b.Text
	}
	return ""
}

func (s *Session) handleSessionUpdate(rawParams json.RawMessage) {
	var p sessionUpdateParams
	if err := json.Unmarshal(rawParams, &p); err != nil {
		return
	}
	u := p.Update

	s.mu.Lock()
	switch u.SessionUpdate {
	case "available_commands_update":
		cmds := make([]engine.Command, 0, len(u.AvailableCommands))
		for _, c := range u.AvailableCommands {
			cmds = append(cmds, engine.Command{
				Name:        c.Name,
				Description: c.Description,
				Source:      "agy",
			})
		}
		s.commands = cmds
		s.mu.Unlock()
		return

	case "config_option_update":
		var newLevel string
		for _, opt := range u.ConfigOptions {
			if opt.ID == "model" && opt.CurrentValue != "" {
				s.currentModelID = opt.CurrentValue
				_, newLevel = s.catalog.stateModel(opt.CurrentValue)
			}
		}
		s.mu.Unlock()
		if newLevel != "" {
			s.sink.ThinkingChanged(newLevel)
		}
		return

	case "usage_update":
		if u.Size > 0 {
			s.contextUsage = &engine.ContextUsage{
				Tokens:  u.Used,
				Window:  u.Size,
				Percent: float64(u.Used) * 100 / float64(u.Size),
			}
		}
		s.mu.Unlock()
		return
	}

	if s.replaying {
		s.handleReplayUpdateLocked(u)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	s.handleLiveUpdate(u)
}

func (s *Session) handleReplayUpdateLocked(u sessionUpdateEv) {
	switch u.SessionUpdate {
	case "user_message_chunk":
		s.flushReplayAssistantLocked()
		text := cleanUserText(extractChunkText(u.Content))
		if text == "" {
			return
		}
		raw, _ := json.Marshal(text)
		s.messages = append(s.messages, engine.Message{
			Role:      "user",
			Content:   raw,
			Timestamp: time.Now().UnixMilli(),
		})

	case "agent_thought_chunk":
		text := extractChunkText(u.Content)
		if text == "" {
			return
		}
		s.appendAssistantBlockLocked("thinking", text)

	case "agent_message_chunk":
		text := extractChunkText(u.Content)
		if text == "" {
			return
		}
		s.appendAssistantBlockLocked("text", text)

	case "tool_call":
		if strings.HasPrefix(u.ToolCallID, "interaction_") {
			return
		}
		name, args := normalizeToolCall(u)
		s.toolCalls[u.ToolCallID] = trackedTool{name: name, args: args}
		s.appendToolCallBlockLocked(u.ToolCallID, name, args)

	case "tool_call_update":
		if strings.HasPrefix(u.ToolCallID, "interaction_") {
			return
		}
		if u.Status != "completed" && u.Status != "failed" {
			return
		}
		s.flushReplayAssistantLocked()
		t := s.toolCalls[u.ToolCallID]
		res := buildToolResultMessage(u, cmp.Or(t.name, "tool"))
		s.messages = append(s.messages, res)
	}
}

func (s *Session) ensureAssistantLocked() *engine.Message {
	if s.asst == nil {
		s.asst = &engine.Message{
			Role:      "assistant",
			Model:     s.currentModelID,
			Timestamp: time.Now().UnixMilli(),
		}
	}
	return s.asst
}

func (s *Session) appendAssistantBlockLocked(kind, text string) {
	m := s.ensureAssistantLocked()
	blocks := m.Blocks()
	if n := len(blocks); n > 0 && blocks[n-1].Type == kind {
		if kind == "thinking" {
			blocks[n-1].Thinking += text
		} else {
			blocks[n-1].Text += text
		}
	} else {
		if kind == "thinking" {
			blocks = append(blocks, engine.Block{Type: "thinking", Thinking: text})
		} else {
			blocks = append(blocks, engine.Block{Type: "text", Text: text})
		}
	}
	m.Content, _ = json.Marshal(blocks)
}

func (s *Session) appendToolCallBlockLocked(callID, name string, args json.RawMessage) {
	m := s.ensureAssistantLocked()
	blocks := m.Blocks()
	for i := range blocks {
		if blocks[i].Type == "toolCall" && blocks[i].ID == callID {
			blocks[i].Name = name
			blocks[i].Arguments = args
			m.Content, _ = json.Marshal(blocks)
			return
		}
	}
	blocks = append(blocks, engine.Block{
		Type:      "toolCall",
		ID:        callID,
		Name:      name,
		Arguments: args,
	})
	m.Content, _ = json.Marshal(blocks)
}

func (s *Session) flushReplayAssistantLocked() {
	if s.asst == nil {
		return
	}
	s.messages = append(s.messages, *s.asst)
	s.asst = nil
}

func (s *Session) finishReplayLocked() {
	s.flushReplayAssistantLocked()
	s.replaying = false
}

func (s *Session) handleLiveUpdate(u sessionUpdateEv) {
	switch u.SessionUpdate {
	case "agent_thought_chunk":
		text := extractChunkText(u.Content)
		if text == "" {
			return
		}
		s.streamDelta(engine.BlockThinking, text)

	case "agent_message_chunk":
		text := extractChunkText(u.Content)
		if text == "" {
			return
		}
		s.streamDelta(engine.BlockText, text)

	case "tool_call":
		if strings.HasPrefix(u.ToolCallID, "interaction_") {
			return
		}
		name, args := normalizeToolCall(u)
		s.mu.Lock()
		s.toolCalls[u.ToolCallID] = trackedTool{name: name, args: args}
		s.appendToolCallBlockLocked(u.ToolCallID, name, args)
		inProgress := u.Status == "in_progress"
		if inProgress {
			s.openTools[u.ToolCallID] = true
		}
		var flushed *engine.Message
		if inProgress && s.asst != nil {
			cp := *s.asst
			flushed = &cp
			s.messages = append(s.messages, cp)
			s.asst = nil
			s.liveStart = false
			s.liveIdx = -1
			s.liveKind = ""
		}
		s.mu.Unlock()

		if flushed != nil {
			s.sink.MessageEnded(*flushed)
		}
		if inProgress {
			s.sink.ToolStarted(u.ToolCallID, name, args)
		}

	case "tool_call_update":
		if strings.HasPrefix(u.ToolCallID, "interaction_") {
			return
		}
		if u.Status == "in_progress" {
			s.mu.Lock()
			t, had := s.toolCalls[u.ToolCallID]
			if !had || u.Title != "" || len(u.RawInput) > 0 {
				name, args := normalizeToolCall(u)
				t = trackedTool{name: name, args: args}
				s.toolCalls[u.ToolCallID] = t
			}
			s.appendToolCallBlockLocked(u.ToolCallID, t.name, t.args)
			alreadyOpen := s.openTools[u.ToolCallID]
			s.openTools[u.ToolCallID] = true
			var flushed *engine.Message
			if s.asst != nil {
				cp := *s.asst
				flushed = &cp
				s.messages = append(s.messages, cp)
				s.asst = nil
				s.liveStart = false
				s.liveIdx = -1
				s.liveKind = ""
			}
			s.mu.Unlock()

			if flushed != nil {
				s.sink.MessageEnded(*flushed)
			}
			if !alreadyOpen {
				s.sink.ToolStarted(u.ToolCallID, t.name, t.args)
			}
			return
		}

		if u.Status != "completed" && u.Status != "failed" {
			return
		}
		s.mu.Lock()
		t := s.toolCalls[u.ToolCallID]
		wasOpen := s.openTools[u.ToolCallID]
		delete(s.openTools, u.ToolCallID)
		var flushed *engine.Message
		if s.asst != nil {
			cp := *s.asst
			flushed = &cp
			s.messages = append(s.messages, cp)
			s.asst = nil
			s.liveStart = false
			s.liveIdx = -1
			s.liveKind = ""
		}
		res := buildToolResultMessage(u, cmp.Or(t.name, "tool"))
		s.messages = append(s.messages, res)
		s.mu.Unlock()

		if flushed != nil {
			s.sink.MessageEnded(*flushed)
		}
		if wasOpen {
			s.sink.ToolEnded(u.ToolCallID)
		}
		s.sink.MessageEnded(res)
	}
}

func (s *Session) streamDelta(kind engine.BlockKind, text string) {
	s.mu.Lock()
	needAssistantStart := !s.liveStart
	s.liveStart = true

	blockType := "text"
	if kind == engine.BlockThinking {
		blockType = "thinking"
	}
	m := s.ensureAssistantLocked()
	blocks := m.Blocks()
	needBlockStart := false
	if len(blocks) == 0 || blocks[len(blocks)-1].Type != blockType {
		needBlockStart = true
		if kind == engine.BlockThinking {
			blocks = append(blocks, engine.Block{Type: "thinking", Thinking: text})
		} else {
			blocks = append(blocks, engine.Block{Type: "text", Text: text})
		}
	} else {
		if kind == engine.BlockThinking {
			blocks[len(blocks)-1].Thinking += text
		} else {
			blocks[len(blocks)-1].Text += text
		}
	}
	m.Content, _ = json.Marshal(blocks)
	idx := len(blocks) - 1
	s.liveIdx = idx
	s.liveKind = kind
	s.mu.Unlock()

	if needAssistantStart {
		s.sink.AssistantStarted()
	}
	if needBlockStart {
		s.sink.BlockStarted(idx, kind, "")
	}
	s.sink.BlockDelta(idx, text)
}

func (s *Session) flushLiveAssistant(stopReason string) {
	s.mu.Lock()
	if s.asst == nil {
		s.mu.Unlock()
		return
	}
	if stopReason != "" && s.asst.StopReason == "" {
		s.asst.StopReason = stopReason
	}
	msg := *s.asst
	s.messages = append(s.messages, msg)
	s.asst = nil
	s.liveStart = false
	s.liveIdx = -1
	s.liveKind = ""
	s.mu.Unlock()

	s.sink.MessageEnded(msg)
}

// ---------------------------------------------------------------------------
// Tool Call & Result Normalization

func parseInputMap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		return m
	}
	var str string
	if json.Unmarshal(raw, &str) == nil && str != "" {
		if json.Unmarshal([]byte(str), &m) == nil {
			return m
		}
	}
	return nil
}

// normalizeToolCall translates an ACP tool_call notification into a concise
// tool name and normalized JSON arguments with keys that perchd's toolSummary
// and editPreview recognize ("command", "path", "oldText", "newText").
func normalizeToolCall(u sessionUpdateEv) (string, json.RawMessage) {
	args := parseInputMap(u.RawInput)
	if args == nil {
		args = map[string]any{}
	}

	// Check _meta.mcp if this is an MCP tool call.
	var meta struct {
		MCP struct {
			Server string `json:"server"`
			Tool   string `json:"tool"`
		} `json:"mcp"`
	}
	_ = json.Unmarshal(u.Meta, &meta)

	name := strings.TrimSpace(u.Title)
	for _, prefix := range []string{"Running ", "Run "} {
		if strings.HasPrefix(name, prefix) {
			name = strings.TrimPrefix(name, prefix)
			break
		}
	}
	name = strings.TrimSuffix(name, "?")

	switch {
	case meta.MCP.Tool != "":
		if meta.MCP.Server != "" {
			name = meta.MCP.Server + ":" + meta.MCP.Tool
		} else {
			name = meta.MCP.Tool
		}
	case u.Kind == "execute":
		if name != "" && args["command"] == nil && args["CommandLine"] == nil {
			args["command"] = name
		}
		name = "bash"
	case u.Kind == "edit":
		name = "edit"
	case u.Kind == "read":
		name = "read"
	case u.Kind == "search":
		name = "search"
	case u.Kind == "fetch":
		name = "fetch"
	default:
		if fields := strings.Fields(name); len(fields) > 0 {
			first := fields[0]
			if len(fields) > 1 && args["path"] == nil && args["command"] == nil {
				rest := strings.TrimSpace(strings.TrimPrefix(name, first))
				args["path"] = rest
			}
			name = first
		}
	}
	if name == "" {
		name = cmp.Or(u.Kind, "tool")
	}

	// Copy common Antigravity argument names into perchd's standard summary keys.
	if args["command"] == nil {
		for _, k := range []string{"CommandLine", "command_line", "commandLine"} {
			if s, ok := args[k].(string); ok && s != "" {
				args["command"] = s
				break
			}
		}
	}
	if args["path"] == nil && args["file_path"] == nil {
		for _, k := range []string{"AbsolutePath", "absolute_path", "TargetFile", "target_file", "FilePath", "DirectoryPath", "directory_path", "SearchDirectory", "SearchPath"} {
			if s, ok := args[k].(string); ok && s != "" {
				args["path"] = s
				break
			}
		}
		if args["path"] == nil && len(u.Locations) > 0 && u.Locations[0].Path != "" {
			args["path"] = u.Locations[0].Path
		}
	}
	if args["query"] == nil {
		for _, k := range []string{"Query", "SearchTerm"} {
			if s, ok := args[k].(string); ok && s != "" {
				args["query"] = s
				break
			}
		}
	}
	if args["url"] == nil {
		if s, ok := args["Url"].(string); ok && s != "" {
			args["url"] = s
		}
	}

	// Populate oldText / newText from diff content blocks if present so editPreview works.
	var cblocks []acpContentBlock
	if json.Unmarshal(u.Content, &cblocks) == nil {
		for _, cb := range cblocks {
			if cb.Type == "diff" {
				if cb.Path != "" && args["path"] == nil {
					args["path"] = cb.Path
				}
				if args["oldText"] == nil && args["newText"] == nil && (cb.OldText != "" || cb.NewText != "") {
					args["oldText"] = cb.OldText
					args["newText"] = cb.NewText
				}
			}
		}
	}

	b, _ := json.Marshal(args)
	return name, b
}

func buildToolResultMessage(u sessionUpdateEv, toolName string) engine.Message {
	isErr := u.Status == "failed"
	var textParts []string
	var diffStr string

	// 1. Check content blocks (e.g. diff or text content).
	var cblocks []acpContentBlock
	if len(u.Content) > 0 && json.Unmarshal(u.Content, &cblocks) == nil {
		for _, cb := range cblocks {
			switch cb.Type {
			case "diff":
				if d := unifiedDiff(cb.Path, cb.OldText, cb.NewText); d != "" {
					diffStr = d
				}
			case "content":
				if cb.Content != nil && cb.Content.Text != "" {
					textParts = append(textParts, cb.Content.Text)
				}
			case "text":
				if cb.Text != "" {
					textParts = append(textParts, cb.Text)
				}
			}
		}
	}

	// 2. Check rawOutput if textParts is still empty.
	if len(textParts) == 0 && len(u.RawOutput) > 0 {
		var str string
		if json.Unmarshal(u.RawOutput, &str) == nil {
			if str != "" {
				textParts = append(textParts, str)
			}
		} else {
			var obj map[string]any
			if json.Unmarshal(u.RawOutput, &obj) == nil {
				if out, ok := obj["combinedOutput"].(string); ok {
					textParts = append(textParts, out)
				} else if out, ok := obj["formatted_output"].(string); ok {
					textParts = append(textParts, out)
				} else {
					b, _ := json.MarshalIndent(obj, "", "  ")
					textParts = append(textParts, string(b))
				}
			} else {
				textParts = append(textParts, string(u.RawOutput))
			}
		}
	}

	contentBlocks := []engine.Block{{Type: "text", Text: strings.Join(textParts, "\n")}}
	rawContent, _ := json.Marshal(contentBlocks)
	msg := engine.Message{
		Role:       "toolResult",
		ToolCallID: u.ToolCallID,
		ToolName:   toolName,
		IsError:    isErr,
		Content:    rawContent,
		Timestamp:  time.Now().UnixMilli(),
	}
	msg.Details.Diff = diffStr
	return msg
}

func unifiedDiff(path, oldText, newText string) string {
	if oldText == newText || len(oldText)+len(newText) > 100_000 {
		return ""
	}
	split := func(s string) []string {
		if s == "" {
			return nil
		}
		lines := strings.SplitAfter(strings.TrimSuffix(s, "\n")+"\n", "\n")
		return lines[:len(lines)-1]
	}
	label := cmp.Or(path, "file")
	diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        split(oldText),
		B:        split(newText),
		FromFile: "a/" + label,
		ToFile:   "b/" + label,
		Context:  3,
	})
	if err != nil {
		return ""
	}
	return diff
}

// ---------------------------------------------------------------------------
// Engine Methods

func (s *Session) State(ctx context.Context) (engine.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, thinking := s.catalog.stateModel(s.currentModelID)
	file := s.sessionFile
	if file != "" {
		if st, err := os.Stat(file); err != nil || st.Size() == 0 {
			if walSt, walErr := os.Stat(file + "-wal"); walErr != nil || walSt.Size() == 0 {
				file = ""
			}
		}
	}
	return engine.State{
		SessionID:   s.sessionID,
		SessionFile: file,
		SessionName: s.sessionName,
		Thinking:    thinking,
		Model:       m,
	}, nil
}

func (s *Session) Messages(ctx context.Context) ([]engine.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]engine.Message(nil), s.messages...), nil
}

// Prompt sends a user message. If a turn is already active, the prompt is
// queued and dispatched immediately when the active turn finishes.
func (s *Session) Prompt(ctx context.Context, text string, mode engine.PromptMode) error {
	s.mu.Lock()
	if s.running {
		s.queue = append(s.queue, text)
		qlen := len(s.queue)
		s.mu.Unlock()
		s.sink.QueueChanged(qlen)
		return nil
	}
	s.running = true
	s.mu.Unlock()

	s.sink.RunStarted()
	go s.runLoop(text)
	return nil
}

func (s *Session) runLoop(firstText string) {
	next := firstText
	for {
		s.executeTurn(next)

		s.mu.Lock()
		if len(s.queue) == 0 {
			s.running = false
			s.mu.Unlock()
			s.sink.QueueChanged(0)
			s.sink.RunSettled()
			return
		}
		next = s.queue[0]
		s.queue = s.queue[1:]
		qlen := len(s.queue)
		s.mu.Unlock()
		s.sink.QueueChanged(qlen)
	}
}

func (s *Session) executeTurn(text string) {
	rawText, _ := json.Marshal(text)
	userMsg := engine.Message{
		Role:      "user",
		Content:   rawText,
		Timestamp: time.Now().UnixMilli(),
	}

	s.mu.Lock()
	isFirstUser := firstUserPrompt(s.messages) == ""
	s.messages = append(s.messages, userMsg)
	sid := s.sessionID
	s.mu.Unlock()

	if isFirstUser && s.cfg.Store != nil && sid != "" {
		_ = s.cfg.Store.UpdateMeta(sid, map[string]any{"firstMsg": engine.OneLine(text, 140)})
	}

	s.sink.MessageEnded(userMsg)

	var resp struct {
		StopReason string `json:"stopReason"`
	}
	err := s.call(context.Background(), "session/prompt", map[string]any{
		"sessionId": sid,
		"prompt": []map[string]any{
			{"type": "text", "text": text},
		},
	}, &resp)

	stopReason := resp.StopReason
	if stopReason == "cancelled" {
		stopReason = "aborted"
	}
	s.flushLiveAssistant(stopReason)
	if err != nil {
		s.sink.Notice("error", "Antigravity: "+err.Error())
	}
}

func (s *Session) Abort(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	queued := append([]string(nil), s.queue...)
	s.queue = nil
	sid := s.sessionID
	s.mu.Unlock()

	s.sink.QueueChanged(0)
	err := s.notify("session/cancel", map[string]any{"sessionId": sid})
	return queued, err
}

var errNoShell = errors.New("the agy backend has no direct shell; ask Antigravity to run the command")

func (s *Session) Shell(ctx context.Context, command string, exclude bool) (engine.ShellResult, error) {
	return engine.ShellResult{}, errNoShell
}

func (s *Session) AbortShell(ctx context.Context) error { return errNoShell }

func (s *Session) Compact(ctx context.Context, instructions string) error {
	return errors.New("the agy backend does not support manual context compaction")
}

func (s *Session) SetName(ctx context.Context, name string) error {
	s.mu.Lock()
	s.sessionName = name
	sid := s.sessionID
	s.mu.Unlock()

	if s.cfg.Store != nil && sid != "" {
		if err := s.cfg.Store.UpdateMeta(sid, map[string]any{"name": name}); err != nil {
			return err
		}
	}
	s.sink.NameChanged(name)
	return nil
}

func (s *Session) Commands(ctx context.Context) ([]engine.Command, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]engine.Command(nil), s.commands...), nil
}

func (s *Session) Stats(ctx context.Context) (engine.Stats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var st engine.Stats
	for _, m := range s.messages {
		switch m.Role {
		case "user":
			st.UserMessages++
		case "assistant":
			st.AssistantMessages++
			for _, b := range m.Blocks() {
				if b.Type == "toolCall" {
					st.ToolCalls++
				}
			}
		}
	}
	if s.contextUsage != nil {
		cp := *s.contextUsage
		st.Context = &cp
		st.Total = cp.Tokens
	}
	return st, nil
}

func (s *Session) Models(ctx context.Context) ([]engine.Model, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]engine.Model(nil), s.catalog.models...), nil
}

func (s *Session) SetModel(ctx context.Context, provider, id string) error {
	s.mu.Lock()
	_, curEffort := s.catalog.stateModel(s.currentModelID)
	fullID, ok := s.catalog.resolveFullID(id, curEffort)
	if !ok {
		fullID = id
	}
	s.mu.Unlock()
	return s.setFullModel(ctx, fullID)
}

func (s *Session) ThinkingLevels(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, _ := s.catalog.stateModel(s.currentModelID)
	if m == nil {
		return nil, nil
	}
	return append([]string(nil), s.catalog.efforts[m.ID]...), nil
}

func (s *Session) SetThinking(ctx context.Context, level string) error {
	s.mu.Lock()
	m, _ := s.catalog.stateModel(s.currentModelID)
	if m == nil {
		s.mu.Unlock()
		return errors.New("no model selected")
	}
	fullID, ok := s.catalog.resolveFullID(m.ID, level)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("thinking level %q is not supported by %s", level, m.Name)
	}
	if err := s.setFullModel(ctx, fullID); err != nil {
		return err
	}
	s.sink.ThinkingChanged(level)
	return nil
}

func (s *Session) setFullModel(ctx context.Context, fullID string) error {
	s.mu.Lock()
	sid := s.sessionID
	s.mu.Unlock()

	if err := s.call(ctx, "session/set_config_option", map[string]any{
		"sessionId": sid,
		"configId":  "model",
		"value":     fullID,
	}, nil); err != nil {
		return err
	}
	s.mu.Lock()
	s.currentModelID = fullID
	s.mu.Unlock()
	return nil
}
