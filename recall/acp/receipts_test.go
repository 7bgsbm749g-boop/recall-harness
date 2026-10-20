package acp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func TestDurableReceiptPreventsCommandReplay(t *testing.T) {
	for _, completed := range []bool{false, true} {
		name := "unknown-outcome"
		if completed {
			name = "known-result"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := &Server{ctx: ctx, output: io.Discard}
			session := &Session{server: server, directory: t.TempDir(), meta: Metadata{ID: "fixture", Mode: "full-access"}}
			registry := &registry{tools: map[string]boundTool{}}
			var invocations atomic.Int32
			registry.add("change", "fixture", map[string]any{}, false, func(context.Context, json.RawMessage) (any, error) { invocations.Add(1); return "changed", nil })
			data, _ := jsonv2.Marshal(jobPlan{CallID: "call", Name: "change", Arguments: json.RawMessage(`{}`)})
			spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "recall.tool", Version: 1, Data: data})
			if err != nil {
				t.Fatal(err)
			}
			current := operation.Operation{ID: "durable-test-operation", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady}
			if completed {
				handler := newJobHandler(ctx, session, registry)
				if err := handler.AddRemoteJob(current); err != nil {
					t.Fatal(err)
				}
				result := <-handler.RemoteJobUpdates()
				if result.Status != operation.StatusCompleted {
					t.Fatal("first tool did not complete")
				}
			} else {
				receipts := filepath.Join(session.directory, "receipts")
				if err := os.MkdirAll(receipts, 0700); err != nil {
					t.Fatal(err)
				}
				hash := sha256.Sum256([]byte(current.ID))
				if err := os.WriteFile(filepath.Join(receipts, hex.EncodeToString(hash[:])+".started"), []byte("change"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			recovered := newJobHandler(ctx, session, registry)
			if err := recovered.AddRemoteJob(current); err != nil {
				t.Fatal(err)
			}
			result := <-recovered.RemoteJobUpdates()
			state, err := operation.DecodeRemoteJobState(result)
			if err != nil {
				t.Fatal(err)
			}
			if completed {
				if invocations.Load() != 1 || result.Status != operation.StatusCompleted {
					t.Fatal("known result was executed again")
				}
			} else {
				if invocations.Load() != 0 || result.Status != operation.StatusFailed || !strings.Contains(state.TerminalError, "unknown outcome") {
					t.Fatal("unknown mutation outcome was silently retried")
				}
			}
		})
	}
}
