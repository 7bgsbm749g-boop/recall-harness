package acp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type boundTool struct {
	definition tool.Definition
	readOnly   bool
	run        func(context.Context, json.RawMessage) (any, error)
}
type registry struct {
	tool.Registry
	tools        map[string]boundTool
	instructions string
}

func (r *registry) add(name, description string, parameters map[string]any, readOnly bool, run func(context.Context, json.RawMessage) (any, error)) {
	r.tools[name] = boundTool{definition: tool.Definition{Tool: llm.Tool{Type: llm.ToolFunction, Name: name, Description: description, Parameters: parameters}}, readOnly: readOnly, run: run}
}
func (r *registry) StaticDefinitions() []tool.Definition {
	base := r.Registry.StaticDefinitions()
	out := make([]tool.Definition, 0, len(base)+len(r.tools))
	for _, d := range base {
		if _, exists := r.tools[d.Tool.Name]; !exists {
			out = append(out, d)
		}
	}
	for _, bound := range r.tools {
		out = append(out, bound.definition)
	}
	return out
}
func (r *registry) Resolve(name string) (tool.Translator, bool) {
	if _, ok := r.tools[name]; ok {
		return jobTranslator{}, true
	}
	return r.Registry.Resolve(name)
}

type jobPlan struct {
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}
type jobTranslator struct{}

func (jobTranslator) Translate(ctx tool.Context, call llm.ToolCall) tool.CallStatus {
	if !json.Valid([]byte(call.Arguments)) {
		return tool.CallStatus{Error: "Tool arguments must be valid JSON"}
	}
	data, err := jsonv2.Marshal(jobPlan{CallID: call.CallID, Name: call.Name, Arguments: json.RawMessage(call.Arguments)})
	if err != nil {
		return tool.CallStatus{Error: err.Error()}
	}
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "recall.tool", Version: 1, Data: data})
	if err != nil {
		return tool.CallStatus{Error: err.Error()}
	}
	spec.MaxOutputLength = 32000
	return tool.CallStatus{WaitingFor: []operation.ID{ctx.Submit(spec)}}
}

type jobResult struct{ result llm.ToolResult }

func (r jobResult) ToLLMResult() llm.ToolResult { return r.result }
func (jobTranslator) TranslateResult(callID string, status tool.CallStatus, operations []operation.Operation) (tool.Result, error) {
	result := llm.ToolResult{CallID: callID}
	if status.Error != "" {
		result.Output = []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: status.Error}}
		return jobResult{result}, nil
	}
	if len(operations) != 1 {
		return nil, errors.New("Recall tool requires one durable operation")
	}
	current := operations[0]
	state, err := operation.DecodeRemoteJobState(current)
	if err != nil {
		return nil, err
	}
	result.Running = current.Status == operation.StatusReady || current.Status == operation.StatusAwaiting || current.Status == operation.StatusCanceling
	text := state.TerminalResult
	if state.TerminalError != "" {
		text += "\nTool error: " + state.TerminalError
	}
	result.Output = []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: text}}
	return jobResult{result}, nil
}

type jobHandler struct {
	ctx      context.Context
	session  *Session
	registry *registry
	updates  chan operation.Operation
	mu       sync.Mutex
	cancels  map[operation.ID]context.CancelFunc
}

