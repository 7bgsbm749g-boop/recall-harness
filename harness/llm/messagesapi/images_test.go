package messagesapi

import (
	"encoding/json/v2"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestUserImageBecomesAnAnthropicImageBlock(t *testing.T) {
	url := "data:image/png;base64,aGVsbG8="
	for _, text := range []string{"Inspect the screenshot.", ""} {
		body, err := requestBody(llm.Request{Model: llm.Model{ID: "fixture"}, Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: text, Media: &llm.MessageMedia{Images: []string{url}}}}}}, false, "")
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type   string `json:"type"`
					Source struct {
						Type string `json:"type"`
						MIME string `json:"media_type"`
						Data string `json:"data"`
					} `json:"source"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err = json.Unmarshal(body, &wire); err != nil {
			t.Fatal(err)
		}
		want := 1
		if text != "" {
			want = 2
		}
		if len(wire.Messages) != 1 || wire.Messages[0].Role != "user" || len(wire.Messages[0].Content) != want {
			t.Fatalf("image missing: %s", body)
		}
		last := wire.Messages[0].Content[want-1]
		if last.Type != "image" || last.Source.Type != "base64" || last.Source.MIME != "image/png" || last.Source.Data != "aGVsbG8=" {
			t.Fatalf("invalid image block: %s", body)
		}
	}
}
