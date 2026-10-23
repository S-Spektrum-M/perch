// Package engine is the contract between perchd and the agent that runs
// sessions. perchd owns the browser-facing session (transcript mirror, live
// updates, rendering); a Backend owns the agent processes and their storage.
//
// The transcript model (Message, Block) currently mirrors pi's AgentMessage,
// because pi is the first backend. Other backends translate into it.
package engine

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrSessionBusy means another process already has the session open.
var ErrSessionBusy = errors.New("session is already open in another process")

// Backend lists sessions and starts engines for them.
type Backend interface {
	// Name identifies the backend in user-facing messages, e.g. "pi".
	Name() string

	// List returns the stored sessions, most recently modified first.
	List() []SessionInfo
	// Find looks a stored session up by its ID.
	Find(id string) (SessionInfo, bool)

	// Open starts an engine for an existing session, in info.Cwd.
	Open(info SessionInfo, sink Sink) (Engine, error)
	// Create starts an engine for a new session in dir.
	Create(dir, name string, sink Sink) (Engine, error)
	// Duplicate starts an engine for a new session copied from info.
	Duplicate(info SessionInfo, sink Sink) (Engine, error)

	// SaveModelDefaults makes model and thinking the defaults for new sessions.
	SaveModelDefaults(ctx context.Context, model Model, thinking string) error
}

// ModelCatalog is an optional backend capability: the models a new session
// can be started with, listed without starting one.
type ModelCatalog interface {
	Models(ctx context.Context) ([]Model, error)
}

// SessionNamer is an optional backend capability for naming an unnamed session
// from a user prompt without changing that session's model or transcript.
type SessionNamer interface {
	SuggestSessionName(ctx context.Context, dir, prompt string) (string, error)
}

// Engine runs one session. Methods may be called concurrently. Progress is
// reported through the Sink the engine was started with, not return values.
type Engine interface {
	// State and Messages describe the session; perchd calls them after start.
	State(ctx context.Context) (State, error)
	Messages(ctx context.Context) ([]Message, error)

	// Prompt sends a user message. While a run is in progress, mode says
	// whether it interjects (PromptSteer) or waits (PromptFollowUp).
	Prompt(ctx context.Context, text string, mode PromptMode) error
	// Abort stops the current run, first clearing queued messages, and
	// returns their text so the user can edit and resend it.
	Abort(ctx context.Context) (queued []string, err error)

	// Shell runs a command directly, outside a run. Unless exclude is set,
	// its output is given to the model with the next prompt.
	Shell(ctx context.Context, command string, exclude bool) (ShellResult, error)
	AbortShell(ctx context.Context) error

	// Compact summarizes the transcript to free context. It reports
	// CompactionStarted, CompactionEnded and TranscriptReplaced.
	Compact(ctx context.Context, instructions string) error
	SetName(ctx context.Context, name string) error
	// Commands lists what can be run by sending "/name" as a prompt.
	Commands(ctx context.Context) ([]Command, error)
	Stats(ctx context.Context) (Stats, error)

	// Models lists the models that can be used now.
	Models(ctx context.Context) ([]Model, error)
	SetModel(ctx context.Context, provider, id string) error
	// ThinkingLevels lists the levels the current model supports.
	ThinkingLevels(ctx context.Context) ([]string, error)
	SetThinking(ctx context.Context, level string) error

	// RespondUI answers a dialog reported by Sink.DialogOpened.
	RespondUI(id string, resp UIResponse) error

	// Close stops the engine. Sink.Stopped is reported before Close returns.
	Close()
}

// Sink receives an engine's progress. Calls may come from several
// goroutines, so implementations must be safe for concurrent use. They must
// not call back into the engine.
type Sink interface {
	// A run is one prompt and everything the agent does in response.
	RunStarted()
	RunSettled()

	// The assistant message currently being streamed. Blocks are numbered
	// by their index in the final message.
	AssistantStarted()
	BlockStarted(idx int, kind BlockKind, toolName string)
	BlockDelta(idx int, text string)
	// MessageEnded appends a complete message to the transcript.
	MessageEnded(m Message)

	// Tool calls. partial is a replaceable snapshot of the output so far,
	// not a delta. Updates may arrive without a start.
	ToolStarted(callID, name string, args json.RawMessage)
	ToolUpdated(callID, name string, args json.RawMessage, partial Message)
	ToolEnded(callID string)

	Retrying(attempt, max int, reason string)
	RetryEnded()
	CompactionStarted()
	CompactionEnded()
	// TranscriptReplaced swaps the whole transcript, e.g. after compaction.
	TranscriptReplaced(msgs []Message)

	QueueChanged(n int)
	NameChanged(name string)
	ThinkingChanged(level string)
	// Notice shows a transient message; kind is info, warning or error.
	Notice(kind, text string)

	// Interaction requested by the agent or its extensions.
	DialogOpened(d Dialog)
	DialogClosed(id string)
	// StatusChanged sets a status-bar entry; empty text removes it.
	StatusChanged(key, text string)
	// WidgetChanged sets a widget's lines; nil removes it.
	WidgetChanged(key string, lines []string)
	// EditorText replaces the prompt box contents.
	EditorText(text string)

	// Stopped reports that the engine stopped, with a human-readable reason.
	Stopped(reason string)
}

// BlockKind is the kind of a streamed assistant block.
type BlockKind string

const (
	BlockText     BlockKind = "text"
	BlockThinking BlockKind = "thinking"
	BlockTool     BlockKind = "tool"
)

// PromptMode says how a prompt sent during a run is delivered.
type PromptMode string

const (
	PromptNow      PromptMode = ""         // no run in progress
	PromptSteer    PromptMode = "steer"    // after the current tool calls
	PromptFollowUp PromptMode = "followUp" // after the run finishes
)

// State is a session's current settings.
type State struct {
	SessionID   string
	SessionFile string // "" until the session is stored
	SessionName string
	Thinking    string
	Model       *Model // nil when none is selected
}

type Model struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

type ShellResult struct {
	Output    string
	ExitCode  *int // nil when the command didn't exit normally
	Cancelled bool
}

type Command struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
}

type Stats struct {
	UserMessages, AssistantMessages, ToolCalls  int
	Input, Output, CacheRead, CacheWrite, Total int64
	Cost                                        float64
	Context                                     *ContextUsage // nil when unknown
	Account                                     string        // who the session runs as; "" when not applicable
}

type ContextUsage struct {
	Tokens, Window int64
	Percent        float64
}

// Dialog asks the user to choose, confirm or type something.
type Dialog struct {
	ID          string
	Method      string // select, confirm, input, editor
	Title       string
	Message     string
	Placeholder string
	Prefill     string
	Options     []string
}

// UIResponse answers a Dialog. Exactly one of the fields applies.
type UIResponse struct {
	Cancelled bool
	Confirmed *bool  // confirm dialogs
	Value     string // everything else
}
