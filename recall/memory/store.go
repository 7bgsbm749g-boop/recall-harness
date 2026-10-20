// Package memory provides immutable evidence and versioned project memory.
package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/gofrs/flock"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

const SchemaVersion = 1

type Event struct {
	Thread  string    `json:"thread"`
	Ordinal int       `json:"ordinal"`
	Time    time.Time `json:"time"`
	Item    llm.Item  `json:"item"`
}

type Fact struct {
	Text       string   `json:"text"`
	Kind       string   `json:"kind"`            // instruction, decision, observation, proposal, task
	Scope      string   `json:"scope,omitempty"` // turn, thread, project
	Status     string   `json:"status"`          // current, uncertain, superseded, resolved
	Sources    []string `json:"sources"`
	Supersedes []string `json:"supersedes,omitempty"`
}

type Document struct {
	Summary        string   `json:"summary"`
	Facts          []Fact   `json:"facts"`
	RetrievalHints []string `json:"retrieval_hints"`
}

type Record struct {
	Version  int       `json:"version"`
	Scope    string    `json:"scope"`
	Thread   string    `json:"thread,omitempty"`
	Level    int       `json:"level"`
	Time     time.Time `json:"time"`
	Document Document  `json:"document"`
	Sources  []string  `json:"sources"`
}

type Checkpoint struct {
	Through int    `json:"through"`
	Digest  string `json:"digest"`
	Memory  string `json:"memory"`
}

type Thread struct {
	Inputs        []string   `json:"inputs"`
	Prepared      Checkpoint `json:"prepared"`
	ProjectMemory string     `json:"project_memory,omitempty"`
	Levels        [][]string `json:"levels"`
	LastError     string     `json:"last_error,omitempty"`
	RetryAfter    time.Time  `json:"retry_after,omitempty"`
}

type Project struct {
	Version  int               `json:"version"`
	Key      string            `json:"key"`
	Revision uint64            `json:"revision"`
	Threads  map[string]Thread `json:"threads"`
	Overview string            `json:"overview"`
	Levels   [][]string        `json:"levels"`
}

type Store struct{ Directory, ID, Key string }

