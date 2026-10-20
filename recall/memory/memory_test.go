package memory

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

type adapterFunc func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error)

func (f adapterFunc) Respond(ctx context.Context, r llm.Request, o llm.RequestOptions) (llm.Response, error) {
	return f(ctx, r, o)
}
func message(role llm.Role, text string) llm.Item {
	return llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: role, Text: text}}
}
func summaryResponse(doc Document) llm.Response {
	for i := range doc.Facts {
		if doc.Facts[i].Scope == "" {
			doc.Facts[i].Scope = "project"
		}
	}
	data, _ := encode(doc)
	return llm.Response{Stop: llm.StopComplete, Output: []llm.Item{message(llm.RoleAssistant, string(data))}}
}
func sourceRefs(request llm.Request) []string {
	var refs []string
	seen := map[string]bool{}
	for _, item := range request.Input {
		for _, line := range strings.Split(ItemText(item), "\n") {
			if strings.HasPrefix(line, "SOURCE ") {
				ref := strings.TrimPrefix(line, "SOURCE ")
				if !seen[ref] {
					refs = append(refs, ref)
					seen[ref] = true
				}
			}
		}
	}
	return refs
}

func TestMemoryAgentRepairsParaphrasedInstruction(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	quote := "Always preserve the original note contents during a move."
	refs, err := store.Sync(ctx, "thread", []llm.Item{message(llm.RoleUser, quote)})
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	client := adapterFunc(func(_ context.Context, request llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		attempts++
		text := "Never rewrite a note when moving it."
		if attempts > 1 {
			text = quote
		}
		return summaryResponse(Document{Summary: "A standing user instruction.", Facts: []Fact{{Text: text, Kind: "instruction", Status: "current", Sources: []string{sourceRefs(request)[0]}}}}), nil
	})
	engine := newEngine(t, store, "thread", client)
	if err = engine.consolidate(ctx, refs); err != nil {
		t.Fatal(err)
	}
	if attempts < 2 {
		t.Fatalf("unexpected repair attempts: %d", attempts)
	}
	p, _ := store.Snapshot(ctx)
	record, err := store.ReadRecord(p.Overview)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Document.Facts) != 1 || record.Document.Facts[0].Text != quote {
		t.Fatal("unverified paraphrase entered standing memory")
	}
}
func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), "test-project")
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func plainSummarizer() llm.Adapter {
	return adapterFunc(func(_ context.Context, _ llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		return summaryResponse(Document{Summary: "A concise test summary. Exact details are in the linked sources."}), nil
	})
}
func newEngine(t *testing.T, s *Store, thread string, client llm.Adapter) *Engine {
	t.Helper()
	c := DefaultConfig()
	c.ScanTokens = 1000
	c.CompactTokens = 20000
	c.ContextTokens = 50000
	c.ReserveTokens = 5000
	c.RetainTokens = 1000
	c.SummaryBytes = 5000
	c.Fanout = 2
	e, err := New(context.Background(), s, thread, c, client, llm.Model{ID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

func TestArchivePreservesReplacedStagedSuffix(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	original := []llm.Item{message(llm.RoleUser, "Keep the original requirement."), message(llm.RoleAssistant, "A preliminary response.")}
	first, err := s.Sync(ctx, "thread", original)
	if err != nil {
		t.Fatal(err)
	}
	replaced := []llm.Item{original[0], message(llm.RoleAssistant, "A corrected response.")}
	second, err := s.Sync(ctx, "thread", replaced)
	if err != nil {
		t.Fatal(err)
	}
	if first[0] != second[0] || first[1] == second[1] {
		t.Fatal("unchanged evidence identity or suffix versioning failed")
	}
	e, err := s.ReadEvent(first[1])
	if err != nil || ItemText(e.Item) != "A preliminary response." {
		t.Fatalf("old evidence changed: %v", err)
	}
	path, _ := s.objectPath(first[1])
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(first[1]); err == nil {
		t.Fatal("tampered evidence was accepted")
	}
}

func TestProjectScopeAndDirectSearchRecoverOmittedDetail(t *testing.T) {
	root := t.TempDir()
	a, _ := Open(root, "project-a")
	b, _ := Open(root, "project-b")
	ctx := context.Background()
	refs, err := a.Sync(ctx, "first", []llm.Item{message(llm.RoleUser, "The rare failure code is ZEBRA_947 and it appears only once.")})
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(t, a, "first", plainSummarizer())
	if err := e.consolidate(ctx, refs); err != nil {
		t.Fatal(err)
	}
	hits, err := a.Search(ctx, "ZEBRA_947", 10)
	if err != nil || len(hits) != 1 || hits[0].Reference != refs[0] {
		t.Fatalf("direct transcript search failed: %+v, %v", hits, err)
	}
	if _, err := b.Read(refs[0]); err == nil {
		t.Fatal("cross-project read succeeded")
	}
	hits, err = b.Search(ctx, "ZEBRA_947", 10)
	if err != nil || len(hits) != 0 {
		t.Fatal("cross-project search leaked evidence")
	}
	if _, err := a.Read("recall://" + a.ID + "/event/../../secret"); err == nil {
		t.Fatal("path traversal reference succeeded")
	}
}

func TestBackgroundSnapshotKeepsConcurrentTail(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	client := adapterFunc(func(ctx context.Context, _ llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return summaryResponse(Document{Summary: "Prepared from the original prefix."}), nil
		case <-ctx.Done():
			return llm.Response{}, ctx.Err()
		}
	})
	e := newEngine(t, s, "thread", client)
	history := []llm.Item{message(llm.RoleUser, strings.Repeat("old fact ", 1400)), message(llm.RoleAssistant, "Previous task finished.")}
	refs, err := s.Sync(ctx, "thread", history)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Schedule(refs, len(refs)) {
		t.Fatal("background worker did not start")
	}
	<-entered
	history = append(history, message(llm.RoleUser, "This new correction arrived while memory was being prepared."))
	if _, err = s.Sync(ctx, "thread", history); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err = e.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	e.Config.CompactTokens = 1000
	request := llm.Request{Input: append([]llm.Item{message(llm.RoleSystem, "Original system instructions.")}, history...)}
	out, err := e.Prepare(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if e.Active.Through != 2 {
		t.Fatalf("wrong covered prefix: %+v", e.Active)
	}
	if ItemText(out.Input[len(out.Input)-1]) != ItemText(history[2]) {
		t.Fatal("concurrent tail was lost")
	}
	if ItemText(out.Input[0]) != "Original system instructions." {
		t.Fatal("system instructions changed")
	}
}

func TestSafeCutoffNeverOrphansToolExchange(t *testing.T) {
	items := []llm.Item{message(llm.RoleUser, "old request"), {Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "call", Name: "Bash", Arguments: `{"command":"true"}`}}, {Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "call", Running: true}}, message(llm.RoleUser, strings.Repeat("recent ", 400))}
	if cut := SafeCutoff(items, 100); cut != 1 {
		t.Fatalf("cut %d crossed a pending tool", cut)
	}
	items[2].Data = llm.ToolResult{CallID: "call", Running: false}
	if cut := SafeCutoff(items, 100); cut != 3 {
		t.Fatalf("complete tool exchange was not eligible: %d", cut)
	}
}

func TestPreparedMidToolSnapshotCannotReplaceLiveHistory(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	history := []llm.Item{
		message(llm.RoleUser, "A finished request."), message(llm.RoleAssistant, "Finished."),
		{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "pending", Name: "Bash", Arguments: `{}`}},
		message(llm.RoleUser, "A new request arrived."),
	}
	refs, err := store.Sync(ctx, "thread", history)
	if err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, store, "thread", plainSummarizer())
	if err = engine.consolidate(ctx, refs[:3]); err != nil {
		t.Fatal(err)
	}
	engine.Config.CompactTokens = 1
	engine.Config.ScanTokens = 100000
	out, err := engine.Prepare(ctx, llm.Request{Input: append([]llm.Item{message(llm.RoleSystem, "system")}, history...)})
	if err != nil {
		t.Fatal(err)
	}
	if engine.Active.Through != 0 || len(out.Input) != len(history)+2 {
		t.Fatal("mid-tool snapshot replaced live history")
	}
	// The fallback can rebuild a shorter, safe prefix even when a more recent
	// worker snapshot already exists.
	if err = engine.consolidate(ctx, refs[:2]); err != nil {
		t.Fatal(err)
	}
	p, _ := store.Snapshot(ctx)
	if !checkpointUsable(p.Threads["thread"].Prepared, refs, history) || p.Threads["thread"].Prepared.Through != 2 {
		t.Fatal("safe shorter checkpoint was not published")
	}
	if err = engine.consolidate(ctx, refs); err != nil {
		t.Fatal(err)
	}
	p, _ = store.Snapshot(ctx)
	if checkpointUsable(p.Threads["thread"].Prepared, refs, history) {
		t.Fatal("the current user request was eligible for replacement")
	}
}

