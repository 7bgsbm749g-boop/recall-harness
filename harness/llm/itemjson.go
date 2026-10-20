package llm

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
)

type itemJSON Item

const legacyItemReasoning ItemType = "reasoning"

func (item Item) Validate() error {
	switch item.Type {
	case ItemMessage:
		message, ok := item.Data.(Message)
		if !ok {
			return fmt.Errorf("message data must be llm.Message, got %T", item.Data)
		}
		if message.Media != nil {
			if message.Role != RoleUser {
				return fmt.Errorf("image attachments require a user message")
			}
			for _, image := range message.Media.Images {
				if strings.TrimSpace(image) == "" {
					return fmt.Errorf("image URL must not be empty")
				}
			}
		}
	case ItemToolCall:
		if _, ok := item.Data.(ToolCall); !ok {
			return fmt.Errorf("tool call data must be llm.ToolCall, got %T", item.Data)
		}
	case ItemToolResult:
		if _, ok := item.Data.(ToolResult); !ok {
			return fmt.Errorf("tool result data must be llm.ToolResult, got %T", item.Data)
		}
	case ItemProvider:
		provider, ok := item.Data.(ProviderItem)
		if !ok {
			return fmt.Errorf("provider data must be llm.ProviderItem, got %T", item.Data)
		}
		if len(provider.Raw) == 0 || provider.Raw.Kind() == jsontext.KindNull {
			return fmt.Errorf("provider item Raw is required")
		}
	default:
		return fmt.Errorf("unsupported item type %q", item.Type)
	}
	return nil
}

func (item Item) MarshalJSON() ([]byte, error) {
	if err := item.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(itemJSON(item))
}

func (item *Item) UnmarshalJSON(encoded []byte) error {
	var decoded struct {
		itemJSON
		Data jsontext.Value
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return err
	}

	data, err := decodeItemData(decoded.Type, decoded.Data)
	if err != nil {
		return err
	}
	decoded.itemJSON.Data = data
	if decoded.Type == legacyItemReasoning {
		decoded.Type = ItemProvider
	} else if err := Item(decoded.itemJSON).Validate(); err != nil {
		return err
	}
	*item = Item(decoded.itemJSON)
	return nil
}

func decodeItemData(kind ItemType, encoded jsontext.Value) (any, error) {
	if encoded.Kind() == jsontext.KindNull {
		return nil, fmt.Errorf("%s data must not be null", kind)
	}
	switch kind {
	case ItemMessage:
		var value Message
		if err := json.Unmarshal(encoded, &value); err != nil {
			return nil, fmt.Errorf("decode message data: %w", err)
		}
		return value, nil
	case ItemToolCall:
		var value ToolCall
		if err := json.Unmarshal(encoded, &value); err != nil {
			return nil, fmt.Errorf("decode tool call data: %w", err)
		}
		return value, nil
	case ItemToolResult:
		var value ToolResult
		if err := json.Unmarshal(encoded, &value); err != nil {
			return nil, fmt.Errorf("decode tool result data: %w", err)
		}
		return value, nil
	case ItemProvider:
		var value ProviderItem
		if err := json.Unmarshal(encoded, &value); err != nil {
			return nil, fmt.Errorf("decode provider data: %w", err)
		}
		return value, nil
	case legacyItemReasoning:
		var value legacyReasoning
		if err := json.Unmarshal(encoded, &value); err != nil {
			return nil, fmt.Errorf("decode reasoning data: %w", err)
		}
		provider := ProviderItem{Raw: value.Raw}
		if value.Raw.Kind() == jsontext.KindBeginObject {
			var header map[string]jsontext.Value
			if err := json.Unmarshal(value.Raw, &header); err != nil {
				return nil, fmt.Errorf("decode legacy reasoning payload: %w", err)
			}
			if kind := header["type"]; kind.Kind() == jsontext.KindString {
				if err := json.Unmarshal(kind, &provider.Type); err != nil {
					return nil, fmt.Errorf("decode legacy reasoning type: %w", err)
				}
			}
		}
		if len(value.Summary) != 0 {
			provider.Display = &ProviderDisplay{Kind: ProviderDisplayReasoning, Text: strings.Join(value.Summary, "\n\n")}
		}
		return provider, nil
	default:
		return nil, fmt.Errorf("unsupported item type %q", kind)
	}
}

type legacyReasoning struct {
	Summary []string
	Raw     jsontext.Value
}
