package acp

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"io"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func TestPlanModeCannotExecuteMutatingTools(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &Session{server: &Server{ctx: ctx, output: io.Discard}, directory: t.TempDir(), meta: Metadata{ID: "plan", Mode: "plan"}}
	registry := &registry{tools: map[string]boundTool{}}
	registry.add("change", "fixture", map[string]any{}, false, func(context.Context, json.RawMessage) (any, error) {
		t.Error("plan mode executed a mutation")
		return "changed", nil
	})
	data, _ := jsonv2.Marshal(jobPlan{CallID: "call", Name: "change", Arguments: json.RawMessage(`{}`)})
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "recall.tool", Version: 1, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	current := operation.Operation{ID: "plan-operation", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady}
	handler := newJobHandler(ctx, session, registry)
	if err = handler.AddRemoteJob(current); err != nil {
		t.Fatal(err)
	}
	result := <-handler.RemoteJobUpdates()
	state, err := operation.DecodeRemoteJobState(result)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != operation.StatusFailed || !strings.Contains(state.TerminalError, "Plan mode") {
		t.Fatal("plan mode did not refuse a mutation")
	}
}
