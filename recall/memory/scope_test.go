package memory

import (
	"context"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestProjectMemoryDoesNotPromoteLocalInstructions(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	quotes := []string{"Reply exactly TEST_OK for this turn.", "Use brief replies in this conversation.", "Use CSV for all future synthetic exports in this project."}
	refs, err := store.Sync(ctx, "first", []llm.Item{message(llm.RoleUser, quotes[0]+" "+quotes[1]+" "+quotes[2])})
	if err != nil {
		t.Fatal(err)
	}
	client := adapterFunc(func(_ context.Context, request llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		facts := []Fact{}
		for i, scope := range []string{"turn", "thread", "project"} {
			facts = append(facts, Fact{Text: quotes[i], Kind: "instruction", Scope: scope, Status: "current", Sources: []string{sourceRefs(request)[0]}})
		}
		return summaryResponse(Document{Summary: "Synthetic export policy with local instructions marked by scope.", Facts: facts}), nil
	})
	engine := newEngine(t, store, "first", client)
	if err = engine.consolidate(ctx, refs); err != nil {
		t.Fatal(err)
	}
	p, _ := store.Snapshot(ctx)
	thread, _ := store.ReadRecord(p.Threads["first"].Prepared.Memory)
	project, _ := store.ReadRecord(p.Overview)
	if len(thread.Document.Facts) != 3 {
		t.Fatal("thread history lost local evidence")
	}
	if len(project.Document.Facts) != 1 || project.Document.Facts[0].Text != quotes[2] {
		t.Fatal("a local instruction became a standing project rule")
	}
}

func TestCorrectionMustSupersedeWithinTheSameScope(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	old, replacement := "Use JSON for project exports.", "Use CSV for project exports."
	refs, err := store.Sync(ctx, "first", []llm.Item{message(llm.RoleUser, old), message(llm.RoleUser, replacement)})
	if err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, store, "first", plainSummarizer())
	inherited := []Fact{{Text: old, Kind: "instruction", Scope: "project", Status: "current", Sources: refs[:1]}}
	for _, scope := range []string{"turn", "project"} {
		doc := Document{Summary: "A scoped correction.", Facts: []Fact{{Text: replacement, Kind: "instruction", Scope: scope, Status: "current", Sources: refs[1:], Supersedes: refs[:1]}}}
		data, _ := encode(doc)
		validated, err := engine.validateDocument(string(data), map[string]bool{refs[0]: true, refs[1]: true}, inherited, "project")
		if err != nil {
			t.Fatal(err)
		}
		if len(validated.Facts) != 1 {
			t.Fatalf("unexpected instruction count for %s", scope)
		}
		want := old
		if scope == "project" {
			want = replacement
		}
		if validated.Facts[0].Text != want {
			t.Fatalf("invalid supersession across %s scope", scope)
		}
	}
}
