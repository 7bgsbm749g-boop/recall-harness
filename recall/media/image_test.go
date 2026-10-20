package media

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"strings"
	"testing"
)

func TestInlineValidation(t *testing.T) {
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, 32, 16))); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(buffer.Bytes())
	url, size, err := Inline(encoded, "image/png")
	if err != nil || size != buffer.Len() || Budget(url) != 2050 {
		t.Fatalf("image validation or pixel budget failed: size=%d error=%v", size, err)
	}
	for _, invalid := range []string{
		"https://example.com/image.png", "file:///tmp/image.png",
		"data:image/png;base64,", "data:image/png;base64,not-base64",
		"data:image/jpeg;base64," + encoded,
		"data:image/png;base64," + strings.Repeat("A", base64.StdEncoding.EncodedLen(MaxImageBytes)+1025),
	} {
		if _, _, _, err := Decode(invalid); err == nil {
			t.Fatal("invalid image accepted")
		}
	}
	if Budget("https://example.com/image.png") != 64000 {
		t.Fatal("unknown image must reserve a bounded fallback without fetching it")
	}
}
