package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/Enterpr1se0/opsnerva/internal/agenttool"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/toolresult"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func registerTools(server *mcp.Server, svc *service.Service) {
	sshTools := agenttool.NewSSH(agenttool.SSHDependencies{
		Execution: svc,
		Tasks:     svc,
		Files:     svc,
		Tunnels:   svc,
		Shells:    svc,
		Results:   toolresult.Policy{},
	})
	historyTools := agenttool.NewHistory(svc)

	mcp.AddTool(server, &mcp.Tool{Name: "ssh_host_inspect", Description: "Inspect one SSH host's OS, user, shell, and uptime.", InputSchema: inputSchema[agenttool.HostInput](), Annotations: readOnlyAnnotations("Inspect SSH host")},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agenttool.HostInput) (*mcp.CallToolResult, sshx.HostInfo, error) {
			output, err := svc.ProbeHost(ctx, input.HostID, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_host_list", Description: "List Agent-enabled SSH host IDs, root access, and detected shells.", InputSchema: inputSchema[struct{}](), Annotations: readOnlyAnnotations("List SSH hosts")},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, agenttool.HostListOutput, error) {
			hosts, err := svc.ListHostCapabilities(ctx)
			return nil, agenttool.HostListOutput{Hosts: hosts}, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_exec", Description: agenttool.SSHExecDescription, InputSchema: inputSchema[agenttool.ExecInput](), Annotations: changeAnnotations("Execute SSH program", true)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agenttool.ExecInput) (*mcp.CallToolResult, agenttool.ExecResult, error) {
			output, err := sshTools.RunExecution(ctx, agenttool.ExecutionRequest(input), "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_run_script", Description: agenttool.SSHScriptDescription, InputSchema: inputSchema[agenttool.ScriptInput](), Annotations: changeAnnotations("Run SSH script", true)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agenttool.ScriptInput) (*mcp.CallToolResult, agenttool.ExecResult, error) {
			output, err := sshTools.RunExecution(ctx, agenttool.ScriptRequest(input), "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_task", Description: agenttool.SSHTaskDescription, InputSchema: inputSchema[agenttool.TaskInput](), Annotations: changeAnnotations("Manage SSH task", false)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agenttool.TaskInput) (*mcp.CallToolResult, agenttool.ExecResult, error) {
			output, err := sshTools.RunTask(ctx, input, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_file_read", Description: "Read, page, tail, inspect metadata, or search one remote file.", InputSchema: inputSchema[agenttool.FileReadInput](), Annotations: readOnlyAnnotations("Read SSH file")},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agenttool.FileReadInput) (*mcp.CallToolResult, agenttool.ExecResult, error) {
			output, err := sshTools.RunFileRead(ctx, input, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_file_list", Description: "List a remote directory.", InputSchema: inputSchema[agenttool.FileListInput](), Annotations: readOnlyAnnotations("List SSH files")},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agenttool.FileListInput) (*mcp.CallToolResult, agenttool.ExecResult, error) {
			output, err := sshTools.RunFileList(ctx, input, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_file_edit", Description: "Create a remote text file or replace/delete one exact unique line block; read existing files first.", InputSchema: inputSchema[agenttool.FileEditInput](), Annotations: changeAnnotations("Edit SSH file", true)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agenttool.FileEditInput) (*mcp.CallToolResult, agenttool.ExecResult, error) {
			output, err := sshTools.RunFileEdit(ctx, input, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_file_transfer", Description: "Transfer one SHA256-bound file between SSH hosts.", InputSchema: inputSchema[agenttool.SSHFileTransferInput](), Annotations: changeAnnotations("Transfer SSH file", true)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agenttool.SSHFileTransferInput) (*mcp.CallToolResult, agenttool.ExecResult, error) {
			output, err := sshTools.RunFileTransfer(ctx, input, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_tunnel", Description: agenttool.SSHTunnelDescription, InputSchema: inputSchema[agenttool.SSHTunnelInput](), Annotations: changeAnnotations("Manage SSH tunnel", false)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agenttool.SSHTunnelInput) (*mcp.CallToolResult, any, error) {
			output, err := sshTools.RunTunnel(ctx, input, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_shell", Description: agenttool.SSHShellDescription, InputSchema: inputSchema[agenttool.SSHShellInput](), Annotations: changeAnnotations("Manage SSH shell", true)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agenttool.SSHShellInput) (*mcp.CallToolResult, any, error) {
			output, err := sshTools.RunShell(ctx, service.SessionIDFromContext(ctx), input, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_history", Description: "Search this MCP session's audited SSH runs with bounded redacted output and cursor pagination.", InputSchema: inputSchema[agenttool.HistorySearchInput](), Annotations: readOnlyAnnotations("Search SSH history")},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agenttool.HistorySearchInput) (*mcp.CallToolResult, agenttool.HistoryOutput, error) {
			output, err := historyTools.Read(ctx, input)
			return nil, output, err
		})
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
	encoded, err := agenttool.InputSchemaJSON[T]()
	if err != nil {
		panic(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(encoded, &schema); err != nil {
		panic(err)
	}
	return &schema
}
