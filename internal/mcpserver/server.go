package mcpserver

import (
	"context"

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
	server := mcp.NewServer(&mcp.Implementation{Name: "eino-ops-agent", Version: version}, nil)

	mcp.AddTool(server, &mcp.Tool{Name: "ssh_host_inspect", Description: "Read-only inspection of a registered SSH host."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.HostInput) (*mcp.CallToolResult, sshx.HostInfo, error) {
			output, err := svc.ProbeHost(ctx, input.HostID)
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_host_list", Description: "List registered host IDs, names, authentication types, and managed sudo modes without connection details or credentials."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, agent.HostListOutput, error) {
			hosts, err := svc.ListHostCapabilities(ctx)
			return nil, agent.HostListOutput{Hosts: hosts}, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_exec", Description: "Execute one remote program with separate arguments through deterministic policy, approval, and audit controls. Set background=true for cancellable background execution; it defaults to false."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.ExecInput) (*mcp.CallToolResult, domain.ExecResult, error) {
			output, err := agent.RunExecutionTool(ctx, svc, execRequest(input), input.Background, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_run_script", Description: "Analyze and run a Bash script. Set background=true for cancellable background execution; it defaults to false. State changes require human approval."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.ScriptInput) (*mcp.CallToolResult, domain.ExecResult, error) {
			output, err := agent.RunExecutionTool(ctx, svc, scriptRequest(input), input.Background, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_task", Description: "Read or cancel one background SSH task with action=status or action=cancel."},
		func(_ context.Context, _ *mcp.CallToolRequest, input agent.TaskInput) (*mcp.CallToolResult, domain.ExecResult, error) {
			output, err := agent.RunTaskTool(svc, input, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_file_read", Description: "Read one remote file or search it after one-time human approval. Search requires pattern plus match_mode=literal|regex, returns every matching line, and reports no matches as search.found=false. Credential paths are denied.", InputSchema: fileSearchInputSchema[agent.FileReadInput]()},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.FileReadInput) (*mcp.CallToolResult, domain.ExecResult, error) {
			output, err := agent.RunFileReadTool(ctx, svc, input, "mcp-client")
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_file_list", Description: "List a remote directory without changing it."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.FileListInput) (*mcp.CallToolResult, domain.ExecResult, error) {
			output, err := svc.ListFiles(ctx, input.HostID, input.Path, "mcp-client")
			output, err = agent.NormalizeExecToolResult(output, err)
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_file_edit", Description: "Apply one reviewed unified diff to an existing remote file."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.FileEditInput) (*mcp.CallToolResult, domain.ExecResult, error) {
			output, err := svc.EditRemoteFile(ctx, input.HostID, input.Path, input.Diff, input.Validator, input.Elevated, input.Reason, "mcp-client")
			output, err = agent.NormalizeExecToolResult(output, err)
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_file_transfer", Description: "Transfer one SHA256-bound regular file between two registered SSH hosts through the control plane after human approval."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.SSHFileTransferInput) (*mcp.CallToolResult, domain.ExecResult, error) {
			output, err := svc.TransferFileBetweenHosts(ctx, input.SourceHostID, input.SourcePath, input.ExpectedSHA256, input.DestinationHostID, input.DestinationPath, input.Overwrite, input.ExpectedDestinationSHA256, input.TimeoutSeconds, input.Reason, "mcp-client")
			output, err = agent.NormalizeExecToolResult(output, err)
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ssh_history", Description: "Search audited commands and redacted outputs by literal substring, or get one exact run by run_id. Secrets are stored as [REDACTED]; prefer short distinctive keywords over full command lines."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.HistorySearchInput) (*mcp.CallToolResult, agent.HistoryOutput, error) {
			output, err := agent.ReadHistoryTool(ctx, svc, input)
			return nil, output, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: "ops_skill", Description: "List enabled operational skills, or load one complete skill by name."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agent.SkillInput) (*mcp.CallToolResult, agent.SkillOutput, error) {
			output, err := agent.ReadSkillTool(ctx, svc, input, "mcp-client")
			return nil, output, err
		})
	return &Server{server: server}
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

func execRequest(input agent.ExecInput) domain.ExecRequest {
	return domain.ExecRequest{HostID: input.HostID, Mode: domain.ExecProgram, Program: input.Program, Args: input.Args, Cwd: input.Cwd, Env: input.Env, Elevated: input.Elevated, TimeoutSeconds: input.TimeoutSeconds, Reason: input.Reason}
}

func scriptRequest(input agent.ScriptInput) domain.ExecRequest {
	return domain.ExecRequest{HostID: input.HostID, Mode: domain.ExecScript, Script: input.Script, Cwd: input.Cwd, Env: input.Env, Elevated: input.Elevated, TimeoutSeconds: input.TimeoutSeconds, Reason: input.Reason}
}
