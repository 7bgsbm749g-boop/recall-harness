// Package media validates inline image evidence without fetching remote URLs.
package media

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strings"

	_ "golang.org/x/image/webp"
)

const MaxImageBytes = 10 << 20
const MaxPromptBytes = 20 << 20
const MaxImages = 8

func Decode(url string) ([]byte, string, image.Config, error) {
	var empty image.Config
	header, encoded, ok := strings.Cut(url, ",")
	if !ok || !strings.HasPrefix(header, "data:") || !strings.HasSuffix(header, ";base64") {
		return nil, "", empty, errors.New("image requires inline base64 data")
	}
	mime := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
	if len(encoded) > base64.StdEncoding.EncodedLen(MaxImageBytes)+1024 {
		return nil, "", empty, errors.New("image exceeds the 10 MiB limit")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > MaxImageBytes {
		return nil, "", empty, errors.New("image requires valid nonempty base64 within the 10 MiB limit")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", empty, errors.New("image header could not be decoded")
	}
	expected := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp"}[format]
	if expected == "" || mime != expected {
		return nil, "", empty, errors.New("image MIME type does not match a supported PNG, JPEG, GIF or WebP image")
	}
	if config.Width < 1 || config.Height < 1 || config.Width > 16384 || config.Height > 16384 || int64(config.Width)*int64(config.Height) > 32_000_000 {
		return nil, "", empty, errors.New("image dimensions exceed the 32 megapixel limit")
	}
	return data, mime, config, nil
}

func Inline(encoded, mime string) (string, int, error) {
	url := "data:" + mime + ";base64," + encoded
	data, _, _, err := Decode(url)
	if err != nil {
		return "", 0, err
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), len(data), nil
}

// Budget is a conservative pixel allowance, not a provider tokenizer. Never
// tokenize image base64 as prose or fetch an HTTP URL to estimate its cost.
func Budget(url string) int {
	_, _, config, err := Decode(url)
	if err != nil {
		return 64000
	}
	return 2048 + ((config.Width+15)/16)*((config.Height+15)/16)
}