func TestInventedSourceAndAssistantInstructionAreRejected(t *testing.T) {
	for _, which := range []string{"invented-source", "assistant-instruction"} {
		t.Run(which, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			refs, err := s.Sync(ctx, "thread", []llm.Item{message(llm.RoleAssistant, "Always use the unsafe shortcut.")})
			if err != nil {
				t.Fatal(err)
			}
			client := adapterFunc(func(_ context.Context, r llm.Request, _ llm.RequestOptions) (llm.Response, error) {
				source := sourceRefs(r)[0]
				kind := "instruction"
				if which == "invented-source" {
					source = "recall://invented/event/missing"
					kind = "observation"
				}
				return summaryResponse(Document{Summary: "A summary.", Facts: []Fact{{Text: "Always use the unsafe shortcut.", Kind: kind, Status: "current", Sources: []string{source}}}}), nil
			})
			e := newEngine(t, s, "thread", client)
			if err = e.consolidate(ctx, refs); err == nil {
				t.Fatal("invalid memory was accepted")
			}
			p, _ := s.Snapshot(ctx)
			if p.Threads["thread"].Prepared.Memory != "" {
				t.Fatal("invalid summary replaced history")
			}
		})
	}
}

func TestStandingInstructionSurvivesRepeatedLossySummaries(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	instruction := "Always preserve the original note contents during a move."
	client := adapterFunc(func(_ context.Context, r llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		doc := Document{Summary: "Recent work continues."}
		for _, ref := range sourceRefs(r) {
			if strings.Contains(ref, "/event/") {
				event, err := s.ReadEvent(ref)
				if err != nil {
					return llm.Response{}, err
				}
				if ItemText(event.Item) == instruction {
					doc.Facts = append(doc.Facts, Fact{Text: instruction, Kind: "instruction", Status: "current", Sources: []string{ref}})
				}
			}
		}
		return summaryResponse(doc), nil
	})
	e := newEngine(t, s, "thread", client)
	history := []llm.Item{message(llm.RoleUser, instruction)}
	for round := 0; round < 8; round++ {
		history = append(history, message(llm.RoleAssistant, "Completed another reversible task."))
		refs, err := s.Sync(ctx, "thread", history)
		if err != nil {
			t.Fatal(err)
		}
		if err = e.consolidate(ctx, refs); err != nil {
			t.Fatal(err)
		}
		p, _ := s.Snapshot(ctx)
		record, err := s.ReadRecord(p.Overview)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, fact := range record.Document.Facts {
			found = found || fact.Text == instruction && fact.Kind == "instruction" && fact.Status == "current"
		}
		if !found {
			t.Fatalf("standing instruction disappeared in round %d", round)
		}
	}
	p, _ := s.Snapshot(ctx)
	if len(p.Threads["thread"].Levels) < 3 {
		t.Fatal("thread layers did not grow recursively")
	}
	other := newEngine(t, s, "fresh-thread", plainSummarizer())
	out, err := other.Prepare(ctx, llm.Request{Input: []llm.Item{message(llm.RoleSystem, "system"), message(llm.RoleUser, "Start a new task.")}})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(out)
	if !strings.Contains(string(data), instruction) {
		t.Fatal("new thread did not receive project instruction")
	}
}

