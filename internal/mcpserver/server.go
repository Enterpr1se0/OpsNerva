package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/ids"
	"github.com/Enterpr1se0/opsnerva/internal/service"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Server struct {
	server         *mcp.Server
	service        *service.Service
	stdioSessionID string
}

func New(svc *service.Service, version string) *Server {
	instance := &Server{service: svc, stdioSessionID: ids.New("mcp_sess")}
	server := mcp.NewServer(&mcp.Implementation{Name: "opsnerva", Version: version}, &mcp.ServerOptions{
		GetSessionID: func() string { return ids.New("mcp_sess") },
	})
	instance.server = server
	server.AddReceivingMiddleware(instance.trackToolCalls)
	registerTools(server, svc)
	return instance
}

func (s *Server) trackToolCalls(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		callRequest, ok := request.(*mcp.CallToolRequest)
		if !ok || method != "tools/call" || s.service == nil || callRequest.Params == nil {
			return next(ctx, method, request)
		}
		sessionID := s.stdioSessionID
		transport := "stdio"
		if request.GetSession() != nil && strings.TrimSpace(request.GetSession().ID()) != "" {
			sessionID = strings.TrimSpace(request.GetSession().ID())
		}
		if callRequest.Extra != nil && callRequest.Extra.Header != nil {
			transport = "streamable_http"
		}
		session := domain.MCPClientSession{ID: sessionID, Transport: transport, ProtocolVersion: callRequest.ProtocolVersion()}
		if info := callRequest.ClientInfo(); info != nil {
			session.ClientName, session.ClientVersion = info.Name, info.Version
		}
		callCtx, call, err := s.service.BeginMCPToolCall(ctx, session, callRequest.Params.Name, string(callRequest.Params.Arguments))
		if err != nil {
			return nil, err
		}
		result, callErr := next(callCtx, method, request)
		call = mcpCallOutcome(call, result, callErr)
		finishErr := s.service.FinishMCPToolCall(ctx, call)
		if finishErr != nil {
			return nil, errors.Join(callErr, finishErr)
		}
		return result, callErr
	}
}

func mcpCallOutcome(call domain.MCPToolCall, result mcp.Result, callErr error) domain.MCPToolCall {
	call.Status = domain.MCPCallCompleted
	if callErr != nil {
		call.Status = domain.MCPCallFailed
		call.Error = callErr.Error()
		if errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded) {
			call.Status = domain.MCPCallInterrupted
		}
	}
	toolResult, ok := result.(*mcp.CallToolResult)
	if !ok || toolResult == nil {
		return call
	}
	if toolResult.IsError {
		call.Status = domain.MCPCallFailed
		for _, content := range toolResult.Content {
			if textContent, ok := content.(*mcp.TextContent); ok && strings.TrimSpace(textContent.Text) != "" {
				call.Error = textContent.Text
				break
			}
		}
	}
	encoded, err := json.Marshal(toolResult.StructuredContent)
	if err != nil {
		return call
	}
	var metadata struct {
		ID         string `json:"id"`
		Status     string `json:"status"`
		RunID      string `json:"run_id"`
		ApprovalID string `json:"approval_id"`
		TaskID     string `json:"task_id"`
		ShellID    string `json:"shell_id"`
		TunnelID   string `json:"tunnel_id"`
		Shell      *struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"shell"`
		Tunnel *struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"tunnel"`
	}
	if json.Unmarshal(encoded, &metadata) != nil {
		return call
	}
	call.RunID, call.ApprovalID, call.TaskID = metadata.RunID, metadata.ApprovalID, metadata.TaskID
	call.ShellID, call.TunnelID, call.OperationStatus = metadata.ShellID, metadata.TunnelID, metadata.Status
	if metadata.Shell != nil {
		call.ShellID = metadata.Shell.ID
		if call.OperationStatus == "" {
			call.OperationStatus = metadata.Shell.Status
		}
	}
	if metadata.Tunnel != nil {
		call.TunnelID = metadata.Tunnel.ID
		if call.OperationStatus == "" {
			call.OperationStatus = metadata.Tunnel.Status
		}
	}
	if call.ToolName == "ssh_shell" && call.ShellID == "" {
		call.ShellID = metadata.ID
	}
	if call.ToolName == "ssh_tunnel" && call.TunnelID == "" {
		call.TunnelID = metadata.ID
	}
	return call
}

func (s *Server) Run(ctx context.Context) error {
	return s.server.Run(ctx, &mcp.StdioTransport{})
}

func (s *Server) HTTPHandler() http.Handler {
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.server },
		&mcp.StreamableHTTPOptions{
			Stateless:                    false,
			JSONResponse:                 true,
			MaxRequestBodyBytes:          mcp.DefaultMaxRequestBodyBytes,
			PropagateRequestCancellation: true,
		},
	)
}
