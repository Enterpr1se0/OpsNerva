package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/agent"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/ids"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"

	"github.com/google/jsonschema-go/jsonschema"
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

	mcp.AddTool(server, &mcp.Tool{Name: "ssh_host_inspect", Description: "Inspect one SSH host's OS, user, and uptime.", InputSchema: inputSchema[agent.HostInput](), Annotations: readOnlyAnnotations("Inspect SSH host")},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.HostInput) (*mcp.CallToolResult, sshx.HostInfo, error) {
			output, err := svc.ProbeHost(ctx, input.HostID, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_host_list", Description: "List SSH host IDs and capabilities; excludes connection data and secrets.", InputSchema: inputSchema[struct{}](), Annotations: readOnlyAnnotations("List SSH hosts")},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, agent.HostListOutput, error) {
			hosts, err := svc.ListHostCapabilities(ctx)
			return nil, agent.HostListOutput{Hosts: hosts}, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_exec", Description: agent.SSHExecToolDescription, InputSchema: inputSchema[agent.ExecInput](), Annotations: changeAnnotations("Execute SSH program", true)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.ExecInput) (*mcp.CallToolResult, agent.ExecToolResult, error) {
			output, err := agent.RunExecutionTool(ctx, svc, execRequest(input), "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_run_script", Description: agent.SSHScriptToolDescription, InputSchema: inputSchema[agent.ScriptInput](), Annotations: changeAnnotations("Run SSH script", true)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.ScriptInput) (*mcp.CallToolResult, agent.ExecToolResult, error) {
			output, err := agent.RunExecutionTool(ctx, svc, scriptRequest(input), "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_task", Description: agent.SSHTaskToolDescription, InputSchema: inputSchema[agent.TaskInput](), Annotations: changeAnnotations("Manage SSH task", false)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.TaskInput) (*mcp.CallToolResult, agent.ExecToolResult, error) {
			output, err := agent.RunTaskTool(ctx, svc, input, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_file_read", Description: "Read, page, tail, inspect metadata, or search one remote file.", InputSchema: inputSchema[agent.FileReadInput](), Annotations: readOnlyAnnotations("Read SSH file")},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.FileReadInput) (*mcp.CallToolResult, agent.ExecToolResult, error) {
			output, err := agent.RunFileReadTool(ctx, svc, input, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_file_list", Description: "List a remote directory.", InputSchema: inputSchema[agent.FileListInput](), Annotations: readOnlyAnnotations("List SSH files")},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.FileListInput) (*mcp.CallToolResult, agent.ExecToolResult, error) {
			output, err := svc.ListFiles(ctx, input.HostID, input.Path, "mcp-client")
			compact, err := agent.CompactExecToolResult(output, err)
			return nil, compact, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_file_edit", Description: "Create a remote text file or replace/delete one exact unique line block; read existing files first.", InputSchema: inputSchema[agent.FileEditInput](), Annotations: changeAnnotations("Edit SSH file", true)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.FileEditInput) (*mcp.CallToolResult, agent.ExecToolResult, error) {
			output, err := svc.EditRemoteFile(ctx, input.HostID, input.Path, input.OldText, input.NewText, input.ValidatorID, input.Elevated, input.Reason, "mcp-client")
			compact, err := agent.CompactExecToolResult(output, err)
			return nil, compact, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_file_transfer", Description: "Transfer one SHA256-bound file between SSH hosts.", InputSchema: inputSchema[agent.SSHFileTransferInput](), Annotations: changeAnnotations("Transfer SSH file", true)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.SSHFileTransferInput) (*mcp.CallToolResult, agent.ExecToolResult, error) {
			output, err := svc.TransferFileBetweenHosts(ctx, input.SourceHostID, input.SourcePath, input.ExpectedSHA256, input.DestinationHostID, input.DestinationPath, input.ExpectedDestinationSHA256, input.TimeoutSeconds, input.Reason, "mcp-client")
			compact, err := agent.CompactExecToolResult(output, err)
			return nil, compact, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_tunnel", Description: agent.SSHTunnelToolDescription, InputSchema: inputSchema[agent.SSHTunnelInput](), Annotations: changeAnnotations("Manage SSH tunnel", false)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.SSHTunnelInput) (*mcp.CallToolResult, any, error) {
			output, err := agent.RunSSHTunnelTool(ctx, svc, input, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_shell", Description: agent.SSHShellToolDescription, InputSchema: inputSchema[agent.SSHShellInput](), Annotations: changeAnnotations("Manage SSH shell", true)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.SSHShellInput) (*mcp.CallToolResult, any, error) {
			output, err := agent.RunSSHShellTool(ctx, svc, input, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_history", Description: "Search this MCP session's audited SSH runs with bounded redacted output and cursor pagination.", InputSchema: inputSchema[agent.HistorySearchInput](), Annotations: readOnlyAnnotations("Search SSH history")},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.HistorySearchInput) (*mcp.CallToolResult, agent.HistoryOutput, error) {
			output, err := agent.ReadHistoryTool(ctx, svc, input)
			return nil, output, err
		})
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

func readOnlyAnnotations(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		Title: title, ReadOnlyHint: true, IdempotentHint: true,
		DestructiveHint: boolHint(false), OpenWorldHint: boolHint(true),
	}
}

func changeAnnotations(title string, destructive bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		Title: title, DestructiveHint: boolHint(destructive), OpenWorldHint: boolHint(true),
	}
}

func boolHint(value bool) *bool {
	return &value
}

func inputSchema[T any]() *jsonschema.Schema {
	encoded, err := agent.InputSchemaJSON[T]()
	if err != nil {
		panic(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(encoded, &schema); err != nil {
		panic(err)
	}
	return &schema
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

func execRequest(input agent.ExecInput) domain.ExecRequest {
	return domain.ExecRequest{HostID: input.HostID, Mode: domain.ExecProgram, Program: input.Program, Args: input.Args, Background: input.Background, Cwd: input.Cwd, Env: input.Env, Elevated: input.Elevated, TimeoutSeconds: input.TimeoutSeconds, MaxOutputBytes: input.MaxOutputBytes, OutputView: input.OutputView, Reason: input.Reason}
}

func scriptRequest(input agent.ScriptInput) domain.ExecRequest {
	return domain.ExecRequest{HostID: input.HostID, Mode: domain.ExecScript, Script: input.Script, Background: input.Background, Cwd: input.Cwd, Env: input.Env, Elevated: input.Elevated, TimeoutSeconds: input.TimeoutSeconds, MaxOutputBytes: input.MaxOutputBytes, OutputView: input.OutputView, Reason: input.Reason}
}
