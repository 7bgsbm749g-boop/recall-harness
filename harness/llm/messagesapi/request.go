package messagesapi

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/anthropicapi"
	"github.com/unreallabsai/unreal-agent/internal/apijson"
)

const defaultMaxOutputTokens = 128_000

func requestBody(request llm.Request, fallback bool, cacheTTL CacheTTL) ([]byte, error) {
	params, err := requestParams(request, cacheTTL)
	if err != nil {
		return nil, err
	}
	body, err := apijson.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encode messages request: %w", err)
	}
	if !fallback {
		return body, nil
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("decode messages request: %w", err)
	}
	fields["fallbacks"] = jsontext.Value(`"default"`)
	body, err = apijson.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encode messages request: %w", err)
	}
	return body, nil
}

func requestParams(request llm.Request, cacheTTL CacheTTL) (anthropicapi.CreateMessageParams, error) {
	var params anthropicapi.CreateMessageParams
	if strings.TrimSpace(request.Model.ID) == "" {
		return params, fmt.Errorf("model must be set")
	}
	params.Model = request.Model.ID
	params.MaxTokens = defaultMaxOutputTokens
	if request.Model.MaxOutputTokens != nil {
		value := *request.Model.MaxOutputTokens
		if value <= 0 || int64(int(value)) != value {
			return params, fmt.Errorf("invalid max output tokens %d", value)
		}
		params.MaxTokens = int(value)
	}
	params.Stream = new(true)
	cacheControl := anthropicapi.CacheControlEphemeral{Type: "ephemeral"}
	if cacheTTL != "" {
		cacheControl.Ttl = new(anthropicapi.CacheControlEphemeralTtl(cacheTTL))
	}
	params.CacheControl = new(anthropicapi.CreateMessageParams_CacheControl)
	if err := setUnion(params.CacheControl, cacheControl); err != nil {
		return params, err
	}
	if request.Model.ReasoningEffort != "" {
		if !request.Model.ReasoningEffort.Valid() {
			return params, fmt.Errorf("unsupported reasoning effort %q", request.Model.ReasoningEffort)
		}
		params.Thinking = new(anthropicapi.ThinkingConfigParam)
		if err := setUnion(params.Thinking, anthropicapi.ThinkingConfigAdaptive{Type: "adaptive"}); err != nil {
			return params, err
		}
		params.OutputConfig = &anthropicapi.OutputConfig{Effort: new(anthropicapi.EffortLevel(request.Model.ReasoningEffort))}
	}
	var err error
	params.Messages, params.System, err = requestInput(request.Input)
	if err != nil {
		return params, err
	}
	tools, err := requestTools(request.Tools)
	if err != nil {
		return params, err
	}
	if len(tools) != 0 {
		params.Tools = &tools
	}
	return params, nil
}

type requestCall struct {
	name     string
	answered bool
}

func requestInput(items []llm.Item) ([]anthropicapi.InputMessage, *anthropicapi.CreateMessageParams_System, error) {
	var system *anthropicapi.CreateMessageParams_System
	start := 0
	if len(items) != 0 && items[0].Type == llm.ItemMessage {
		if message, ok := items[0].Data.(llm.Message); ok && message.Role == llm.RoleSystem {
			system = new(anthropicapi.CreateMessageParams_System)
			if err := system.FromCreateMessageParamsSystem1([]anthropicapi.RequestTextBlock{{Type: "text", Text: message.Text}}); err != nil {
				return nil, nil, err
			}
			start = 1
		}
	}
	messages := make([]anthropicapi.InputMessage, 0)
	calls := make(map[string]*requestCall)
	var pending []string
	for start < len(items) {
		role, err := requestItemRole(items[start])
		if err != nil {
			return nil, nil, fmt.Errorf("input item %d: %w", start, err)
		}
		end := start + 1
		for end < len(items) {
			nextRole, err := requestItemRole(items[end])
			if err != nil {
				return nil, nil, fmt.Errorf("input item %d: %w", end, err)
			}
			if nextRole != role {
				break
			}
			end++
		}
		var blocks []anthropicapi.InputContentBlock
		if role == llm.RoleAssistant {
			for index := start; index < end; index++ {
				item := items[index]
				block, err := requestInputBlock(item)
				if err != nil {
					return nil, nil, fmt.Errorf("input item %d: %w", index, err)
				}
				if item.Type == llm.ItemToolCall {
					call := item.Data.(llm.ToolCall)
					if _, exists := calls[call.CallID]; exists {
						return nil, nil, fmt.Errorf("input item %d: duplicate tool call %q", index, call.CallID)
					}
					calls[call.CallID] = &requestCall{name: call.Name}
					pending = append(pending, call.CallID)
				}
				blocks = append(blocks, block)
			}
		} else {
			var err error
			blocks, err = requestUserItems(items[start:end], start, calls, pending)
			if err != nil {
				return nil, nil, err
			}
			pending = nil
		}
		var content anthropicapi.InputMessage_Content
		if err := content.FromInputMessageContent1(blocks); err != nil {
			return nil, nil, err
		}
		messages = append(messages, anthropicapi.InputMessage{Role: anthropicapi.InputMessageRole(role), Content: content})
		start = end
	}
	if len(pending) != 0 {
		return nil, nil, fmt.Errorf("missing immediately following results for tool calls %q", pending)
	}
	if len(messages) == 0 {
		return nil, nil, fmt.Errorf("messages must not be empty")
	}
	return messages, system, nil
}

