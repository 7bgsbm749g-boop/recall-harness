// Package acp exposes Recall through the Agent Client Protocol over stdio.
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/recall/memory"
)

const Version = "0.1.1"

type Config struct {
	DataDirectory string
	Model         string
	Models        []string
	Effort        llm.ReasoningEffort
	Memory        memory.Config
	Client        llm.Adapter
	AuthCheck     func() error
}
type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type Server struct {
	Config      Config
	ctx         context.Context
	cancel      context.CancelFunc
	output      io.Writer
	writeMu     sync.Mutex
	mu          sync.Mutex
	sessions    map[string]*Session
	pending     map[string]chan envelope
	next        atomic.Uint64
	wg          sync.WaitGroup
	initialized bool
}

func Serve(ctx context.Context, input io.Reader, output io.Writer, c Config) error {
	if c.Client == nil || c.Model == "" || c.DataDirectory == "" {
		return errors.New("Recall requires a model, client, and data directory")
	}
	if err := c.Memory.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(c.DataDirectory, "sessions"), 0700); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	stopInputClose := context.AfterFunc(ctx, func() {
		if closer, ok := input.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	defer stopInputClose()
	s := &Server{Config: c, ctx: ctx, cancel: cancel, output: output, sessions: map[string]*Session{}, pending: map[string]chan envelope{}}
	defer func() {
		cancel()
		s.wg.Wait()
		s.mu.Lock()
		sessions := make([]*Session, 0, len(s.sessions))
		for _, session := range s.sessions {
			sessions = append(sessions, session)
		}
		s.mu.Unlock()
		for _, session := range sessions {
			session.close()
		}
	}()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 65536), 32<<20)
	lines := make(chan []byte)
	scanError := make(chan error, 1)
	go func() {
		defer close(lines)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case lines <- line:
			case <-ctx.Done():
				scanError <- nil
				return
			}
		}
		scanError <- scanner.Err()
	}()
	for {
		var line []byte
		select {
		case <-ctx.Done():
			return nil
		case data, open := <-lines:
			if !open {
				return <-scanError
			}
			line = data
		}
		var message envelope
		if err := json.Unmarshal(line, &message); err != nil {
			traceProtocol("incoming", "invalid-json", nil)
			_ = s.send(map[string]any{"jsonrpc": "2.0", "id": nil, "error": rpcError{-32700, "Invalid JSON"}})
			continue
		}
		traceProtocol("incoming", message.Method, message.ID)
		if message.JSONRPC != "2.0" {
			_ = s.respond(message.ID, nil, &rpcError{-32600, "Expected JSON-RPC 2.0"})
			continue
		}
		if message.Method == "" {
			s.mu.Lock()
			reply := s.pending[string(message.ID)]
			s.mu.Unlock()
			if reply != nil {
				select {
				case reply <- message:
				default:
				}
			}
			continue
		}
		if message.Method == "session/cancel" {
			s.cancelSession(message.Params)
			continue
		}
		if message.Method == "initialize" {
			result, err := s.handle(message.Method, message.Params)
			_ = s.respond(message.ID, result, err)
			continue
		}
		s.wg.Go(func() {
			result, err := s.handle(message.Method, message.Params)
			if len(message.ID) > 0 {
				_ = s.respond(message.ID, result, err)
			}
		})
	}
}

func (s *Server) send(v any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return json.NewEncoder(s.output).Encode(v)
}

