package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	pluginv1 "github.com/orchestra-mcp/gen-go/orchestra/plugin/v1"
	"github.com/orchestra-mcp/sdk-go/protocol"
	"google.golang.org/protobuf/types/known/structpb"
)

// dispatch routes a JSON-RPC request to the appropriate handler. Notifications
// return nil (no response written). Unknown methods return MethodNotFound.
func (g *Gateway) dispatch(ctx context.Context, req *protocol.JSONRPCRequest) *protocol.JSONRPCResponse {
	switch req.Method {
	case "initialize":
		return g.handleInitialize(req)
	case "ping":
		return g.handlePing(req)
	case "tools/list":
		return g.handleToolsList(ctx, req)
	case "tools/call":
		return g.handleToolsCall(ctx, req)
	case "prompts/list":
		return g.handlePromptsList(ctx, req)
	case "prompts/get":
		return g.handlePromptsGet(ctx, req)
	default:
		if strings.HasPrefix(req.Method, "notifications/") {
			log.Printf("transport.webtransport: notification: %s", req.Method)
			return nil
		}
		return &protocol.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &protocol.JSONRPCError{
				Code:    protocol.MethodNotFound,
				Message: fmt.Sprintf("method not found: %s", req.Method),
			},
		}
	}
}

func (g *Gateway) handleInitialize(req *protocol.JSONRPCRequest) *protocol.JSONRPCResponse {
	return &protocol.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: protocol.MCPInitializeResult{
			ProtocolVersion: "2024-11-05",
			Capabilities: protocol.MCPServerCapabilities{
				Tools:   &protocol.MCPToolsCapability{},
				Prompts: &protocol.MCPPromptsCapability{},
			},
			ServerInfo: protocol.MCPServerInfo{
				Name:    "orchestra",
				Version: "1.0.0",
			},
		},
	}
}

func (g *Gateway) handlePing(req *protocol.JSONRPCRequest) *protocol.JSONRPCResponse {
	return &protocol.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  map[string]any{},
	}
}

type toolsListResult struct {
	Tools []protocol.MCPToolDefinition `json:"tools"`
}

func (g *Gateway) handleToolsList(ctx context.Context, req *protocol.JSONRPCRequest) *protocol.JSONRPCResponse {
	resp, err := g.sender.Send(ctx, &pluginv1.PluginRequest{
		RequestId: fmt.Sprintf("web-lt-%v", req.ID),
		Request: &pluginv1.PluginRequest_ListTools{
			ListTools: &pluginv1.ListToolsRequest{},
		},
	})
	if err != nil {
		return errResp(req.ID, protocol.InternalError, fmt.Sprintf("orchestrator list_tools failed: %v", err))
	}

	lt := resp.GetListTools()
	if lt == nil {
		return errResp(req.ID, protocol.InternalError, "unexpected response type from orchestrator")
	}

	mcpTools := make([]protocol.MCPToolDefinition, 0, len(lt.Tools))
	for _, td := range lt.Tools {
		mcpTools = append(mcpTools, ToolDefinitionToMCP(td))
	}

	return &protocol.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  toolsListResult{Tools: mcpTools},
	}
}

type toolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

func (g *Gateway) handleToolsCall(ctx context.Context, req *protocol.JSONRPCRequest) *protocol.JSONRPCResponse {
	var params toolCallParams
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errResp(req.ID, protocol.InvalidParams, fmt.Sprintf("invalid params: %v", err))
		}
	}

	if params.Name == "" {
		return errResp(req.ID, protocol.InvalidParams, "missing required parameter: name")
	}

	var args *structpb.Struct
	if params.Arguments != nil {
		var err error
		args, err = structpb.NewStruct(params.Arguments)
		if err != nil {
			return errResp(req.ID, protocol.InvalidParams, fmt.Sprintf("invalid arguments: %v", err))
		}
	}

	resp, err := g.sender.Send(ctx, &pluginv1.PluginRequest{
		RequestId: fmt.Sprintf("web-tc-%v", req.ID),
		Request: &pluginv1.PluginRequest_ToolCall{
			ToolCall: &pluginv1.ToolRequest{
				ToolName:     params.Name,
				Arguments:    args,
				CallerPlugin: "transport.webtransport",
			},
		},
	})
	if err != nil {
		return errResp(req.ID, protocol.InternalError, fmt.Sprintf("orchestrator tool_call failed: %v", err))
	}

	tc := resp.GetToolCall()
	if tc == nil {
		return errResp(req.ID, protocol.InternalError, "unexpected response type from orchestrator")
	}

	return &protocol.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  ToolResponseToMCP(tc),
	}
}

type promptsListResult struct {
	Prompts []protocol.MCPPromptDefinition `json:"prompts"`
}

func (g *Gateway) handlePromptsList(ctx context.Context, req *protocol.JSONRPCRequest) *protocol.JSONRPCResponse {
	resp, err := g.sender.Send(ctx, &pluginv1.PluginRequest{
		RequestId: fmt.Sprintf("web-lp-%v", req.ID),
		Request: &pluginv1.PluginRequest_ListPrompts{
			ListPrompts: &pluginv1.ListPromptsRequest{},
		},
	})
	if err != nil {
		return errResp(req.ID, protocol.InternalError, fmt.Sprintf("orchestrator list_prompts failed: %v", err))
	}

	lp := resp.GetListPrompts()
	if lp == nil {
		return errResp(req.ID, protocol.InternalError, "unexpected response type from orchestrator")
	}

	mcpPrompts := make([]protocol.MCPPromptDefinition, 0, len(lp.Prompts))
	for _, pd := range lp.Prompts {
		mcpPrompts = append(mcpPrompts, PromptDefinitionToMCP(pd))
	}

	return &protocol.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  promptsListResult{Prompts: mcpPrompts},
	}
}

type promptGetParams struct {
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments,omitempty"`
}

func (g *Gateway) handlePromptsGet(ctx context.Context, req *protocol.JSONRPCRequest) *protocol.JSONRPCResponse {
	var params promptGetParams
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errResp(req.ID, protocol.InvalidParams, fmt.Sprintf("invalid params: %v", err))
		}
	}

	if params.Name == "" {
		return errResp(req.ID, protocol.InvalidParams, "missing required parameter: name")
	}

	resp, err := g.sender.Send(ctx, &pluginv1.PluginRequest{
		RequestId: fmt.Sprintf("web-pg-%v", req.ID),
		Request: &pluginv1.PluginRequest_PromptGet{
			PromptGet: &pluginv1.PromptGetRequest{
				PromptName: params.Name,
				Arguments:  params.Arguments,
			},
		},
	})
	if err != nil {
		return errResp(req.ID, protocol.InternalError, fmt.Sprintf("orchestrator prompt_get failed: %v", err))
	}

	pg := resp.GetPromptGet()
	if pg == nil {
		return errResp(req.ID, protocol.InternalError, "unexpected response type from orchestrator")
	}

	return &protocol.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  PromptGetResponseToMCP(pg),
	}
}

// errResp is a convenience helper for building JSON-RPC error responses.
func errResp(id any, code int, message string) *protocol.JSONRPCResponse {
	return &protocol.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &protocol.JSONRPCError{
			Code:    code,
			Message: message,
		},
	}
}
