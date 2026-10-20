package memory

import (
	"context"
	"fmt"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

type Adapter struct {
	Engine    *Engine
	Client    llm.Adapter
	mu        sync.Mutex
	respondMu sync.Mutex
	last      []llm.Item
}

func (a *Adapter) Respond(ctx context.Context, request llm.Request, options llm.RequestOptions) (llm.Response, error) {
	a.respondMu.Lock()
	defer a.respondMu.Unlock()
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	prepared, err := a.Engine.Prepare(ctx, request)
	if err != nil {
		return llm.Response{}, err
	}
	response, err := a.Client.Respond(ctx, prepared, options)
	if err != nil {
		return response, err
	}
	if response.Usage.InputTokens+response.Usage.OutputTokens > int64(a.Engine.Config.ContextTokens) {
		return response, fmt.Errorf("provider reported context usage above Recall's configured limit")
	}
	// The original request and complete provider response remain canonical.
	all := append(append([]llm.Item(nil), request.Input[1:]...), response.Output...)
	if _, err = a.Engine.Store.Sync(ctx, a.Engine.ThreadID, all); err != nil {
		return response, err
	}
	a.mu.Lock()
	a.last = all
	a.mu.Unlock()
	return response, nil
}

func (a *Adapter) Stats() map[string]any {
	a.respondMu.Lock()
	defer a.respondMu.Unlock()
	return map[string]any{"compactions": a.Engine.Compactions, "covered_events": a.Engine.Active.Through, "memory_error": a.Engine.LastError() != nil}
}

func (a *Adapter) Refresh(ctx context.Context) error {
	a.mu.Lock()
	items := append([]llm.Item(nil), a.last...)
	a.mu.Unlock()
	if len(items) == 0 {
		return nil
	}
	return a.Engine.Refresh(ctx, items)
}
