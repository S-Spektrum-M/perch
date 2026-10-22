package pi

import (
	"encoding/json"
	"strings"
)

// Message is a loose decoding of pi's AgentMessage union. Fields that only
// some roles carry are simply left empty for the others.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []Block

	// assistant
	Usage        *TokenUsage `json:"usage,omitempty"`
	Model        string      `json:"model"`
	StopReason   string      `json:"stopReason"`
	ErrorMessage string      `json:"errorMessage"`

	// toolResult
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	IsError    bool   `json:"isError"`
	Details    struct {
		Diff           string `json:"diff"`
		FullOutputPath string `json:"fullOutputPath"`
	} `json:"details"`

	// bashExecution
	Command  string `json:"command"`
	Output   string `json:"output"`
	ExitCode *int   `json:"exitCode"`

	// custom
	CustomType string `json:"customType"`
	Display    *bool  `json:"display"`

	// branchSummary, compactionSummary
	Summary string `json:"summary"`

	Timestamp int64 `json:"timestamp"`
}

// TokenUsage is the provider-reported usage for one assistant step.
type TokenUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
}

// Block is one content block of a message.
type Block struct {
	Type      string          `json:"type"` // text, thinking, toolCall, image
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Redacted  bool            `json:"redacted"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	MimeType  string          `json:"mimeType"`
}

// Blocks normalises Content, which may be a bare string.
func (m Message) Blocks() []Block {
	if len(m.Content) == 0 {
		return nil
	}
	if m.Content[0] == '"' {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			return []Block{{Type: "text", Text: s}}
		}
		return nil
	}
	var bs []Block
	_ = json.Unmarshal(m.Content, &bs)
	return bs
}

// PlainText joins the text blocks of a message.
func (m Message) PlainText() string {
	var parts []string
	for _, b := range m.Blocks() {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// LastAssistantText returns the text of the latest assistant message that has
// any, mirroring pi's get_last_assistant_text.
func LastAssistantText(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			if t := msgs[i].PlainText(); t != "" {
				return t
			}
		}
	}
	return ""
}
