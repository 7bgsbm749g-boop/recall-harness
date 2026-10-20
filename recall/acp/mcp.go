package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

type MCPConfig struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Env     []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"env"`
	URL     string `json:"url"`
	Headers []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
}
type MCPDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations struct {
		ReadOnlyHint bool `json:"readOnlyHint"`
	} `json:"annotations"`
}
type MCPClient struct {
	config       MCPConfig
	mu           sync.Mutex
	next         uint64
	process      *exec.Cmd
	stdin        io.WriteCloser
	scanner      *bufio.Scanner
	http         *http.Client
	sessionID    string
	protocol     string
	instructions string
	cancel       context.CancelFunc
	dead         atomic.Bool
	closeOnce    sync.Once
}

func ConnectMCP(ctx context.Context, config MCPConfig) (*MCPClient, error) {
	ctx, cancel := context.WithCancel(ctx)
	c := &MCPClient{config: config, cancel: cancel, http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("MCP redirects are not followed") }}}
	if config.Type == "" || config.Type == "stdio" {
		if config.Command == "" {
			cancel()
			return nil, errors.New("MCP stdio command is missing")
		}
		command := exec.CommandContext(ctx, config.Command, config.Args...)
		configureCommand(command)
		command.Env = os.Environ()
		for _, entry := range config.Env {
			command.Env = append(command.Env, entry.Name+"="+entry.Value)
		}
		command.Stderr = io.Discard
		input, err := command.StdinPipe()
		if err != nil {
			cancel()
			return nil, err
		}
		output, err := command.StdoutPipe()
		if err != nil {
			cancel()
			return nil, err
		}
		if err = command.Start(); err != nil {
			cancel()
			return nil, err
		}
		c.process = command
		c.stdin = input
		c.scanner = bufio.NewScanner(output)
		c.scanner.Buffer(make([]byte, 65536), 16<<20)
	} else if config.Type == "http" {
		parsed, err := url.Parse(config.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			cancel()
			return nil, errors.New("invalid MCP HTTP URL")
		}
	} else {
		cancel()
		return nil, fmt.Errorf("unsupported MCP transport %q", config.Type)
	}
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
		Instructions    string `json:"instructions"`
	}
	if err := c.request(ctx, "initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "Recall", "version": Version}}, &initialized); err != nil {
		c.Close()
		return nil, err
	}
	c.protocol = initialized.ProtocolVersion
	c.instructions = initialized.Instructions
	if err := c.notification(ctx, "notifications/initialized", map[string]any{}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
func (c *MCPClient) Close() {
	c.closeOnce.Do(func() {
		c.dead.Store(true)
		c.cancel()
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		if c.process != nil {
			_ = c.process.Wait()
		}
	})
}
func (c *MCPClient) notification(ctx context.Context, method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return err
	}
	if c.stdin != nil {
		_, err = c.stdin.Write(append(data, '\n'))
		return err
	}
	_, err = c.httpMessage(ctx, data, "")
	return err
}
func (c *MCPClient) request(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	id := fmt.Sprintf("recall-mcp-%d", c.next)
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	var message envelope
	if c.stdin != nil {
		if _, err = c.stdin.Write(append(data, '\n')); err != nil {
			return err
		}
		// Cancel the command to release a blocked pipe read when the active tool
		// is cancelled. The next prompt reconnects rather than reusing it.
		stop := context.AfterFunc(ctx, func() { c.dead.Store(true); c.cancel() })
		defer stop()
		for c.scanner.Scan() {
			if err = json.Unmarshal(c.scanner.Bytes(), &message); err != nil {
				return err
			}
			var responseID string
			_ = json.Unmarshal(message.ID, &responseID)
			if responseID == id {
				break
			}
			if message.Method != "" && len(message.ID) > 0 {
				reply, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": message.ID, "error": rpcError{-32601, "Recall does not support this MCP client request"}})
				if _, err = c.stdin.Write(append(reply, '\n')); err != nil {
					return err
				}
			}
		}
		if err = c.scanner.Err(); err != nil {
			return err
		}
		var responseID string
		_ = json.Unmarshal(message.ID, &responseID)
		if responseID != id {
			return errors.New("MCP process closed before replying")
		}
	} else {
		payload, err := c.httpMessage(ctx, data, id)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(payload, &message); err != nil {
			return err
		}
	}
	if message.Error != nil {
		return fmt.Errorf("MCP request failed: %s", message.Error.Message)
	}
	if len(message.Result) == 0 {
		return errors.New("MCP reply has no result")
	}
	return json.Unmarshal(message.Result, result)
}
func (c *MCPClient) httpMessage(ctx context.Context, data []byte, wantID string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", c.config.URL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for _, header := range c.config.Headers {
		req.Header.Set(header.Name, header.Value)
	}
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	if c.protocol != "" {
		req.Header.Set("Mcp-Protocol-Version", c.protocol)
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("MCP HTTP request failed; credential-bearing request details withheld")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("MCP HTTP status %d", response.StatusCode)
	}
	if id := response.Header.Get("Mcp-Session-Id"); id != "" {
		c.sessionID = id
	}
	if wantID == "" {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return nil, nil
	}
	if strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		scanner := bufio.NewScanner(io.LimitReader(response.Body, 16<<20))
		scanner.Buffer(make([]byte, 65536), 16<<20)
		var event strings.Builder
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data:") {
				if event.Len() > 0 {
					event.WriteByte('\n')
				}
				event.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
			if line == "" && event.Len() > 0 {
				raw := []byte(event.String())
				event.Reset()
				var envelope struct {
					ID string `json:"id"`
				}
				if json.Unmarshal(raw, &envelope) == nil && envelope.ID == wantID {
					return raw, nil
				}
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("MCP event stream ended without the requested response")
	}
	data, err = io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 16<<20 {
		return nil, errors.New("MCP response exceeds 16 MiB")
	}
	var envelope struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.ID != wantID {
		return nil, errors.New("MCP response ID mismatch")
	}
	return data, nil
}
func (c *MCPClient) List(ctx context.Context) ([]MCPDefinition, error) {
	var all []MCPDefinition
	cursor := ""
	for page := 0; page < 100; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var result struct {
			Tools      []MCPDefinition `json:"tools"`
			NextCursor string          `json:"nextCursor"`
		}
		if err := c.request(ctx, "tools/list", params, &result); err != nil {
			return nil, err
		}
		all = append(all, result.Tools...)
		if result.NextCursor == "" {
			return all, nil
		}
		if result.NextCursor == cursor {
			return nil, errors.New("MCP tools pagination did not advance")
		}
		cursor = result.NextCursor
	}
	return nil, errors.New("too many MCP tools pages")
}
func (c *MCPClient) Call(ctx context.Context, name string, args json.RawMessage) (any, error) {
	var result any
	err := c.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &result)
	return result, err
}