func newJobHandler(ctx context.Context, s *Session, r *registry) *jobHandler {
	return &jobHandler{ctx: ctx, session: s, registry: r, updates: make(chan operation.Operation), cancels: map[operation.ID]context.CancelFunc{}}
}
func (h *jobHandler) RemoteJobPlanType() operation.RemoteJobPlanType       { return "recall.tool" }
func (h *jobHandler) RemoteJobPlanVersion() operation.RemoteJobPlanVersion { return 1 }
func (h *jobHandler) RemoteJobUpdates() <-chan operation.Operation         { return h.updates }
func (h *jobHandler) CancelRemoteJob(id operation.ID, _ string) error {
	h.mu.Lock()
	cancel := h.cancels[id]
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}
func (h *jobHandler) AddRemoteJob(current operation.Operation) error {
	ctx, cancel := context.WithCancel(h.ctx)
	h.mu.Lock()
	if _, exists := h.cancels[current.ID]; exists {
		h.mu.Unlock()
		cancel()
		return nil
	}
	h.cancels[current.ID] = cancel
	h.mu.Unlock()
	go func() {
		defer cancel()
		defer func() { h.mu.Lock(); delete(h.cancels, current.ID); h.mu.Unlock() }()
		state, err := operation.DecodeRemoteJobState(current)
		if err != nil {
			return
		}
		var plan jobPlan
		if err = jsonv2.Unmarshal(state.Plan.Data, &plan); err != nil {
			h.finish(ctx, current, state, "", err)
			return
		}
		bound, exists := h.registry.tools[plan.Name]
		if !exists {
			h.finish(ctx, current, state, "", errors.New("tool is no longer available"))
			return
		}
		if !bound.readOnly {
			if err = h.permission(ctx, plan); err != nil {
				h.finish(ctx, current, state, "", err)
				return
			}
		}
		_ = h.session.server.notify(h.session.meta.ID, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": plan.CallID, "status": "in_progress"})
		// A crash after a mutating tool starts has an unknown outcome. Preserve
		// its receipt and refuse automatic re-execution on session recovery.
		receipt := filepath.Join(h.session.directory, "receipts")
		if err = os.MkdirAll(receipt, 0700); err != nil {
			h.finish(ctx, current, state, "", err)
			return
		}
		hash := sha256.Sum256([]byte(current.ID))
		stem := filepath.Join(receipt, hex.EncodeToString(hash[:]))
		resultPath := stem + ".result.json"
		if !bound.readOnly {
			if data, readErr := os.ReadFile(resultPath); readErr == nil {
				var saved struct {
					Text  string
					Error string
				}
				if json.Unmarshal(data, &saved) == nil {
					var savedErr error
					if saved.Error != "" {
						savedErr = errors.New(saved.Error)
					}
					h.finish(ctx, current, state, saved.Text, savedErr)
					return
				}
			}
			marker, markErr := os.OpenFile(stem+".started", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if markErr != nil {
				if errors.Is(markErr, os.ErrExist) {
					markErr = errors.New("previous tool execution has an unknown outcome; inspect the workspace or service before retrying")
				}
				h.finish(ctx, current, state, "", markErr)
				return
			}
			_, writeErr := marker.Write([]byte(plan.Name))
			syncErr := marker.Sync()
			closeErr := marker.Close()
			if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
				h.finish(ctx, current, state, "", err)
				return
			}
		}
		value, runErr := bound.run(ctx, plan.Arguments)
		encoded, encodeErr := json.Marshal(value)
		if runErr == nil {
			runErr = encodeErr
		}
		text := string(encoded)
		if !bound.readOnly {
			saved := struct {
				Text  string
				Error string
			}{Text: text}
			if runErr != nil {
				saved.Error = runErr.Error()
			}
			data, _ := json.Marshal(saved)
			if err = os.WriteFile(resultPath, data, 0600); err != nil {
				runErr = errors.Join(runErr, err)
			}
		}
		status := "completed"
		if runErr != nil {
			status = "failed"
		}
		_ = h.session.server.notify(h.session.meta.ID, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": plan.CallID, "status": status, "content": []any{map[string]any{"type": "content", "content": map[string]string{"type": "text", "text": truncate(text, 32000)}}}})
		h.finish(ctx, current, state, text, runErr)
	}()
	return nil
}
func truncate(text string, limit int) string {
	if len(text) > limit {
		return text[:limit] + "\n[output truncated; full result is retained in the tool receipt]"
	}
	return text
}
func (h *jobHandler) finish(ctx context.Context, current operation.Operation, state operation.RemoteJobState, text string, err error) {
	state.TerminalResult = text
	status := operation.StatusCompleted
	if err != nil {
		state.TerminalError = err.Error()
		status = operation.StatusFailed
	}
	if ctx.Err() != nil {
		status = operation.StatusCanceled
		state.TerminalError = "Tool cancelled; inspect external state before retrying mutations"
	}
	step, stepErr := operation.UpdateRemoteJob(current, state, status)
	if stepErr != nil {
		return
	}
	select {
	case h.updates <- *step.Operation:
	case <-h.ctx.Done():
	}
}
func (h *jobHandler) permission(ctx context.Context, plan jobPlan) error {
	if h.session.meta.Mode == "plan" {
		return errors.New("Plan mode permits read-only tools; switch to Ask or Full access before changing files or running commands")
	}
	if h.session.meta.Mode == "full-access" {
		return nil
	}
	var result struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	params := map[string]any{"sessionId": h.session.meta.ID, "toolCall": map[string]any{"toolCallId": plan.CallID, "title": plan.Name, "kind": toolKind(plan.Name), "status": "pending", "rawInput": plan.Arguments}, "options": []any{map[string]string{"optionId": "allow_once", "name": "Allow once", "kind": "allow_once"}, map[string]string{"optionId": "reject_once", "name": "Reject", "kind": "reject_once"}}}
	if err := h.session.server.call(ctx, "session/request_permission", params, &result); err != nil {
		return err
	}
	if result.Outcome.Outcome != "selected" || result.Outcome.OptionID != "allow_once" {
		return errors.New("tool permission was not granted")
	}
	return nil
}
func safeName(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, value)
}
func toolKind(name string) string {
	switch name {
	case "Bash":
		return "execute"
	case "ViewImage", "memory_read":
		return "read"
	case "memory_search":
		return "search"
	default:
		return "other"
	}
}

type cappedBuffer struct {
	mu    sync.Mutex
	data  []byte
	limit int
	total int
}

func (b *cappedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += len(data)
	remaining := b.limit - len(b.data)
	if remaining > 0 {
		b.data = append(b.data, data[:min(len(data), remaining)]...)
	}
	return len(data), nil
}
func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := string(b.data)
	if b.total > len(b.data) {
		text += "\n[command output truncated]"
	}
	return text
}

