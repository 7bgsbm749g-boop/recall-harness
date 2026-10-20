package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/recall/memory"
)

type fakeAdapter func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error)

func (f fakeAdapter) Respond(ctx context.Context, r llm.Request, o llm.RequestOptions) (llm.Response, error) {
	return f(ctx, r, o)
}
func assistant(text string) llm.Response {
	return llm.Response{Stop: llm.StopComplete, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text}}}}
}
func isSummary(r llm.Request) bool {
	return len(r.Input) > 0 && strings.Contains(memory.ItemText(r.Input[0]), "dedicated memory agent")
}
func fakeSummary() llm.Response {
	return assistant(`{"summary":"The previous task completed. Exact evidence remains in the linked transcript.","facts":[],"retrieval_hints":[]}`)
}

type testPeer struct {
	t       *testing.T
	input   *io.PipeWriter
	done    chan error
	mu      sync.Mutex
	writes  sync.Mutex
	pending map[string]chan envelope
	updates []map[string]any
	next    atomic.Uint64
	allow   bool
}

func startPeer(t *testing.T, dir string, client llm.Adapter, allow bool) *testPeer {
	t.Helper()
	inputR, inputW := io.Pipe()
	outputR, outputW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	p := &testPeer{t: t, input: inputW, done: make(chan error, 1), pending: map[string]chan envelope{}, allow: allow}
	go func() {
		err := Serve(ctx, inputR, outputW, Config{DataDirectory: dir, Model: "fixture", Models: []string{"fixture", "other"}, Effort: llm.ReasoningEffortLow, Memory: memory.DefaultConfig(), Client: client})
		_ = outputW.Close()
		p.done <- err
	}()
	go func() {
		decoder := json.NewDecoder(outputR)
		for {
			var message envelope
			if decoder.Decode(&message) != nil {
				return
			}
			if message.Method == "session/request_permission" {
				option := "reject_once"
				if p.allow {
					option = "allow_once"
				}
				p.write(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": option}}})
				continue
			}
			p.mu.Lock()
			if message.Method == "session/update" {
				var params struct {
					Update map[string]any `json:"update"`
				}
				_ = json.Unmarshal(message.Params, &params)
				p.updates = append(p.updates, params.Update)
			} else if ch := p.pending[string(message.ID)]; ch != nil {
				ch <- message
			}
			p.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = inputW.Close()
		select {
		case err := <-p.done:
			if err != nil {
				t.Errorf("ACP server shutdown: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("ACP server did not stop")
		}
		_ = outputR.Close()
	})
	return p
}
func (p *testPeer) write(value any) {
	p.writes.Lock()
	defer p.writes.Unlock()
	data, _ := json.Marshal(value)
	_, _ = p.input.Write(append(data, '\n'))
}
func (p *testPeer) request(method string, params any) (json.RawMessage, error) {
	id := p.next.Add(1)
	raw, _ := json.Marshal(id)
	ch := make(chan envelope, 1)
	p.mu.Lock()
	p.pending[string(raw)] = ch
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, string(raw)); p.mu.Unlock() }()
	p.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	select {
	case response := <-ch:
		if response.Error != nil {
			return nil, errors.New(response.Error.Message)
		}
		return response.Result, nil
	case <-time.After(10 * time.Second):
		return nil, errors.New("ACP test request timed out")
	}
}
func (p *testPeer) initialize() {
	p.t.Helper()
	raw, err := p.request("initialize", map[string]any{"protocolVersion": 2, "clientCapabilities": map[string]any{}, "clientInfo": map[string]string{"name": "test", "version": "1"}})
	if err != nil {
		p.t.Fatal(err)
	}
	var result struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if json.Unmarshal(raw, &result) != nil || result.ProtocolVersion != 1 {
		p.t.Fatal("ACP v1 negotiation failed")
	}
}
func (p *testPeer) newSession(cwd string) string {
	p.t.Helper()
	raw, err := p.request("session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	if err != nil {
		p.t.Fatal(err)
	}
	var result struct {
		SessionID string `json:"sessionId"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		p.t.Fatal(err)
	}
	return result.SessionID
}
func (p *testPeer) prompt(id, text string) (json.RawMessage, error) {
	return p.request("session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]string{"type": "text", "text": text}}})
}

func TestACPMultiTurnAndPersistentResume(t *testing.T) {
	dir := t.TempDir()
	cwd := t.TempDir()
	var calls atomic.Int32
	client := fakeAdapter(func(_ context.Context, r llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		if isSummary(r) {
			return fakeSummary(), nil
		}
		calls.Add(1)
		all := ""
		for _, item := range r.Input {
			all += memory.ItemText(item)
		}
		if strings.Contains(all, "second request") && !strings.Contains(all, "first request") {
			t.Error("prior user message was lost on resume")
		}
		return assistant("test response"), nil
	})
	first := startPeer(t, dir, client, true)
	first.initialize()
	id := first.newSession(cwd)
	if _, err := first.prompt(id, "first request"); err != nil {
		t.Fatal(err)
	}
	second := startPeer(t, dir, client, true)
	second.initialize()
	if _, err := second.request("session/load", map[string]any{"sessionId": id, "cwd": cwd, "mcpServers": []any{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := second.prompt(id, "second request"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected foreground call count")
	}
	raw, err := second.request("session/list", map[string]any{"cwd": cwd})
	if err != nil || !strings.Contains(string(raw), id) {
		t.Fatal("saved session was not listed")
	}
	if _, err := second.request("session/load", map[string]any{"sessionId": id, "cwd": t.TempDir(), "mcpServers": []any{}}); err == nil {
		t.Fatal("session resumed in an unrelated workspace")
	}
}

func TestACPToolApprovalControlsActualExecution(t *testing.T) {
	for _, allow := range []bool{false, true} {
		name := "deny"
		if allow {
			name = "allow"
		}
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			client := fakeAdapter(func(_ context.Context, r llm.Request, _ llm.RequestOptions) (llm.Response, error) {
				if isSummary(r) {
					return fakeSummary(), nil
				}
				for _, item := range r.Input {
					if item.Type == llm.ItemToolResult {
						return assistant("tool result received"), nil
					}
				}
				return llm.Response{Stop: llm.StopComplete, Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "test-call", Name: "Bash", Arguments: `{"command":"echo approved > approval-result.txt"}`}}}}, nil
			})
			p := startPeer(t, t.TempDir(), client, allow)
			p.initialize()
			id := p.newSession(cwd)
			if _, err := p.prompt(id, "run the fixture command"); err != nil {
				t.Fatal(err)
			}
			_, err := os.Stat(filepath.Join(cwd, "approval-result.txt"))
			if allow && err != nil {
				t.Fatal("approved command did not run")
			}
			if !allow && !errors.Is(err, os.ErrNotExist) {
				t.Fatal("denied command changed the workspace")
			}
		})
	}
}

func TestACPCancelInterruptsPendingModelAndKeepsServerUsable(t *testing.T) {
	entered := make(chan struct{})
	var calls atomic.Int32
	client := fakeAdapter(func(ctx context.Context, r llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		if isSummary(r) {
			return fakeSummary(), nil
		}
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			return llm.Response{}, ctx.Err()
		}
		return assistant("resumed"), nil
	})
	p := startPeer(t, t.TempDir(), client, true)
	p.initialize()
	id := p.newSession(t.TempDir())
	done := make(chan error, 1)
	go func() {
		raw, err := p.prompt(id, "a cancelled request")
		if err == nil && !strings.Contains(string(raw), "cancelled") {
			err = errors.New("wrong cancellation result")
		}
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("request failed before reaching the model: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("request did not reach the model")
	}
	p.write(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": id}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := p.prompt(id, "continue after cancellation"); err != nil {
		t.Fatal(err)
	}
}

func TestACPRejectsUnadvertisedAudio(t *testing.T) {
	p := startPeer(t, t.TempDir(), fakeAdapter(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		t.Error("unsupported input reached model")
		return assistant("wrong"), nil
	}), true)
	p.initialize()
	id := p.newSession(t.TempDir())
	if _, err := p.request("session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]string{"type": "audio", "data": "base64", "mimeType": "audio/wav"}}}); err == nil {
		t.Fatal("unsupported audio input was silently accepted")
	}
}

func TestServeCancellationClosesIdleInput(t *testing.T) {
	inputR, inputW := io.Pipe()
	defer inputW.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, inputR, io.Discard, Config{DataDirectory: t.TempDir(), Model: "fixture", Memory: memory.DefaultConfig(), Client: fakeAdapter(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
			return assistant("unused"), nil
		})})
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("idle ACP process did not release stdin on shutdown")
	}
}

func TestWorktreesShareProjectIdentity(t *testing.T) {
	root := t.TempDir()
	run := runShell(root)
	for _, command := range []string{"git init -q", "git config user.email test@example.invalid", "git config user.name Test", "git commit -q --allow-empty -m initial", "git remote add origin https://github.com/example/recall-fixture.git", "git worktree add -q -b test-branch sibling"} {
		data, _ := json.Marshal(map[string]string{"command": command})
		result, err := run(context.Background(), data)
		if err != nil {
			t.Fatal(err)
		}
		if result.(map[string]any)["exit_code"].(int) != 0 {
			t.Fatalf("fixture setup failed: %+v", result)
		}
	}
	if ProjectKey(root) != ProjectKey(filepath.Join(root, "sibling")) {
		t.Fatal("git worktree got separate project memory")
	}
}
