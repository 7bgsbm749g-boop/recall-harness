package localfile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func (store *Store) ListSessions(ctx context.Context) ([]sessionstore.SessionInfo, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(store.directory)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return store.listSessionEntries(ctx, entries)
}

func (store *Store) listSessionEntries(ctx context.Context, entries []fs.DirEntry) ([]sessionstore.SessionInfo, error) {
	sessions := make([]sessionstore.SessionInfo, 0)
	for _, entry := range entries {
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		name, ok := strings.CutSuffix(entry.Name(), sessionFileSuffix)
		if !ok || !entry.Type().IsRegular() {
			continue
		}
		id := session.ID(name)
		if err := validateSessionID(id); err != nil {
			continue
		}
		// Windows DirEntry.Info can return metadata cached by ReadDir. Check
		// the path again so removed or replaced files are not listed as live.
		info, err := os.Lstat(filepath.Join(store.directory, entry.Name()))
		if errors.Is(err, errors.ErrUnsupported) {
			return nil, fmt.Errorf("list session %q: %w", id, err)
		}
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		sessions = append(sessions, sessionstore.SessionInfo{
			ID: id, LastUpdatedAt: info.ModTime().UTC(),
		})
	}
	slices.SortFunc(sessions, func(left, right sessionstore.SessionInfo) int {
		return strings.Compare(string(left.ID), string(right.ID))
	})
	return sessions, nil
}
