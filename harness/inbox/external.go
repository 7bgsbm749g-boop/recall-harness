package inbox

import (
	"encoding/json/v2"
	"fmt"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// ExternalMessage adds attachments without changing persisted text-only inputs.
type ExternalMessage struct {
	Text  string            `json:"text"`
	Media *llm.MessageMedia `json:"media,omitzero"`
}

func (input Input) DecodeExternalMessage() (llm.Message, error) {
	message := llm.Message{Role: llm.RoleUser}
	if input.Kind != InputExternal {
		return message, fmt.Errorf("external input %q has input kind %q", input.ID, input.Kind)
	}
	var text string
	if err := json.Unmarshal(input.Payload, &text); err == nil {
		message.Text = text
		return message, nil
	}
	var external ExternalMessage
	if err := json.Unmarshal(input.Payload, &external, json.RejectUnknownMembers(true)); err != nil {
		return message, fmt.Errorf("decode external input %q: %w", input.ID, err)
	}
	message.Text, message.Media = external.Text, external.Media
	if message.Text == "" && (message.Media == nil || len(message.Media.Images) == 0) {
		return message, fmt.Errorf("external input %q has no text or image payload", input.ID)
	}
	if err := (llm.Item{Type: llm.ItemMessage, Data: message}).Validate(); err != nil {
		return message, err
	}
	return message, nil
}
