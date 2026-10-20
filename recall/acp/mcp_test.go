package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestMCPHTTPAndSSE(t *testing.T) {
	for _, sse := range []bool{false, true} {
		name := "json"
		if sse {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			initialized := false
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Fixture-Key") != "synthetic-test-key" {
					t.Error("MCP header was not passed")
				}
				var request struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil {
					t.Error("invalid MCP request")
					return
				}
				mu.Lock()
				defer mu.Unlock()
				var result any
				switch request.Method {
				case "initialize":
					initialized = true
					w.Header().Set("Mcp-Session-Id", "fixture-session")
					result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}, "instructions": "Fixture instructions."}
				case "notifications/initialized":
					w.WriteHeader(202)
					return
				case "tools/list":
					if !initialized || r.Header.Get("Mcp-Session-Id") != "fixture-session" {
						t.Error("MCP session was not retained")
					}
					result = map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "Return text", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}, "annotations": map[string]bool{"readOnlyHint": true}}}}
				case "tools/call":
					calls++
					result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "MCP_FIXTURE_OK"}}}
				default:
					t.Error("unexpected MCP method")
				}
				data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
				if sse {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(data)
				}
			}))
			defer server.Close()
			cfg := MCPConfig{Type: "http", Name: "fixture", URL: server.URL}
			cfg.Headers = append(cfg.Headers, struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			}{"X-Fixture-Key", "synthetic-test-key"})
			client, err := ConnectMCP(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			definitions, err := client.List(context.Background())
			if err != nil || len(definitions) != 1 || !definitions[0].Annotations.ReadOnlyHint {
				t.Fatalf("bad MCP discovery: %+v, %v", definitions, err)
			}
			result, err := client.Call(context.Background(), "echo", json.RawMessage(`{}`))
			data, _ := json.Marshal(result)
			if err != nil || !strings.Contains(string(data), "MCP_FIXTURE_OK") {
				t.Fatalf("MCP tool failed: %s, %v", data, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if calls != 1 {
				t.Fatal("MCP tool ran more than once")
			}
		})
	}
}

func TestMCPDoesNotFollowCredentialBearingRedirects(t *testing.T) {
	received := false
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { received = true }))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	_, err := ConnectMCP(context.Background(), MCPConfig{Type: "http", URL: origin.URL})
	if err == nil || received {
		t.Fatal("MCP followed a redirect")
	}
}

func TestMCPStdioProcess(t *testing.T) {
	cfg := MCPConfig{Name: "fixture", Command: os.Args[0], Args: []string{"-test.run=^TestMCPHelperProcess$"}}
	cfg.Env = append(cfg.Env, struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}{"RECALL_MCP_TEST_HELPER", "1"})
	client, err := ConnectMCP(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	definitions, err := client.List(context.Background())
	if err != nil || len(definitions) != 1 {
		t.Fatal("stdio MCP discovery failed", err)
	}
	result, err := client.Call(context.Background(), "echo", json.RawMessage(`{"text":"stdio"}`))
	data, _ := json.Marshal(result)
	if err != nil || !strings.Contains(string(data), "STDIO_OK") {
		t.Fatal("stdio tool failed", err)
	}
}
func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("RECALL_MCP_TEST_HELPER") != "1" {
		return
	}
	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if decoder.Decode(&request) != nil {
			os.Exit(0)
		}
		if len(request.ID) == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "helper", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "STDIO_OK"}}}
		}
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}
}
