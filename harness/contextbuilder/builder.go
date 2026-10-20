package contextbuilder

import (
	_ "embed"
	"fmt"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// ToolCallRunningPayload is the result a running call shows until it completes.
const ToolCallRunningPayload = "Tool call is still running. Its result arrives in a later turn: continue with independent work, or end your turn to wait for it."

//go:embed prompts/preamble.md
var preambleFile string

var preamble = strings.TrimSpace(preambleFile)

type builder struct {
	request         llm.Request
	preamble        string
	systemPrompt    string
	committedPrefix []llm.Item
	stagedSuffix    []llm.Item
	prefixTokens    []prefixToken
}

type prefixToken struct {
	committedPrefixIndex int // Exclusive end of the response in committedPrefix.
	Tokens               int64
}

var _ Builder = (*builder)(nil)

func NewBuilder(skills ...tool.Skill) Builder {
	currentPreamble := preamble
	if skillPrompt := formatSkillsForPrompt(skills); skillPrompt != "" {
		currentPreamble += "\n\n" + skillPrompt
	}
	return newBuilder(currentPreamble)
}

func newBuilder(currentPreamble string) *builder {
	current := &builder{preamble: currentPreamble, committedPrefix: make([]llm.Item, 1)}
	current.SetSystemPrompt("")
	return current
}

func (current *builder) AddExternalInput(input inbox.Input) error {
	if input.Kind != inbox.InputExternal {
		return fmt.Errorf(
			"external input %q has input kind %q",
			input.ID,
			input.Kind,
		)
	}

	message, err := input.DecodeExternalMessage()
	if err != nil {
		return err
	}
	current.stageItems(llm.Item{
		Type: llm.ItemMessage,
		Data: message,
	})
	return nil
}

func (current *builder) SetModel(model llm.Model) {
	current.request.Model = model
}

func (current *builder) AddControlMessage(request inbox.ControlMessage) {
	switch request.Mode {
	case inbox.UpdateSettings:
		settings := request.Parameters.(inbox.Settings)
		if settings.Model != "" && current.request.Model.ID != settings.Model {
			current.request.Model.ID = settings.Model
			current.prefixTokens = nil
		}
		if settings.CompactionThreshold != nil {
			current.request.Model.CompactionThreshold = *settings.CompactionThreshold
		}
		if settings.MaxOutputTokens != nil {
			current.request.Model.MaxOutputTokens = settings.MaxOutputTokens
		}
		if settings.ReasoningEffort != "" {
			current.request.Model.ReasoningEffort = settings.ReasoningEffort
		}
	case inbox.Heartbeat:
		current.stagedSuffix = append(current.stagedSuffix, llm.Item{
			Type: llm.ItemMessage,
			Data: llm.Message{Role: llm.RoleUser, Text: request.Reason},
		})
	}
}

func (current *builder) SetSystemPrompt(prompt string) {
	current.systemPrompt = prompt
	current.committedPrefix[0] = llm.Item{Type: llm.ItemMessage, Data: llm.Message{
		Role: llm.RoleSystem,
		Text: strings.TrimSpace(current.preamble + "\n\n" + current.systemPrompt),
	}}
}

func (current *builder) AddModelResponse(response llm.Response) {
	current.committedPrefix = append(current.committedPrefix, response.Output...)
	current.prefixTokens = append(current.prefixTokens, prefixToken{
		committedPrefixIndex: len(current.committedPrefix),
		Tokens:               response.Usage.InputTokens + response.Usage.OutputTokens,
	})
}

func (current *builder) AddTool(tool llm.Tool) {
	current.request.Tools = append(current.request.Tools, tool)
}

func (current *builder) AddToolResult(
	callID string,
	payload []llm.ToolResultOutput,
	running bool,
) {
	runningIndex := slices.IndexFunc(current.stagedSuffix, func(item llm.Item) bool {
		if item.Type != llm.ItemToolResult {
			return false
		}
		result := item.Data.(llm.ToolResult)
		return result.CallID == callID && result.Running
	})
	if running {
		payload = []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: ToolCallRunningPayload}}
	}
	resultItem := llm.Item{
		Type: llm.ItemToolResult,
		Data: llm.ToolResult{CallID: callID, Output: payload, Running: running},
	}
	if runningIndex != -1 {
		current.stagedSuffix[runningIndex] = resultItem
		return
	}
	current.stagedSuffix = append(current.stagedSuffix, resultItem)
}

func (current *builder) stageItems(items ...llm.Item) {
	current.stagedSuffix = append(current.stagedSuffix, items...)
}

func (current *builder) Commit() {
	current.committedPrefix = append(current.committedPrefix, current.stagedSuffix...)
	current.stagedSuffix = nil
}

func (current *builder) Build() (Result, error) {
	request := current.request
	input := make([]llm.Item, 0, len(current.committedPrefix)+len(current.stagedSuffix))
	input = append(input, current.committedPrefix...)
	input = append(input, current.stagedSuffix...)
	request.Input = input
	request.Tools = append([]llm.Tool(nil), request.Tools...)
	return Result{Request: request}, nil
}
