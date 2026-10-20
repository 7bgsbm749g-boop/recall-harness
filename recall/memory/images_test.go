package memory

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestImageBudgetCountsPixelsWithoutChangingArchivedEvidence(t *testing.T) {
	pixels := image.NewRGBA(image.Rect(0, 0, 256, 256))
	for i := range pixels.Pix {
		pixels.Pix[i] = byte(i*31 + i/113)
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, pixels); err != nil {
		t.Fatal(err)
	}
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(buffer.Bytes())
	user := llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "Inspect this screenshot.", Media: &llm.MessageMedia{Images: []string{url}}}}
	result := llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "view", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultImage, Value: url}}}}
	for _, item := range []llm.Item{user, result} {
		before, err := encode(item)
		if err != nil {
			t.Fatal(err)
		}
		estimate := Estimate([]llm.Item{item})
		if estimate < 2304 || estimate > 3000 {
			t.Fatalf("image budget counted encoded bytes as prose: %d", estimate)
		}
		after, _ := encode(item)
		if !bytes.Equal(before, after) {
			t.Fatal("estimation changed the original image evidence")
		}
	}
	if text := ItemText(user); strings.Contains(text, "base64") || !strings.Contains(text, "256x256") {
		t.Fatal("memory search must contain image metadata without encoded pixels")
	}
}
