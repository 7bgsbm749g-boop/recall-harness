package memory

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// WorkerOnce resumes durable pending work even after an ACP process exits.
// The transcript/coverage difference is the queue; no acknowledged job can be
// lost between a separate queue write and transcript persistence.
func WorkerOnce(ctx context.Context, root string, config Config, client llm.Adapter, model llm.Model) (int, error) {
	entries, err := os.ReadDir(filepath.Join(root, "projects"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count := 0
	var failures []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, "projects", entry.Name(), "project.json"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return count, err
		}
		var manifest Project
		if err = json.Unmarshal(data, &manifest); err != nil {
			return count, err
		}
		store, err := Open(root, manifest.Key)
		if err != nil {
			return count, err
		}
		if store.ID != entry.Name() {
			return count, errors.New("memory project directory does not match its identity")
		}
		for thread, t := range manifest.Threads {
			if err = ctx.Err(); err != nil {
				return count, err
			}
			if len(t.Inputs) == 0 || checkpointMatches(t.Prepared, t.Inputs) && t.Prepared.Through == len(t.Inputs) && t.ProjectMemory == t.Prepared.Memory || time.Now().Before(t.RetryAfter) {
				continue
			}
			engine, err := New(ctx, store, thread, config, client, model)
			if err != nil {
				return count, err
			}
			engine.Schedule(t.Inputs, len(t.Inputs))
			err = engine.Wait(ctx)
			engine.Close()
			if err != nil {
				failures = append(failures, err)
			} else {
				count++
			}
		}
	}
	return count, errors.Join(failures...)
}

func Worker(ctx context.Context, root string, config Config, client llm.Adapter, model llm.Model, interval time.Duration, onResult func(int, error)) error {
	if interval < time.Second {
		return errors.New("memory worker interval must be at least one second")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(root, "worker.lock"))
	ok, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("a Recall memory worker is already active")
	}
	defer lock.Unlock()
	for {
		count, err := WorkerOnce(ctx, root, config, client, model)
		if onResult != nil {
			onResult(count, err)
		}
		if ctx.Err() != nil {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