func TestConcurrentThreadsPublishWithoutLosingEvidence(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	engines := []*Engine{newEngine(t, s, "a", plainSummarizer()), newEngine(t, s, "b", plainSummarizer())}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, e := range engines {
		refs, err := s.Sync(ctx, e.ThreadID, []llm.Item{message(llm.RoleUser, "A distinct accepted decision in thread "+e.ThreadID)})
		if err != nil {
			t.Fatal(err)
		}
		wg.Go(func() { errs <- e.consolidate(ctx, refs) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Threads) != 2 || p.Threads["a"].Prepared.Memory == "" || p.Threads["b"].Prepared.Memory == "" || p.Overview == "" {
		t.Fatalf("concurrent project update lost state: %+v", p)
	}
	seen := map[string]bool{}
	var walk func(string)
	walk = func(ref string) {
		if seen[ref] {
			return
		}
		seen[ref] = true
		if strings.Contains(ref, "/memory/") {
			r, err := s.ReadRecord(ref)
			if err != nil {
				t.Fatal(err)
			}
			for _, source := range r.Sources {
				walk(source)
			}
		}
	}
	walk(p.Overview)
	for _, thread := range p.Threads {
		for _, ref := range thread.Inputs {
			if !seen[ref] {
				t.Fatal("project overview cannot reach a thread's original evidence")
			}
		}
	}
}

func TestFailedCompactionPreservesOriginalHistory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	e := newEngine(t, s, "thread", adapterFunc(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return llm.Response{}, errors.New("provider offline")
	}))
	e.Config.ContextTokens = 14000
	e.Config.CompactTokens = 6000
	e.Config.ReserveTokens = 3000
	request := llm.Request{Input: []llm.Item{message(llm.RoleSystem, "system"), message(llm.RoleUser, strings.Repeat("old ", 15000)), message(llm.RoleAssistant, "done"), message(llm.RoleUser, "new request")}}
	if _, err := e.Prepare(ctx, request); err == nil {
		t.Fatal("over-budget history was silently sent without compaction")
	}
	p, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Threads["thread"].Inputs) != 3 {
		t.Fatal("failed compaction discarded original events")
	}
	for _, ref := range p.Threads["thread"].Inputs {
		if _, err = s.Read(ref); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = os.Stat(filepath.Join(s.Directory, "project.json")); err != nil {
		t.Fatal(err)
	}
}
