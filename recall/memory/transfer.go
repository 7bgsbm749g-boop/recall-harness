package memory

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type transferFrame struct {
	Type      string         `json:"type"`
	Version   int            `json:"version,omitempty"`
	Key       string         `json:"key,omitempty"`
	ThreadID  string         `json:"thread_id,omitempty"`
	Thread    *Thread        `json:"thread,omitempty"`
	Reference string         `json:"reference,omitempty"`
	Data      jsontext.Value `json:"data,omitempty"`
	Objects   int            `json:"objects,omitempty"`
	Threads   int            `json:"threads,omitempty"`
}

// Export streams immutable objects and thread heads. Authentication and tool
// execution state are deliberately outside this memory format.
func (s *Store) Export(ctx context.Context, output io.Writer) error {
	return s.ExportKnown(ctx, output, nil)
}

func (s *Store) Inventory() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.Directory, "objects"))
	if err != nil {
		return nil, err
	}
	refs := []string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		kind, hash, ok := strings.Cut(strings.TrimSuffix(entry.Name(), ".json"), "-")
		if !ok {
			return nil, errors.New("invalid memory object name")
		}
		refs = append(refs, "recall://"+s.ID+"/"+kind+"/"+hash)
	}
	return refs, nil
}

func (s *Store) ExportKnown(ctx context.Context, output io.Writer, known map[string]bool) error {
	p, err := s.Snapshot(ctx)
	if err != nil {
		return err
	}
	encoder := jsontext.NewEncoder(output)
	write := func(frame transferFrame) error { return json.MarshalEncode(encoder, frame, json.Deterministic(true)) }
	if err = write(transferFrame{Type: "project", Version: SchemaVersion, Key: s.Key}); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(s.Directory, "objects"))
	if err != nil {
		return err
	}
	objects := 0
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		kind, hash, ok := strings.Cut(strings.TrimSuffix(entry.Name(), ".json"), "-")
		if !ok {
			return errors.New("invalid memory object filename")
		}
		ref := "recall://" + s.ID + "/" + kind + "/" + hash
		if known[ref] {
			continue
		}
		data, err := s.Read(ref)
		if err != nil {
			return err
		}
		if err = write(transferFrame{Type: "object", Reference: ref, Data: data}); err != nil {
			return err
		}
		objects++
	}
	for id, t := range p.Threads {
		if err = write(transferFrame{Type: "thread", ThreadID: id, Thread: &t}); err != nil {
			return err
		}
	}
	return write(transferFrame{Type: "end", Objects: objects, Threads: len(p.Threads)})
}

func Import(ctx context.Context, root string, input io.Reader) (changed int, err error) {
	decoder := jsontext.NewDecoder(input)
	read := func() (transferFrame, error) {
		var frame transferFrame
		err := json.UnmarshalDecode(decoder, &frame, json.RejectUnknownMembers(true))
		return frame, err
	}
	header, err := read()
	if err != nil {
		return 0, err
	}
	if header.Type != "project" || header.Version != SchemaVersion || header.Key == "" {
		return 0, errors.New("unsupported Recall memory transfer header")
	}
	store, err := Open(root, header.Key)
	if err != nil {
		return 0, err
	}
	threads := map[string]Thread{}
	objects := 0
	ended := false
	for {
		if err = ctx.Err(); err != nil {
			return 0, err
		}
		frame, readErr := read()
		if readErr != nil {
			return 0, fmt.Errorf("incomplete memory transfer: %w", readErr)
		}
		switch frame.Type {
		case "object":
			if _, err = store.objectPath(frame.Reference); err != nil {
				return 0, err
			}
			if digest(frame.Data) != frame.Reference[strings.LastIndex(frame.Reference, "/")+1:] {
				return 0, errors.New("transferred memory object failed integrity check")
			}
			err = store.withProject(ctx, true, func(*Project) error {
				var ref string
				var err error
				if strings.Contains(frame.Reference, "/event/") {
					var event Event
					if err = json.Unmarshal(frame.Data, &event, json.RejectUnknownMembers(true)); err != nil {
						return err
					}
					ref, err = store.put("event", event)
				} else {
					var record Record
					if err = json.Unmarshal(frame.Data, &record, json.RejectUnknownMembers(true)); err != nil {
						return err
					}
					if record.Version != SchemaVersion {
						return errors.New("unsupported transferred memory record")
					}
					ref, err = store.put("memory", record)
				}
				if err != nil {
					return err
				}
				if ref != frame.Reference {
					return errors.New("transferred memory is not canonical for this schema")
				}
				return nil
			})
			if err != nil {
				return 0, err
			}
			objects++
		case "thread":
			if frame.Thread == nil || frame.ThreadID == "" {
				return 0, errors.New("invalid transferred thread")
			}
			if _, duplicate := threads[frame.ThreadID]; duplicate {
				return 0, errors.New("duplicate transferred thread")
			}
			threads[frame.ThreadID] = *frame.Thread
		case "end":
			if frame.Objects != objects || frame.Threads != len(threads) {
				return 0, errors.New("memory transfer counts do not match")
			}
			ended = true
		default:
			return 0, errors.New("unknown memory transfer frame")
		}
		if ended {
			break
		}
	}
	if _, err = read(); !errors.Is(err, io.EOF) {
		return 0, errors.New("unexpected content after memory transfer end")
	}
	for _, t := range threads {
		for _, ref := range t.Inputs {
			if _, err = store.ReadEvent(ref); err != nil {
				return 0, err
			}
		}
		if t.Prepared.Memory != "" {
			if !checkpointMatches(t.Prepared, t.Inputs) {
				return 0, errors.New("transferred summary does not match its transcript prefix")
			}
			if _, err = store.ReadRecord(t.Prepared.Memory); err != nil {
				return 0, err
			}
		}
		for _, ref := range flatten(t.Levels) {
			if _, err = store.ReadRecord(ref); err != nil {
				return 0, err
			}
		}
	}
	err = store.withProject(ctx, true, func(p *Project) error {
		// Validate every head before making any manifest change.
		for id, incoming := range threads {
			local, exists := p.Threads[id]
			if !exists {
				continue
			}
			n := min(len(local.Inputs), len(incoming.Inputs))
			if PrefixDigest(local.Inputs[:n]) != PrefixDigest(incoming.Inputs[:n]) {
				return fmt.Errorf("thread %s has divergent histories; both sets of immutable evidence are retained", id)
			}
		}
		for id, incoming := range threads {
			local, exists := p.Threads[id]
			if exists && (len(local.Inputs) > len(incoming.Inputs) || len(local.Inputs) == len(incoming.Inputs) && local.Prepared.Through >= incoming.Prepared.Through) {
				continue
			}
			incoming.ProjectMemory = ""
			incoming.LastError = ""
			incoming.RetryAfter = time.Time{}
			p.Threads[id] = incoming
			changed++
		}
		return nil
	})
	return changed, err
}
