package responsesapi

import (
	"encoding/json/v2"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestUserImageIsAnInputImageOnTheWire(t *testing.T) {
	url := "data:image/png;base64,aGVsbG8="
	for _, id := range []string{"", "stored-message"} {
		for _, text := range []string{"Inspect the screenshot.", ""} {
			item, err := requestInputItem(llm.Item{Type: llm.ItemMessage, ProviderID: id, Data: llm.Message{Role: llm.RoleUser, Text: text, Media: &llm.MessageMedia{Images: []string{url}}}})
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(item)
			var wire struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
					URL  string `json:"image_url"`
				} `json:"content"`
			}
			if err = json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			want := 1
			if text != "" {
				want = 2
			}
			if wire.Role != "user" || len(wire.Content) != want || wire.Content[want-1].Type != "input_image" || wire.Content[want-1].URL != url {
				t.Fatalf("image was not transmitted: %s", data)
			}
		}
	}
}
