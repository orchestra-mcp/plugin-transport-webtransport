package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	pluginv1 "github.com/orchestra-mcp/gen-go/orchestra/plugin/v1"
	"github.com/orchestra-mcp/sdk-go/protocol"
	"google.golang.org/protobuf/types/known/structpb"
)

// mockSender implements Sender for testing without a real QUIC connection.
type mockSender struct {
	sendFunc func(ctx context.Context, req *pluginv1.PluginRequest) (*pluginv1.PluginResponse, error)
}

func (m *mockSender) Send(ctx context.Context, req *pluginv1.PluginRequest) (*pluginv1.PluginResponse, error) {
	if m.sendFunc != nil {
		return m.sendFunc(ctx, req)
	}
	return nil, fmt.Errorf("mockSender: no sendFunc configured")
}

// testFS is a minimal in-memory FS with a dist/index.html for tests.
var testFS fs.FS = func() fs.FS {
	m := fstest.MapFS{
		"dist/index.html": &fstest.MapFile{Data: []byte(`<!DOCTYPE html><html><body>Test</body></html>`)},
	}
	return m
}()

// newTestGateway creates a Gateway with a no-op sender, empty apiKey, and
// the test embedded FS.
func newTestGateway(sender Sender, apiKey string) *Gateway {
	return NewGateway(sender, apiKey, testFS)
}

// doRPC sends a JSON-RPC request body to the Gateway's handleRPC handler and
// returns the http.ResponseRecorder.
func doRPC(t *testing.T, gw *Gateway, body string, extraHeaders ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(extraHeaders); i += 2 {
		req.Header.Set(extraHeaders[i], extraHeaders[i+1])
	}
	rr := httptest.NewRecorder()
	gw.handleRPC(rr, req)
	return rr
}

// parseRPCResponse unmarshals the response body into a JSONRPCResponse.
func parseRPCResponse(t *testing.T, rr *httptest.ResponseRecorder) protocol.JSONRPCResponse {
	t.Helper()
	var resp protocol.JSONRPCResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v\nbody: %s", err, rr.Body.String())
	}
	return resp
}

// --- HTTP layer tests ---

// Test 1
func TestHealthEndpoint(t *testing.T) {
	gw := newTestGateway(&mockSender{}, "")
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	gw.handleHealth(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	if rr.Body.String() != "ok" {
		t.Errorf("body: got %q, want %q", rr.Body.String(), "ok")
	}
}

// Test 2
func TestCORSPreflight(t *testing.T) {
	gw := newTestGateway(&mockSender{}, "")
	req := httptest.NewRequest(http.MethodOptions, "/rpc", nil)
	rr := httptest.NewRecorder()
	corsMiddleware(http.HandlerFunc(gw.handleCORSPreflight)).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusNoContent)
	}
	if rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("CORS origin header missing or wrong: %q", rr.Header().Get("Access-Control-Allow-Origin"))
	}
	if rr.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Error("Access-Control-Allow-Methods header missing")
	}
	if rr.Header().Get("Access-Control-Allow-Headers") == "" {
		t.Error("Access-Control-Allow-Headers header missing")
	}
}

// Test 3
func TestCORSHeadersOnRPCResponse(t *testing.T) {
	gw := newTestGateway(&mockSender{}, "")
	req := httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	corsMiddleware(http.HandlerFunc(gw.handleRPC)).ServeHTTP(rr, req)

	if rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("CORS origin header missing on RPC response: %q", rr.Header().Get("Access-Control-Allow-Origin"))
	}
}

// Test 4
func TestRPCInvalidContentType(t *testing.T) {
	gw := newTestGateway(&mockSender{}, "")
	req := httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	gw.handleRPC(rr, req)

	if rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusUnsupportedMediaType)
	}
}

// Test 5
func TestRPCEmptyBody(t *testing.T) {
	gw := newTestGateway(&mockSender{}, "")
	req := httptest.NewRequest(http.MethodPost, "/rpc", bytes.NewReader(nil))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	gw.handleRPC(rr, req)

	resp := parseRPCResponse(t, rr)
	if resp.Error == nil {
		t.Fatal("expected JSON-RPC error for empty body")
	}
	if resp.Error.Code != protocol.ParseError {
		t.Errorf("error code: got %d, want %d", resp.Error.Code, protocol.ParseError)
	}
}

// Test 6
func TestRPCInvalidJSON(t *testing.T) {
	gw := newTestGateway(&mockSender{}, "")
	rr := doRPC(t, gw, `{invalid json}`)
	resp := parseRPCResponse(t, rr)

	if resp.Error == nil {
		t.Fatal("expected JSON-RPC parse error")
	}
	if resp.Error.Code != protocol.ParseError {
		t.Errorf("error code: got %d, want %d", resp.Error.Code, protocol.ParseError)
	}
}