func requestItemRole(item llm.Item) (llm.Role, error) {
	if err := item.Validate(); err != nil {
		return "", err
	}
	switch item.Type {
	case llm.ItemMessage:
		role := item.Data.(llm.Message).Role
		switch role {
		case llm.RoleUser, llm.RoleAssistant:
			return role, nil
		case llm.RoleSystem:
			return "", fmt.Errorf("system message must be the first input item")
		default:
			return "", fmt.Errorf("unsupported role %q", role)
		}
	case llm.ItemToolResult:
		return llm.RoleUser, nil
	default:
		return llm.RoleAssistant, nil
	}
}

func requestUserItems(items []llm.Item, offset int, calls map[string]*requestCall, pending []string) ([]anthropicapi.InputContentBlock, error) {
	results := make(map[string]anthropicapi.InputContentBlock, len(pending))
	var ordinary []anthropicapi.InputContentBlock
	for index, item := range items {
		var blocks []anthropicapi.InputContentBlock
		var err error
		if item.Type == llm.ItemMessage {
			message := item.Data.(llm.Message)
			var content []llm.ToolResultOutput
			if message.Text != "" || message.Media == nil || len(message.Media.Images) == 0 {
				content = append(content, llm.ToolResultOutput{Kind: llm.ToolResultText, Value: message.Text})
			}
			if message.Media != nil {
				for _, url := range message.Media.Images {
					content = append(content, llm.ToolResultOutput{Kind: llm.ToolResultImage, Value: url})
				}
			}
			blocks, err = requestToolOutput(content)
		} else {
			result := item.Data.(llm.ToolResult)
			call, exists := calls[result.CallID]
			if !exists {
				return nil, fmt.Errorf("input item %d: result for unknown tool call %q", offset+index, result.CallID)
			}
			blocks, err = requestToolOutput(result.Output)
			if err != nil {
				return nil, fmt.Errorf("input item %d: %w", offset+index, err)
			}
			if !call.answered {
				var content anthropicapi.RequestToolResultBlock_Content
				if err := setUnion(&content, blocks); err != nil {
					return nil, fmt.Errorf("input item %d: %w", offset+index, err)
				}
				var block anthropicapi.InputContentBlock
				if err := setUnion(&block, anthropicapi.RequestToolResultBlock{
					Type: "tool_result", ToolUseId: result.CallID, Content: &content,
				}); err != nil {
					return nil, fmt.Errorf("input item %d: %w", offset+index, err)
				}
				results[result.CallID] = block
				call.answered = true
				continue
			}
			status := "completed"
			if result.Running {
				status = "running"
			}
			var header anthropicapi.InputContentBlock
			err = setUnion(&header, anthropicapi.RequestTextBlock{Type: "text", Text: fmt.Sprintf("%q (call %q, %s)\n", call.name, result.CallID, status)})
			blocks = append([]anthropicapi.InputContentBlock{header}, blocks...)
		}
		if err != nil {
			return nil, fmt.Errorf("input item %d: %w", offset+index, err)
		}
		ordinary = append(ordinary, blocks...)
	}
	// The first result answers the call, even when it only reports running.
	// Emit those results in call order; later updates must not rewrite history.
	blocks := make([]anthropicapi.InputContentBlock, 0, len(pending)+len(ordinary))
	for _, id := range pending {
		block, exists := results[id]
		if !exists {
			return nil, fmt.Errorf("missing immediately following result for tool call %q", id)
		}
		blocks = append(blocks, block)
	}
	return append(blocks, ordinary...), nil
}