// Optional diagnostics record protocol metadata only, never prompts, tool
// arguments, credentials, or response content.
func traceProtocol(direction, method string, id json.RawMessage) {
	path := os.Getenv("RECALL_TRACE_PATH")
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer file.Close()
	_ = json.NewEncoder(file).Encode(map[string]any{"time": time.Now().UTC(), "direction": direction, "method": method, "id": id})
}
func (s *Server) respond(id json.RawMessage, result any, err *rpcError) error {
	traceProtocol("outgoing", "response", id)
	if len(id) == 0 {
		return nil
	}
	if err != nil {
		return s.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": err})
	}
	return s.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
func (s *Server) notify(id string, update any) error {
	return s.send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": id, "update": update}})
}
func (s *Server) call(ctx context.Context, method string, params any, result any) error {
	id := fmt.Sprintf("recall-%d", s.next.Add(1))
	raw, _ := json.Marshal(id)
	ch := make(chan envelope, 1)
	s.mu.Lock()
	s.pending[string(raw)] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, string(raw)); s.mu.Unlock() }()
	if err := s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case response := <-ch:
		if response.Error != nil {
			return fmt.Errorf("ACP client request failed: %s", response.Error.Message)
		}
		return json.Unmarshal(response.Result, result)
	case <-ctx.Done():
		return ctx.Err()
	}
}
func fail(err error) *rpcError {
	if err == nil {
		return nil
	}
	return &rpcError{-32000, err.Error()}
}
func (s *Server) getSession(raw json.RawMessage) (*Session, error) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	s.mu.Lock()
	session := s.sessions[p.SessionID]
	s.mu.Unlock()
	if session == nil {
		return nil, errors.New("unknown Recall session")
	}
	return session, nil
}
func (s *Server) cancelSession(raw json.RawMessage) {
	session, err := s.getSession(raw)
	if err == nil {
		session.mu.Lock()
		cancel := session.cancel
		session.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
}

func (s *Server) handle(method string, raw json.RawMessage) (any, *rpcError) {
	if method == "initialize" {
		s.mu.Lock()
		s.initialized = true
		s.mu.Unlock()
		return map[string]any{"protocolVersion": 1, "agentInfo": map[string]string{"name": "Recall", "title": "Recall harness", "version": Version}, "agentCapabilities": map[string]any{"loadSession": true, "sessionCapabilities": map[string]any{"list": map[string]any{}, "resume": map[string]any{}, "close": map[string]any{}}, "promptCapabilities": map[string]any{"embeddedContext": true, "image": true}, "mcpCapabilities": map[string]bool{"http": true}}, "authMethods": []any{}}, nil
	}
	s.mu.Lock()
	initialized := s.initialized
	s.mu.Unlock()
	if !initialized {
		return nil, &rpcError{-32002, "Initialize Recall first"}
	}
	switch method {
	case "session/new", "session/load", "session/resume":
		return s.setup(method, raw)
	case "session/list":
		sessions, err := s.list(raw)
		return map[string]any{"sessions": sessions}, fail(err)
	case "session/prompt":
		session, err := s.getSession(raw)
		if err != nil {
			return nil, fail(err)
		}
		result, err := session.prompt(raw)
		return result, fail(err)
	case "session/set_model", "session/set_mode", "session/set_config_option":
		session, err := s.getSession(raw)
		if err != nil {
			return nil, fail(err)
		}
		result, err := session.configure(method, raw)
		return result, fail(err)
	case "session/close":
		session, err := s.getSession(raw)
		if err != nil {
			return nil, fail(err)
		}
		session.close()
		s.mu.Lock()
		delete(s.sessions, session.meta.ID)
		s.mu.Unlock()
		return map[string]any{}, nil
	case "authenticate":
		if s.Config.AuthCheck != nil {
			if err := s.Config.AuthCheck(); err != nil {
				return nil, &rpcError{-32000, "Recall cannot use the existing model login. Sign in with the provider CLI, then retry."}
			}
		}
		return map[string]any{}, nil
	default:
		return nil, &rpcError{-32601, "Unsupported method: " + method}
	}
}

func (s *Server) setup(method string, raw json.RawMessage) (any, *rpcError) {
	var p struct {
		CWD        string      `json:"cwd"`
		SessionID  string      `json:"sessionId"`
		MCPServers []MCPConfig `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &rpcError{-32602, "Invalid session parameters"}
	}
	if !filepath.IsAbs(p.CWD) {
		return nil, &rpcError{-32602, "Session cwd must be absolute"}
	}
	info, err := os.Stat(p.CWD)
	if err != nil || !info.IsDir() {
		return nil, &rpcError{-32602, "Session workspace does not exist"}
	}
	if s.Config.AuthCheck != nil {
		if err := s.Config.AuthCheck(); err != nil {
			return nil, &rpcError{-32000, "Recall cannot use the existing model login. Sign in with the provider CLI, then retry."}
		}
	}
	session, err := s.openSession(p.CWD, p.SessionID, method == "session/new", p.MCPServers)
	if err != nil {
		return nil, fail(err)
	}
	if method == "session/load" {
		if err := session.replay(); err != nil {
			return nil, fail(err)
		}
	}
	result := session.setupResult()
	if method == "session/new" {
		result["sessionId"] = session.meta.ID
	}
	return result, nil
}

func (s *Server) list(raw json.RawMessage) ([]map[string]any, error) {
	var p struct {
		CWD string `json:"cwd"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(s.Config.DataDirectory, "sessions"))
	if err != nil {
		return nil, err
	}
	result := []map[string]any{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.Config.DataDirectory, "sessions", entry.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var meta Metadata
		if json.Unmarshal(data, &meta) != nil || meta.Version != 1 {
			continue
		}
		if p.CWD != "" && filepath.Clean(meta.CWD) != filepath.Clean(p.CWD) {
			continue
		}
		result = append(result, map[string]any{"sessionId": meta.ID, "cwd": meta.CWD, "title": "Recall session", "updatedAt": meta.Updated.Format(time.RFC3339Nano)})
	}
	return result, nil
}