// Test 7
func TestAuthRequired(t *testing.T) {
	gw := newTestGateway(&mockSender{}, "secret-key")
	rr := doRPC(t, gw, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusUnauthorized)
	}
}

// Test 8
func TestAuthSuccess(t *testing.T) {
	gw := newTestGateway(&mockSender{}, "secret-key")
	rr := doRPC(t, gw, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Authorization", "Bearer secret-key")

	if rr.Code == http.StatusUnauthorized {
		t.Error("expected auth to succeed with correct Bearer token")
	}
	resp := parseRPCResponse(t, rr)
	if resp.Error != nil {
		t.Errorf("unexpected error: %+v", resp.Error)
	}
}

// --- Handler tests ---

// Test 9
func TestInitialize(t *testing.T) {
	gw := newTestGateway(&mockSender{}, "")
	rr := doRPC(t, gw, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0.0"}}}`)
	resp := parseRPCResponse(t, rr)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}

	resultBytes, _ := json.Marshal(resp.Result)
	var initResult protocol.MCPInitializeResult
	if err := json.Unmarshal(resultBytes, &initResult); err != nil {
		t.Fatalf("unmarshal init result: %v", err)
	}

	if initResult.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocolVersion: got %q, want %q", initResult.ProtocolVersion, "2024-11-05")
	}
	if initResult.ServerInfo.Name != "orchestra" {
		t.Errorf("serverInfo.name: got %q, want %q", initResult.ServerInfo.Name, "orchestra")
	}
	if initResult.Capabilities.Tools == nil {
		t.Error("expected capabilities.tools to be set")
	}
	if initResult.Capabilities.Prompts == nil {
		t.Error("expected capabilities.prompts to be set")
	}
}

// Test 10
func TestPing(t *testing.T) {
	gw := newTestGateway(&mockSender{}, "")
	rr := doRPC(t, gw, `{"jsonrpc":"2.0","id":42,"method":"ping"}`)
	resp := parseRPCResponse(t, rr)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	if string(resultBytes) != "{}" {
		t.Errorf("ping result: got %s, want {}", string(resultBytes))
	}
}

// Test 11
func TestToolsList(t *testing.T) {
	schema, _ := structpb.NewStruct(map[string]any{
		"type":       "object",
		"properties": map[string]any{"project_id": map[string]any{"type": "string"}},
	})
	sender := &mockSender{
		sendFunc: func(ctx context.Context, req *pluginv1.PluginRequest) (*pluginv1.PluginResponse, error) {
			if req.GetListTools() == nil {
				t.Error("expected ListTools request")
			}
			return &pluginv1.PluginResponse{
				RequestId: req.RequestId,
				Response: &pluginv1.PluginResponse_ListTools{
					ListTools: &pluginv1.ListToolsResponse{
						Tools: []*pluginv1.ToolDefinition{
							{Name: "create_feature", Description: "Create a feature", InputSchema: schema},
							{Name: "list_features", Description: "List features"},
						},
					},
				},
			}, nil
		},
	}

	gw := newTestGateway(sender, "")
	rr := doRPC(t, gw, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	resp := parseRPCResponse(t, rr)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var listResult toolsListResult
	if err := json.Unmarshal(resultBytes, &listResult); err != nil {
		t.Fatalf("unmarshal tools list: %v", err)
	}
	if len(listResult.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(listResult.Tools))
	}
	if listResult.Tools[0].Name != "create_feature" {
		t.Errorf("tool[0].name: got %q", listResult.Tools[0].Name)
	}
}

