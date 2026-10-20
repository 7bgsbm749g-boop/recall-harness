package memory

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestMemoryTransferPreservesSourcesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	source := testStore(t)
	refs, err := source.Sync(ctx, "remote-thread", []llm.Item{message(llm.RoleUser, "Remember the synthetic fleet marker FLEET_RECALL_947.")})
	if err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, source, "remote-thread", plainSummarizer())
	if err = engine.consolidate(ctx, refs); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err = source.Export(ctx, &output); err != nil {
		t.Fatal(err)
	}
	destinationRoot := t.TempDir()
	changed, err := Import(ctx, destinationRoot, bytes.NewReader(output.Bytes()))
	if err != nil || changed != 1 {
		t.Fatalf("memory import failed: %d, %v", changed, err)
	}
	destination, _ := Open(destinationRoot, source.Key)
	hits, err := destination.Search(ctx, "FLEET_RECALL_947", 10)
	if err != nil || len(hits) == 0 || hits[0].Reference != refs[0] {
		t.Fatal("fleet transfer changed source identity or search", err)
	}
	changed, err = Import(ctx, destinationRoot, bytes.NewReader(output.Bytes()))
	if err != nil || changed != 0 {
		t.Fatal("identical transfer was not idempotent", err)
	}
	count, err := WorkerOnce(ctx, destinationRoot, engine.Config, plainSummarizer(), llm.Model{ID: "fixture"})
	if err != nil || count != 1 {
		t.Fatal("imported thread did not enter project memory", err)
	}
	snapshot, _ := destination.Snapshot(ctx)
	if snapshot.Overview == "" || snapshot.Threads["remote-thread"].ProjectMemory == "" {
		t.Fatal("imported project brief is missing")
	}
}

