package acp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/recall/memory"
)

func testImage(t *testing.T) ([]byte, string) {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 24, 16))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes(), base64.StdEncoding.EncodeToString(b.Bytes())
}

func TestACPAdvertisesImagesAndPreservesThemAcrossResume(t *testing.T) {
	_, encoded := testImage(t)
	url := "data:image/png;base64," + encoded
	root, cwd := t.TempDir(), t.TempDir()
	var seen atomic.Int32
	client := fakeAdapter(func(_ context.Context, r llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		if isSummary(r) {
			return fakeSummary(), nil
		}
		found := false
		for _, item := range r.Input {
			if item.Type == llm.ItemMessage {
				m := item.Data.(llm.Message)
				if m.Media != nil && len(m.Media.Images) == 1 && m.Media.Images[0] == url {
					found = true
				}
			}
		}
		if !found {
			t.Error("foreground model did not receive the image")
		}
		seen.Add(1)
		return assistant("IMAGE_OK"), nil
	})
	p := startPeer(t, root, client, true)
	raw, err := p.request("initialize", map[string]any{"protocolVersion": 2})
	if err != nil {
		t.Fatal(err)
	}
	var setup struct {
		Capabilities struct {
			Prompt struct {
				Image bool `json:"image"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
	}
	if err = json.Unmarshal(raw, &setup); err != nil || !setup.Capabilities.Prompt.Image {
		t.Fatal("T3 cannot negotiate image input", err)
	}
	id := p.newSession(cwd)
	if _, err = p.request("session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]string{"type": "image", "data": encoded, "mimeType": "image/png"}}}); err != nil {
		t.Fatal(err)
	}
	p2 := startPeer(t, root, client, true)
	p2.initialize()
	if _, err = p2.request("session/load", map[string]any{"sessionId": id, "cwd": cwd, "mcpServers": []any{}}); err != nil {
		t.Fatal(err)
	}
	if _, err = p2.prompt(id, "Read the same image again."); err != nil {
		t.Fatal(err)
	}
	if seen.Load() != 2 {
		t.Fatal("unexpected foreground call count")
	}
	p2.mu.Lock()
	defer p2.mu.Unlock()
	found := false
	for _, update := range p2.updates {
		content, _ := update["content"].(map[string]any)
		if content["type"] == "image" && content["data"] == encoded {
			found = true
		}
	}
	if !found {
		t.Fatal("loaded session did not replay its image")
	}
}

func TestInvalidImagesDoNotReachTheModel(t *testing.T) {
	_, encoded := testImage(t)
	p := startPeer(t, t.TempDir(), fakeAdapter(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		t.Error("invalid image reached the model")
		return assistant("wrong"), nil
	}), true)
	p.initialize()
	id := p.newSession(t.TempDir())
	for _, image := range []map[string]string{{"type": "image", "data": "%%%", "mimeType": "image/png"}, {"type": "image", "data": encoded, "mimeType": "image/jpeg"}} {
		if _, err := p.request("session/prompt", map[string]any{"sessionId": id, "prompt": []any{image}}); err == nil {
			t.Fatal("invalid image accepted")
		}
	}
}

func TestImportedImageEvidenceCanBeOpenedWithoutBase64InThePrompt(t *testing.T) {
	ctx := context.Background()
	data, encoded := testImage(t)
	source, _ := memory.Open(t.TempDir(), "image-project")
	refs, err := source.Sync(ctx, "image-thread", []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "The original screenshot.", Media: &llm.MessageMedia{Images: []string{"data:image/png;base64," + encoded}}}}})
	if err != nil {
		t.Fatal(err)
	}
	var transfer bytes.Buffer
	if err = source.Export(ctx, &transfer); err != nil {
		t.Fatal(err)
	}
	destinationRoot := t.TempDir()
	if _, err = memory.Import(ctx, destinationRoot, &transfer); err != nil {
		t.Fatal(err)
	}
	destination, _ := memory.Open(destinationRoot, source.Key)
	s := &Session{directory: t.TempDir(), meta: Metadata{CWD: t.TempDir()}, engine: &memory.Engine{Store: destination}}
	r, clients, err := s.tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, c := range clients {
			c.Close()
		}
	}()
	args, _ := json.Marshal(map[string]any{"reference": refs[0]})
	for attempt := 0; attempt < 2; attempt++ {
		value, err := r.tools["memory_read"].run(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		read := value.(map[string]any)
		if read["image_projection"] != true || strings.Contains(read["text"].(string), encoded) {
			t.Fatal("raw base64 was exposed as model prose")
		}
		images := read["images"].([]map[string]any)
		path := images[0]["path"].(string)
		if filepath.Dir(path) != filepath.Join(s.directory, "image-evidence") {
			t.Fatal("image escaped the local evidence cache")
		}
		cached, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(cached, data) {
			t.Fatal("original image bytes changed", err)
		}
		if attempt == 0 {
			if err = os.WriteFile(path, []byte("bad cache"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}