func runShell(cwd string) func(context.Context, json.RawMessage) (any, error) {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var p struct {
			Command string `json:"command"`
			Timeout int    `json:"timeout_seconds"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, err
		}
		if strings.TrimSpace(p.Command) == "" {
			return nil, errors.New("command is required")
		}
		if p.Timeout <= 0 {
			p.Timeout = 300
		}
		if p.Timeout > 3600 {
			p.Timeout = 3600
		}
		ctx, cancel := context.WithTimeout(ctx, time.Duration(p.Timeout)*time.Second)
		defer cancel()
		shell := os.Getenv("SHELL")
		argv := []string{"-c", p.Command}
		if runtime.GOOS == "windows" {
			shell = "powershell.exe"
			argv = []string{"-NoProfile", "-NonInteractive", "-Command", p.Command}
		} else if shell == "" {
			shell = "/bin/sh"
		}
		command := exec.CommandContext(ctx, shell, argv...)
		command.Dir = cwd
		configureCommand(command)
		stdout := &cappedBuffer{limit: 64000}
		stderr := &cappedBuffer{limit: 16000}
		command.Stdout = stdout
		command.Stderr = stderr
		err := command.Run()
		code := 0
		if command.ProcessState != nil {
			code = command.ProcessState.ExitCode()
		}
		value := map[string]any{"stdout": stdout.String(), "stderr": stderr.String(), "exit_code": code}
		if ctx.Err() != nil {
			return value, ctx.Err()
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return value, nil
		}
		return value, err
	}
}