// Test 12
func TestToolsCall(t *testing.T) {
	result, _ := structpb.NewStruct(map[string]any{"text": "feature created"})
	sender := &mockSender{
		sendFunc: func(ctx context.Context, req *pluginv1.PluginRequest) (*pluginv1.PluginResponse, error) {
			tc := req.GetToolCall()
			if tc == nil {
				t.Error("expected ToolCall request")
				return nil, fmt.Errorf("expected ToolCall")
			}
			if tc.CallerPlugin != "transport.webtransport" {
				t.Errorf("caller_plugin: got %q, want %q", tc.CallerPlugin, "transport.webtransport")
			}
			return &pluginv1.PluginResponse{
				RequestId: req.RequestId,
				Response: &pluginv1.PluginResponse_ToolCall{
					ToolCall: &pluginv1.ToolResponse{Success: true, Result: result},
				},
			}, nil
		},
	}

	gw := newTestGateway(sender, "")
	rr := doRPC(t, gw, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"create_feature","arguments":{"project_id":"test"}}}`)
	resp := parseRPCResponse(t, rr)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var mcpResult protocol.MCPToolResult
	if err := json.Unmarshal(resultBytes, &mcpResult); err != nil {
		t.Fatalf("unmarshal tool result: %v", err)
	}
	if mcpResult.IsError {
		t.Error("expected IsError=false")
	}
	if len(mcpResult.Content) == 0 || mcpResult.Content[0].Text != "feature created" {
		t.Errorf("content text: got %q", mcpResult.Content[0].Text)
	}
}

// Test 13
func TestToolsCallError(t *testing.T) {
	sender := &mockSender{
		sendFunc: func(ctx context.Context, req *pluginv1.PluginRequest) (*pluginv1.PluginResponse, error) {
			return &pluginv1.PluginResponse{
				RequestId: req.RequestId,
				Response: &pluginv1.PluginResponse_ToolCall{
					ToolCall: &pluginv1.ToolResponse{
						Success:      false,
						ErrorCode:    "not_found",
						ErrorMessage: `tool "nonexistent" not found`,
					},
				},
			}, nil
		},
	}

	gw := newTestGateway(sender, "")
	rr := doRPC(t, gw, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nonexistent"}}`)
	resp := parseRPCResponse(t, rr)

	if resp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %+v", resp.Error)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var mcpResult protocol.MCPToolResult
	if err := json.Unmarshal(resultBytes, &mcpResult); err != nil {
		t.Fatalf("unmarshal tool result: %v", err)
	}
	if !mcpResult.IsError {
		t.Error("expected IsError=true")
	}
	if !strings.Contains(mcpResult.Content[0].Text, "not found") {
		t.Errorf("error text: got %q", mcpResult.Content[0].Text)
	}
}