func TestMemoryTransferRejectsTruncationAndDivergentHeads(t *testing.T) {
	ctx := context.Background()
	source := testStore(t)
	if _, err := source.Sync(ctx, "same-thread", []llm.Item{message(llm.RoleUser, "Remote history.")}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := source.Export(ctx, &out); err != nil {
		t.Fatal(err)
	}
	destinationRoot := t.TempDir()
	destination, _ := Open(destinationRoot, source.Key)
	localRefs, err := destination.Sync(ctx, "same-thread", []llm.Item{message(llm.RoleUser, "Different local history.")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Import(ctx, destinationRoot, bytes.NewReader(out.Bytes())); err == nil || !strings.Contains(err.Error(), "divergent") {
		t.Fatal("divergent histories were silently overwritten", err)
	}
	snapshot, _ := destination.Snapshot(ctx)
	if snapshot.Threads["same-thread"].Inputs[0] != localRefs[0] {
		t.Fatal("failed import replaced the current transcript")
	}
	data := out.Bytes()
	if _, err = Import(ctx, t.TempDir(), bytes.NewReader(data[:len(data)/2])); err == nil {
		t.Fatal("truncated transfer was accepted")
	}
}

func TestIncrementalTransferSendsOnlyMissingObjects(t *testing.T) {
	ctx := context.Background()
	source := testStore(t)
	history := []llm.Item{message(llm.RoleUser, "The first fleet event.")}
	if _, err := source.Sync(ctx, "fleet", history); err != nil {
		t.Fatal(err)
	}
	var first bytes.Buffer
	if err := source.Export(ctx, &first); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := Import(ctx, root, &first); err != nil {
		t.Fatal(err)
	}
	destination, _ := Open(root, source.Key)
	inventory, err := destination.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, ref := range inventory {
		known[ref] = true
	}
	history = append(history, message(llm.RoleUser, "The second fleet event."))
	refs, err := source.Sync(ctx, "fleet", history)
	if err != nil {
		t.Fatal(err)
	}
	var delta bytes.Buffer
	if err = source.ExportKnown(ctx, &delta, known); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(delta.Bytes(), []byte(`"reference":"`+refs[0]+`"`)) {
		t.Fatal("incremental transfer resent an existing object")
	}
	if !bytes.Contains(delta.Bytes(), []byte(`"reference":"`+refs[1]+`"`)) {
		t.Fatal("incremental transfer omitted new evidence")
	}
	data := append([]byte(nil), delta.Bytes()...)
	if changed, err := Import(ctx, root, bytes.NewReader(data)); err != nil || changed != 1 {
		t.Fatalf("delta import failed: %d %v", changed, err)
	}
	if _, err := Import(ctx, t.TempDir(), bytes.NewReader(data)); err == nil {
		t.Fatal("a delta was accepted without its required base objects")
	}
	if _, err := destination.Read(refs[1]); err != nil {
		t.Fatal("new evidence is missing", err)
	}
}

func TestWorkerRecoversClosedSessionAndRetriesProjectPublication(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if _, err := store.Sync(ctx, "closed-session", []llm.Item{message(llm.RoleUser, "A durable correction arrived immediately before shutdown.")}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	client := adapterFunc(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		calls.Add(1)
		return summaryResponse(Document{Summary: "A recovered correction. Exact text is in the source."}), nil
	})
	root := filepath.Dir(filepath.Dir(store.Directory))
	count, err := WorkerOnce(ctx, root, DefaultConfig(), client, llm.Model{ID: "fixture"})
	if err != nil || count != 1 {
		t.Fatalf("closed session was not recovered: %d %v", count, err)
	}
	before := calls.Load()
	count, err = WorkerOnce(ctx, root, DefaultConfig(), client, llm.Model{ID: "fixture"})
	if err != nil || count != 0 || calls.Load() != before {
		t.Fatal("idle worker made another model request")
	}
	if err := store.withProject(ctx, true, func(p *Project) error {
		p.Levels = nil
		p.Overview = ""
		thread := p.Threads["closed-session"]
		thread.ProjectMemory = ""
		p.Threads["closed-session"] = thread
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	count, err = WorkerOnce(ctx, root, DefaultConfig(), client, llm.Model{ID: "fixture"})
	if err != nil || count != 1 {
		t.Fatal("unfinished project publication was not resumed", err)
	}
	p, _ := store.Snapshot(ctx)
	if p.Overview == "" {
		t.Fatal("project memory remained empty")
	}
}

func TestQueuedMemoryRefreshKeepsLatestCompletedTurn(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	client := adapterFunc(func(ctx context.Context, _ llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return summaryResponse(Document{Summary: "The latest completed work is retained."}), nil
		case <-ctx.Done():
			return llm.Response{}, ctx.Err()
		}
	})
	engine := newEngine(t, store, "busy-thread", client)
	history := []llm.Item{message(llm.RoleUser, "First request.")}
	refs, err := store.Sync(ctx, "busy-thread", history)
	if err != nil {
		t.Fatal(err)
	}
	engine.Schedule(refs, len(refs))
	<-entered
	history = append(history, message(llm.RoleAssistant, "First response."), message(llm.RoleUser, "Latest correction."))
	refs, err = store.Sync(ctx, "busy-thread", history)
	if err != nil {
		t.Fatal(err)
	}
	engine.Schedule(refs, len(refs))
	close(release)
	if err = engine.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	p, _ := store.Snapshot(ctx)
	if p.Threads["busy-thread"].Prepared.Through != 3 {
		t.Fatal("completed turn was dropped while memory worker was busy")
	}
}

func TestWorkerRecordsFailuresWithoutLosingSources(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	refs, err := store.Sync(ctx, "failed-thread", []llm.Item{message(llm.RoleUser, "A retained instruction.")})
	if err != nil {
		t.Fatal(err)
	}
	client := adapterFunc(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return llm.Response{}, errors.New("synthetic provider failure")
	})
	root := filepath.Dir(filepath.Dir(store.Directory))
	_, err = WorkerOnce(ctx, root, DefaultConfig(), client, llm.Model{ID: "fixture"})
	if err == nil {
		t.Fatal("worker failure was hidden")
	}
	p, _ := store.Snapshot(ctx)
	if p.Threads["failed-thread"].LastError == "" || p.Threads["failed-thread"].RetryAfter.IsZero() {
		t.Fatal("worker did not retain failure and retry state")
	}
	if _, err = store.Read(refs[0]); err != nil {
		t.Fatal("worker failure lost source evidence")
	}
}
