package mcpserver

import (
	"context"
	"net/http"

	"eino-ops-agent/internal/agent"
	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/service"
	"eino-ops-agent/internal/sshx"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Server struct {
	server *mcp.Server
}

func New(svc *service.Service, version string) *Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "opsnerva", Version: version}, nil)

	mcp.AddTool(server, &mcp.Tool{Name: "ssh_host_inspect", Description: "Read-only inspection of a registered SSH host.", Annotations: readOnlyAnnotations("Inspect SSH host")},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.HostInput) (*mcp.CallToolResult, sshx.HostInfo, error) {
			output, err := svc.ProbeHost(ctx, input.HostID, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_host_list", Description: "List registered host IDs, names, authentication types, and managed sudo modes without connection details or credentials.", Annotations: readOnlyAnnotations("List SSH hosts")},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, agent.HostListOutput, error) {
			hosts, err := svc.ListHostCapabilities(ctx)
			return nil, agent.HostListOutput{Hosts: hosts}, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_exec", Description: "Execute one remote program with separate arguments under the configured approval mode and audit controls. Set background=true for cancellable background execution; it defaults to false.", Annotations: changeAnnotations("Execute SSH program", true)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.ExecInput) (*mcp.CallToolResult, agent.ExecToolResult, error) {
			output, err := agent.RunExecutionTool(ctx, svc, execRequest(input), "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_run_script", Description: "Analyze and run a Bash script under the configured approval mode. Set background=true for cancellable background execution; it defaults to false.", Annotations: changeAnnotations("Run SSH script", true)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.ScriptInput) (*mcp.CallToolResult, agent.ExecToolResult, error) {
			output, err := agent.RunExecutionTool(ctx, svc, scriptRequest(input), "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_task", Description: "Read or cancel one background SSH task with action=status or action=cancel.", Annotations: changeAnnotations("Manage SSH task", false)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.TaskInput) (*mcp.CallToolResult, agent.ExecToolResult, error) {
			output, err := agent.RunTaskTool(ctx, svc, input, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_file_read", Description: "Read one remote file or search it under the configured approval mode. Content reads default to a 131072-byte page; follow file.next_offset while file.has_more, or set full_content=true only for a reasonably sized file. Search requires pattern plus match_mode=literal|regex, returns every matching line, and reports no matches as search.found=false.", InputSchema: fileSearchInputSchema[agent.FileReadInput](), Annotations: readOnlyAnnotations("Read SSH file")},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.FileReadInput) (*mcp.CallToolResult, agent.ExecToolResult, error) {
			output, err := agent.RunFileReadTool(ctx, svc, input, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_file_list", Description: "List a remote directory without changing it.", Annotations: readOnlyAnnotations("List SSH files")},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.FileListInput) (*mcp.CallToolResult, agent.ExecToolResult, error) {
			output, err := svc.ListFiles(ctx, input.HostID, input.Path, "mcp-client")
			compact, err := agent.CompactExecToolResult(output, err)
			return nil, compact, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_file_edit", Description: "Apply one reviewed unified diff to an existing remote file. validator_id accepts only a configured ID, never a command line; omit it unless an exact ID is known.", Annotations: changeAnnotations("Edit SSH file", true)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.FileEditInput) (*mcp.CallToolResult, agent.ExecToolResult, error) {
			output, err := svc.EditRemoteFile(ctx, input.HostID, input.Path, input.Diff, input.ValidatorID, input.Elevated, input.Reason, "mcp-client")
			compact, err := agent.CompactExecToolResult(output, err)
			return nil, compact, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_file_transfer", Description: "Transfer one SHA256-bound regular file between registered SSH hosts. Omit expected_destination_sha256 to require a new destination; provide it to replace that exact version.", Annotations: changeAnnotations("Transfer SSH file", true)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.SSHFileTransferInput) (*mcp.CallToolResult, agent.ExecToolResult, error) {
			output, err := svc.TransferFileBetweenHosts(ctx, input.SourceHostID, input.SourcePath, input.ExpectedSHA256, input.DestinationHostID, input.DestinationPath, input.ExpectedDestinationSHA256, input.TimeoutSeconds, input.Reason, "mcp-client")
			compact, err := agent.CompactExecToolResult(output, err)
			return nil, compact, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_tunnel", Description: "Start, list, or stop local SSH port forwarding through registered hosts. Configured proxies and jump hosts are reused automatically.", Annotations: changeAnnotations("Manage SSH tunnel", false)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.SSHTunnelInput) (*mcp.CallToolResult, any, error) {
			output, err := agent.RunSSHTunnelTool(ctx, svc, input, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_shell", Description: "Start, input, read output, list, interrupt, or close an approved interactive SSH PTY. The server automatically collects input responses and later output batches. Continuous programs return a batch while the PTY keeps running. Never send credentials through input.", Annotations: changeAnnotations("Manage SSH shell", true)},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.SSHShellInput) (*mcp.CallToolResult, any, error) {
			ctx = service.WithMCPClientSession(ctx)
			output, err := agent.RunSSHShellTool(ctx, svc, input, "mcp-client")
			return nil, output, err
		})
	return &Server{server: server}
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

func fileSearchInputSchema[T any]() *jsonschema.Schema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(err)
	}
	matchMode, ok := schema.Properties["match_mode"]
	if !ok {
		panic("file search input schema is missing match_mode")
	}
	matchMode.Enum = []any{string(domain.FileSearchLiteral), string(domain.FileSearchRegex)}
	return schema
}

func (s *Server) Run(ctx context.Context) error {
	return s.server.Run(ctx, &mcp.StdioTransport{})
}

func (s *Server) HTTPHandler() http.Handler {
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.server },
		&mcp.StreamableHTTPOptions{
			Stateless:                    true,
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
