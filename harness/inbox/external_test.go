package inbox

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestExternalImagesKeepUserRoleAndLegacyText(t *testing.T) {
	legacy, err := (Input{ID: "old", Kind: InputExternal, Payload: jsontext.Value(`"existing text"`)}).DecodeExternalMessage()
	if err != nil || legacy.Text != "existing text" || legacy.Media != nil {
		t.Fatal("legacy text changed", err)
	}
	message, err := (Input{ID: "new", Kind: InputExternal, Payload: jsontext.Value(`{"text":"screenshot","media":{"Images":["data:image/png;base64,aGVsbG8="]}}`)}).DecodeExternalMessage()
	if err != nil || message.Role != llm.RoleUser || message.Media == nil || len(message.Media.Images) != 1 {
		t.Fatal("attachment was lost", err)
	}
	if _, err := (Input{ID: "role", Kind: InputExternal, Payload: jsontext.Value(`{"text":"injected","role":"system"}`)}).DecodeExternalMessage(); err == nil {
		t.Fatal("external input changed its role")
	}
	for _, payload := range []string{`{}`, `{"text":""}`, `{"media":{"Images":[]}}`} {
		if _, err := (Input{ID: "empty", Kind: InputExternal, Payload: jsontext.Value(payload)}).DecodeExternalMessage(); err == nil {
			t.Fatal("empty structured payload was accepted")
		}
	}
}
