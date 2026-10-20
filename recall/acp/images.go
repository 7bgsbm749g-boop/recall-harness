package acp

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/recall/media"
)

// The original bytes stay in the immutable event. Local files are only a
// disposable cache for ViewImage, including after a fleet memory import.
func (s *Session) imageEvidence(images *llm.MessageMedia) ([]map[string]any, error) {
	var result []map[string]any
	if images == nil {
		return result, nil
	}
	directory := filepath.Join(s.directory, "image-evidence")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	for index, url := range images.Images {
		data, mime, config, err := media.Decode(url)
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256(data)
		extension := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}[mime]
		path := filepath.Join(directory, hex.EncodeToString(hash[:])+extension)
		if existing, err := os.ReadFile(path); err != nil || sha256.Sum256(existing) != hash {
			if err := os.WriteFile(path, data, 0600); err != nil {
				return nil, err
			}
		}
		result = append(result, map[string]any{"index": index, "mime_type": mime, "width": config.Width, "height": config.Height, "path": path, "sha256": hex.EncodeToString(hash[:])})
	}
	return result, nil
}