func requestInputBlock(item llm.Item) (anthropicapi.InputContentBlock, error) {
	var block anthropicapi.InputContentBlock
	switch item.Type {
	case llm.ItemMessage:
		message := item.Data.(llm.Message)
		return block, setUnion(&block, anthropicapi.RequestTextBlock{Type: "text", Text: message.Text})
	case llm.ItemToolCall:
		call := item.Data.(llm.ToolCall)
		if call.CallID == "" || call.Name == "" {
			return block, fmt.Errorf("tool call must have an ID and name")
		}
		arguments, err := requestToolArguments(call.Arguments)
		if err != nil {
			return block, fmt.Errorf("encode tool call %q arguments: %w", call.CallID, err)
		}
		return block, setUnion(&block, anthropicapi.RequestToolUseBlock{Type: "tool_use", Id: call.CallID, Name: call.Name, Input: arguments})
	case llm.ItemProvider:
		provider := item.Data.(llm.ProviderItem)
		return block, block.UnmarshalJSON(provider.Raw)
	default:
		return block, fmt.Errorf("unsupported assistant item type %q", item.Type)
	}
}

func requestToolArguments(arguments string) (map[string]any, error) {
	value := jsontext.Value(arguments)
	if value.Kind() != jsontext.KindBeginObject || !value.IsValid() {
		// Keep malformed arguments visible alongside the translator's error,
		// while satisfying the provider's object-only history format.
		return map[string]any{"invalid_arguments": arguments}, nil
	}
	var object map[string]any
	if err := apijson.Unmarshal(value, &object); err != nil {
		return nil, err
	}
	return object, nil
}

func requestToolOutput(output []llm.ToolResultOutput) ([]anthropicapi.InputContentBlock, error) {
	blocks := make([]anthropicapi.InputContentBlock, 0, len(output))
	for _, part := range output {
		var block anthropicapi.InputContentBlock
		switch part.Kind {
		case llm.ToolResultText:
			if err := setUnion(&block, anthropicapi.RequestTextBlock{Type: "text", Text: part.Value}); err != nil {
				return nil, err
			}
		case llm.ToolResultImage:
			source, err := requestImageSource(part.Value)
			if err != nil {
				return nil, err
			}
			if err := setUnion(&block, anthropicapi.RequestImageBlock{Type: "image", Source: source}); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported tool result kind %q", part.Kind)
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

func requestImageSource(value string) (anthropicapi.RequestImageBlock_Source, error) {
	var source anthropicapi.RequestImageBlock_Source
	if encoded, ok := strings.CutPrefix(value, "data:"); ok {
		header, data, found := strings.Cut(encoded, ",")
		mediaType, base64Encoded := strings.CutSuffix(header, ";base64")
		if !found || !base64Encoded || !slices.Contains([]string{"image/png", "image/jpeg", "image/gif", "image/webp"}, mediaType) {
			return source, fmt.Errorf("unsupported image data URL")
		}
		decoded, err := base64.StdEncoding.DecodeString(data)
		if err != nil || len(decoded) == 0 {
			return source, fmt.Errorf("image data URL must contain valid nonempty base64 data")
		}
		return source, setUnion(&source, anthropicapi.Base64ImageSource{Type: "base64", MediaType: anthropicapi.Base64ImageSourceMediaType(mediaType), Data: decoded})
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return source, fmt.Errorf("image must be an HTTP(S) URL or a supported base64 data URL")
	}
	return source, setUnion(&source, anthropicapi.URLImageSource{Type: "url", Url: value})
}

func requestTools(source []llm.Tool) ([]anthropicapi.CreateMessageParams_Tools_Item, error) {
	tools := make([]anthropicapi.CreateMessageParams_Tools_Item, 0, len(source))
	for index, tool := range source {
		var converted anthropicapi.CreateMessageParams_Tools_Item
		switch tool.Type {
		case llm.ToolFunction:
			if tool.Name == "" {
				return nil, fmt.Errorf("tool %d: name must be set", index)
			}
			var schema anthropicapi.InputSchema
			encoded, err := apijson.Marshal(tool.Parameters)
			if err != nil {
				return nil, fmt.Errorf("tool %d: encode schema: %w", index, err)
			}
			if err := apijson.Unmarshal(encoded, &schema); err != nil {
				return nil, fmt.Errorf("tool %d: decode schema: %w", index, err)
			}
			if schema.Type != "object" {
				return nil, fmt.Errorf("tool %d: input schema must have type object", index)
			}
			function := anthropicapi.Tool{Type: new(anthropicapi.ToolTypeCustom), Name: tool.Name, InputSchema: schema, Strict: new(false)}
			if tool.Description != "" {
				function.Description = &tool.Description
			}
			if err := converted.FromTool(function); err != nil {
				return nil, fmt.Errorf("tool %d: %w", index, err)
			}
		default:
			return nil, fmt.Errorf("tool %d: unsupported tool type %q", index, tool.Type)
		}
		tools = append(tools, converted)
	}
	return tools, nil
}

func setUnion(destination json.Unmarshaler, source any) error {
	body, err := apijson.Marshal(source)
	if err != nil {
		return err
	}
	return destination.UnmarshalJSON(body)
}