// Test 14
func TestToolsCallMissingName(t *testing.T) {
	gw := newTestGateway(&mockSender{}, "")
	rr := doRPC(t, gw, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"arguments":{}}}`)
	resp := parseRPCResponse(t, rr)

	if resp.Error == nil {
		t.Fatal("expected error for missing tool name")
	}
	if resp.Error.Code != protocol.InvalidParams {
		t.Errorf("error code: got %d, want %d", resp.Error.Code, protocol.InvalidParams)
	}
	if !strings.Contains(resp.Error.Message, "name") {
		t.Errorf("error message should mention 'name': %s", resp.Error.Message)
	}
}

// Test 15
func TestMethodNotFound(t *testing.T) {
	gw := newTestGateway(&mockSender{}, "")
	rr := doRPC(t, gw, `{"jsonrpc":"2.0","id":6,"method":"unknown/method"}`)
	resp := parseRPCResponse(t, rr)

	if resp.Error == nil {
		t.Fatal("expected JSON-RPC error for unknown method")
	}
	if resp.Error.Code != protocol.MethodNotFound {
		t.Errorf("error code: got %d, want %d", resp.Error.Code, protocol.MethodNotFound)
	}
	if !strings.Contains(resp.Error.Message, "unknown/method") {
		t.Errorf("error message should mention method: %s", resp.Error.Message)
	}
}

// Test 16
func TestNotification(t *testing.T) {
	gw := newTestGateway(&mockSender{}, "")
	rr := doRPC(t, gw, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	// Notification — no ID — should return 204 with no body.
	if rr.Code != http.StatusNoContent {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusNoContent)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("expected empty body for notification, got: %s", rr.Body.String())
	}
}

// --- Translator tests ---

// Test 17
func TestStructToMap(t *testing.T) {
	s, _ := structpb.NewStruct(map[string]any{
		"name":   "test",
		"count":  42.0,
		"active": true,
		"tags":   []any{"a", "b"},
		"nested": map[string]any{"key": "val"},
	})

	m := StructToMap(s)
	if m["name"] != "test" {
		t.Errorf("name: got %v", m["name"])
	}
	if m["count"] != 42.0 {
		t.Errorf("count: got %v", m["count"])
	}
	if m["active"] != true {
		t.Errorf("active: got %v", m["active"])
	}
	tags, ok := m["tags"].([]any)
	if !ok || len(tags) != 2 {
		t.Errorf("tags: got %v", m["tags"])
	}
	nested, ok := m["nested"].(map[string]any)
	if !ok || nested["key"] != "val" {
		t.Errorf("nested: got %v", m["nested"])
	}
}

// Test 18
func TestToolDefinitionToMCP(t *testing.T) {
	schema, _ := structpb.NewStruct(map[string]any{
		"type":       "object",
		"properties": map[string]any{"id": map[string]any{"type": "string"}},
	})
	td := &pluginv1.ToolDefinition{
		Name:        "my_tool",
		Description: "Does things",
		InputSchema: schema,
	}

	mcp := ToolDefinitionToMCP(td)
	if mcp.Name != "my_tool" {
		t.Errorf("name: got %q", mcp.Name)
	}
	schemaMap, ok := mcp.InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("inputSchema should be map, got %T", mcp.InputSchema)
	}
	if schemaMap["type"] != "object" {
		t.Errorf("schema type: got %v", schemaMap["type"])
	}
}

// Test 19
func TestToolResponseToMCPSuccess(t *testing.T) {
	result, _ := structpb.NewStruct(map[string]any{"text": "operation completed"})
	resp := &pluginv1.ToolResponse{Success: true, Result: result}

	mcp := ToolResponseToMCP(resp)
	if mcp.IsError {
		t.Error("expected IsError=false")
	}
	if len(mcp.Content) != 1 || mcp.Content[0].Text != "operation completed" {
		t.Errorf("content: got %+v", mcp.Content)
	}
}

// Test 20
func TestToolResponseToMCPError(t *testing.T) {
	resp := &pluginv1.ToolResponse{
		Success:      false,
		ErrorCode:    "validation_error",
		ErrorMessage: "title is required",
	}

	mcp := ToolResponseToMCP(resp)
	if !mcp.IsError {
		t.Error("expected IsError=true")
	}
	if len(mcp.Content) != 1 || mcp.Content[0].Text != "title is required" {
		t.Errorf("content: got %+v", mcp.Content)
	}
}

// --- Prompt tests ---

// Test 21
func TestPromptsList(t *testing.T) {
	sender := &mockSender{
		sendFunc: func(ctx context.Context, req *pluginv1.PluginRequest) (*pluginv1.PluginResponse, error) {
			if req.GetListPrompts() == nil {
				t.Error("expected ListPrompts request")
			}
			return &pluginv1.PluginResponse{
				RequestId: req.RequestId,
				Response: &pluginv1.PluginResponse_ListPrompts{
					ListPrompts: &pluginv1.ListPromptsResponse{
						Prompts: []*pluginv1.PromptDefinition{
							{
								Name:        "setup-project",
								Description: "Set up a project",
								Arguments:   []*pluginv1.PromptArgument{{Name: "name", Required: true}},
							},
						},
					},
				},
			}, nil
		},
	}

	gw := newTestGateway(sender, "")
	rr := doRPC(t, gw, `{"jsonrpc":"2.0","id":7,"method":"prompts/list"}`)
	resp := parseRPCResponse(t, rr)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var listResult promptsListResult
	if err := json.Unmarshal(resultBytes, &listResult); err != nil {
		t.Fatalf("unmarshal prompts list: %v", err)
	}
	if len(listResult.Prompts) != 1 {
		t.Fatalf("expected 1 prompt, got %d", len(listResult.Prompts))
	}
	if listResult.Prompts[0].Name != "setup-project" {
		t.Errorf("prompt name: got %q", listResult.Prompts[0].Name)
	}
	if len(listResult.Prompts[0].Arguments) != 1 || !listResult.Prompts[0].Arguments[0].Required {
		t.Error("expected required argument")
	}
}

// Test 22
func TestPromptsGet(t *testing.T) {
	sender := &mockSender{
		sendFunc: func(ctx context.Context, req *pluginv1.PluginRequest) (*pluginv1.PluginResponse, error) {
			pg := req.GetPromptGet()
			if pg == nil {
				t.Error("expected PromptGet request")
				return nil, fmt.Errorf("expected PromptGet")
			}
			if pg.PromptName != "setup-project" {
				t.Errorf("prompt name: got %q", pg.PromptName)
			}
			if pg.Arguments["project_name"] != "demo" {
				t.Errorf("argument: got %q", pg.Arguments["project_name"])
			}
			return &pluginv1.PluginResponse{
				RequestId: req.RequestId,
				Response: &pluginv1.PluginResponse_PromptGet{
					PromptGet: &pluginv1.PromptGetResponse{
						Description: "Project setup guide",
						Messages: []*pluginv1.PromptMessage{
							{Role: "user", Content: &pluginv1.ContentBlock{Type: "text", Text: "Set up demo."}},
						},
					},
				},
			}, nil
		},
	}

	gw := newTestGateway(sender, "")
	rr := doRPC(t, gw, `{"jsonrpc":"2.0","id":8,"method":"prompts/get","params":{"name":"setup-project","arguments":{"project_name":"demo"}}}`)
	resp := parseRPCResponse(t, rr)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var promptResult protocol.MCPPromptResult
	if err := json.Unmarshal(resultBytes, &promptResult); err != nil {
		t.Fatalf("unmarshal prompt result: %v", err)
	}
	if promptResult.Description != "Project setup guide" {
		t.Errorf("description: got %q", promptResult.Description)
	}
	if len(promptResult.Messages) != 1 || promptResult.Messages[0].Role != "user" {
		t.Errorf("messages: got %+v", promptResult.Messages)
	}
	if promptResult.Messages[0].Content.Text != "Set up demo." {
		t.Errorf("content text: got %q", promptResult.Messages[0].Content.Text)
	}
}
