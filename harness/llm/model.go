package llm

import "encoding/json/jsontext"

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

type ItemType string

const (
	ItemMessage    ItemType = "message"
	ItemToolCall   ItemType = "tool_call"
	ItemToolResult ItemType = "tool_result"
	ItemProvider   ItemType = "provider"
)

type Item struct {
	ProviderID string
	Type       ItemType
	Data       any
}

type Message struct {
	Role  Role
	Text  string
	Phase string
	Media *MessageMedia `json:",omitzero"`
}

// Media is optional so existing text-only message encodings remain unchanged.
// Images are provider-neutral image URLs, including inline base64 data URLs.
type MessageMedia struct {
	Images []string
}

type ToolCall struct {
	CallID    string
	Name      string
	Arguments string
}

type ToolResultKind string

const (
	ToolResultText  ToolResultKind = "text"
	ToolResultImage ToolResultKind = "image"
)

type ToolResultOutput struct {
	Kind  ToolResultKind
	Value string
}

type ToolResult struct {
	CallID  string
	Output  []ToolResultOutput
	Running bool
}

// Raw is required and authoritative for replay. Display is a presentation projection.
type ProviderItem struct {
	Type    string
	Raw     jsontext.Value
	Display *ProviderDisplay `json:",omitzero"`
}

type ProviderDisplayKind string

const ProviderDisplayReasoning ProviderDisplayKind = "reasoning"

type ProviderDisplay struct {
	Kind ProviderDisplayKind
	Text string
}

type ToolType string

const (
	ToolFunction ToolType = "function"
	ToolHosted   ToolType = "hosted"
)

type Tool struct {
	Type        ToolType
	Name        string
	Description string
	Parameters  map[string]any
}

type Model struct {
	ID                  string
	CompactionThreshold int64
	MaxOutputTokens     *int64
	ReasoningEffort     ReasoningEffort
}

type ReasoningEffort string

const (
	ReasoningEffortLow    ReasoningEffort = "low"
	ReasoningEffortMedium ReasoningEffort = "medium"
	ReasoningEffortHigh   ReasoningEffort = "high"
	ReasoningEffortXHigh  ReasoningEffort = "xhigh"
	ReasoningEffortMax    ReasoningEffort = "max"
)

func (effort ReasoningEffort) Valid() bool {
	switch effort {
	case ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortXHigh, ReasoningEffortMax:
		return true
	default:
		return false
	}
}

type Request struct {
	Model Model
	Input []Item
	Tools []Tool
}

type StopReason string

const (
	StopComplete        StopReason = "complete"
	StopMaxOutputTokens StopReason = "max_output_tokens"
	StopRefused         StopReason = "refused"
)

type Response struct {
	ID      string
	Stop    StopReason
	Output  []Item `json:",omitzero"`
	Usage   Usage
	Failure *Failure
}

type Usage struct {
	TokenUsage
	ByModel map[string]TokenUsage `json:",omitzero"` // Empty key means the model was not reported.
	Raw     jsontext.Value        `json:",omitzero"`
}

// InputTokens includes CachedInputTokens and CacheWriteInputTokens.
// OutputTokens includes ReasoningTokens.
type TokenUsage struct {
	InputTokens           int64
	CachedInputTokens     int64
	CacheWriteInputTokens int64
	OutputTokens          int64
	ReasoningTokens       int64
}

func (usage TokenUsage) Add(other TokenUsage) TokenUsage {
	return TokenUsage{
		InputTokens:           usage.InputTokens + other.InputTokens,
		CachedInputTokens:     usage.CachedInputTokens + other.CachedInputTokens,
		CacheWriteInputTokens: usage.CacheWriteInputTokens + other.CacheWriteInputTokens,
		OutputTokens:          usage.OutputTokens + other.OutputTokens,
		ReasoningTokens:       usage.ReasoningTokens + other.ReasoningTokens,
	}
}

type Failure struct {
	Code    string
	Message string
}