func Open(root, key string) (*Store, error) {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(key) == "" {
		return nil, errors.New("memory root and project key are required")
	}
	hash := sha256.Sum256([]byte(key))
	id := hex.EncodeToString(hash[:16])
	s := &Store{Directory: filepath.Join(root, "projects", id), ID: id, Key: key}
	for _, dir := range []string{s.Directory, filepath.Join(s.Directory, "objects"), filepath.Join(s.Directory, "index")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func digest(data []byte) string         { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func encode(v any) ([]byte, error)      { return json.Marshal(v, json.Deterministic(true)) }
func PrefixDigest(refs []string) string { return digest([]byte(strings.Join(refs, "\n"))) }

func atomicWrite(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".recall-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}

func (s *Store) withProject(ctx context.Context, write bool, fn func(*Project) error) error {
	lock := flock.New(filepath.Join(s.Directory, "project.lock"))
	ok, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("memory project is locked")
	}
	defer lock.Unlock()
	p := Project{Version: SchemaVersion, Key: s.Key, Threads: map[string]Thread{}}
	data, err := os.ReadFile(filepath.Join(s.Directory, "project.json"))
	if err == nil {
		if err = json.Unmarshal(data, &p); err != nil {
			return fmt.Errorf("invalid memory manifest: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if p.Version != SchemaVersion || p.Key != s.Key {
		return errors.New("unsupported memory schema or project identity")
	}
	if p.Threads == nil {
		p.Threads = map[string]Thread{}
	}
	if err = fn(&p); err != nil {
		return err
	}
	if !write {
		return nil
	}
	p.Revision++
	data, err = encode(p)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.Directory, "project.json"), data)
}

func (s *Store) Snapshot(ctx context.Context) (p Project, err error) {
	err = s.withProject(ctx, false, func(current *Project) error { p = *current; return nil })
	return
}

func (s *Store) objectPath(ref string) (string, error) {
	prefix := "recall://" + s.ID + "/"
	if !strings.HasPrefix(ref, prefix) {
		return "", errors.New("reference belongs to a different project")
	}
	parts := strings.Split(strings.TrimPrefix(ref, prefix), "/")
	if len(parts) != 2 || (parts[0] != "event" && parts[0] != "memory") || len(parts[1]) != 64 {
		return "", errors.New("invalid memory reference")
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", errors.New("invalid memory object hash")
	}
	return filepath.Join(s.Directory, "objects", parts[0]+"-"+parts[1]+".json"), nil
}

func (s *Store) Read(ref string) ([]byte, error) {
	path, err := s.objectPath(ref)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if digest(data) != ref[strings.LastIndex(ref, "/")+1:] {
		return nil, errors.New("memory object failed integrity check")
	}
	return data, nil
}

func (s *Store) ReadRecord(ref string) (r Record, err error) {
	if !strings.Contains(ref, "/memory/") {
		return r, errors.New("reference is not a memory record")
	}
	data, err := s.Read(ref)
	if err != nil {
		return r, err
	}
	err = json.Unmarshal(data, &r)
	return
}

func (s *Store) ReadEvent(ref string) (e Event, err error) {
	if !strings.Contains(ref, "/event/") {
		return e, errors.New("reference is not a transcript event")
	}
	data, err := s.Read(ref)
	if err != nil {
		return e, err
	}
	err = json.Unmarshal(data, &e)
	return
}

func (s *Store) put(kind string, v any) (string, error) {
	data, err := encode(v)
	if err != nil {
		return "", err
	}
	ref := "recall://" + s.ID + "/" + kind + "/" + digest(data)
	path, _ := s.objectPath(ref)
	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) != string(data) {
			return "", errors.New("immutable memory collision")
		}
		return ref, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err = atomicWrite(path, data); err != nil {
		return "", err
	}
	text := string(data)
	if event, ok := v.(Event); ok {
		text = ItemText(event.Item)
	}
	for _, term := range terms(text) {
		f, err := os.OpenFile(filepath.Join(s.Directory, "index", digest([]byte(term))+".idx"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return "", err
		}
		_, writeErr := fmt.Fprintln(f, ref)
		closeErr := f.Close()
		if writeErr != nil {
			return "", writeErr
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	return ref, nil
}

func ItemText(item llm.Item) string {
	switch item.Type {
	case llm.ItemMessage:
		return item.Data.(llm.Message).Text
	case llm.ItemToolCall:
		c := item.Data.(llm.ToolCall)
		return c.Name + " " + c.Arguments
	case llm.ItemToolResult:
		var out []string
		for _, v := range item.Data.(llm.ToolResult).Output {
			if v.Kind == llm.ToolResultText {
				out = append(out, v.Value)
			}
		}
		return strings.Join(out, "\n")
	case llm.ItemProvider:
		if d := item.Data.(llm.ProviderItem).Display; d != nil {
			return d.Text
		}
	}
	return ""
}

func terms(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' })
	unique := map[string]bool{}
	for _, word := range fields {
		if len(word) > 1 && len(word) <= 100 {
			unique[word] = true
		}
	}
	result := make([]string, 0, len(unique))
	for word := range unique {
		result = append(result, word)
	}
	sort.Strings(result)
	return result
}

// Sync archives model-visible history before any lossy context operation. A changed
// staged suffix gets fresh IDs; old evidence is never overwritten or deleted.
func (s *Store) Sync(ctx context.Context, thread string, items []llm.Item) (refs []string, err error) {
	err = s.withProject(ctx, true, func(p *Project) error {
		t := p.Threads[thread]
		same := 0
		for same < len(t.Inputs) && same < len(items) {
			e, eErr := s.ReadEvent(t.Inputs[same])
			if eErr != nil {
				return eErr
			}
			a, _ := encode(e.Item)
			b, _ := encode(items[same])
			if string(a) != string(b) {
				break
			}
			same++
		}
		refs = append([]string(nil), t.Inputs[:same]...)
		for i := same; i < len(items); i++ {
			ref, e := s.put("event", Event{Thread: thread, Ordinal: i + 1, Time: time.Now().UTC(), Item: items[i]})
			if e != nil {
				return e
			}
			refs = append(refs, ref)
		}
		t.Inputs = refs
		p.Threads[thread] = t
		return nil
	})
	return
}

type Hit struct {
	Reference string `json:"reference"`
	Score     int    `json:"score"`
	Text      string `json:"text"`
}

func (s *Store) Search(ctx context.Context, query string, limit int) ([]Hit, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	words := terms(query)
	if len(words) == 0 {
		return nil, nil
	}
	scores := map[string]int{}
	for _, word := range words {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(filepath.Join(s.Directory, "index", digest([]byte(word))+".idx"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, ref := range strings.Fields(string(data)) {
			if !seen[ref] {
				scores[ref]++
				seen[ref] = true
			}
		}
	}
	hits := make([]Hit, 0, len(scores))
	for ref, score := range scores {
		hits = append(hits, Hit{Reference: ref, Score: score})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Reference < hits[j].Reference
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	for i := range hits {
		data, err := s.Read(hits[i].Reference)
		if err != nil {
			return nil, err
		}
		if strings.Contains(hits[i].Reference, "/event/") {
			e, err := s.ReadEvent(hits[i].Reference)
			if err != nil {
				return nil, err
			}
			hits[i].Text = ItemText(e.Item)
		} else {
			hits[i].Text = string(data)
		}
		if len(hits[i].Text) > 2000 {
			hits[i].Text = hits[i].Text[:2000] + "\n[preview truncated; use memory_read for the source]"
		}
	}
	return hits, nil
}
