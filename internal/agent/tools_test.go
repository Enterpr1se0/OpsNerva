package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"eino-ops-agent/internal/config"
	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/policy"
	"eino-ops-agent/internal/security"
	"eino-ops-agent/internal/service"
	"eino-ops-agent/internal/sshx"
	"eino-ops-agent/internal/store"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

type backgroundToolTransport struct {
	started chan domain.ExecRequest
	release chan struct{}
}

type fileReadToolTransport struct {
	request   domain.ExecRequest
	callCount int
}

type toolFailureLoopModel struct {
	calls  int
	inputs [][]*schema.Message
}

func (m *toolFailureLoopModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.calls++
	m.inputs = append(m.inputs, append([]*schema.Message(nil), input...))
	if m.calls == 1 {
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-invalid", Function: schema.FunctionCall{Name: "raw_failure", Arguments: `{"value":"x"}`},
		}}), nil
	}
	return schema.AssistantMessage("handled the tool failure", nil), nil
}

func (m *toolFailureLoopModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func (m *toolFailureLoopModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (t *backgroundToolTransport) Exec(ctx context.Context, _ sshx.ConnectionSpec, request domain.ExecRequest) (sshx.RawResult, error) {
	t.started <- request
	select {
	case <-t.release:
		return sshx.RawResult{Stdout: []byte("background complete\n")}, nil
	case <-ctx.Done():
		return sshx.RawResult{}, ctx.Err()
	}
}

func (t *fileReadToolTransport) Exec(_ context.Context, _ sshx.ConnectionSpec, request domain.ExecRequest) (sshx.RawResult, error) {
	t.request = request
	t.callCount++
	if strings.HasPrefix(request.Script, "grep -n ") {
		return sshx.RawResult{Stdout: []byte("2:secret contents\n")}, nil
	}
	stdout := "__OPS_FILE_META__\n15\t640\tops\tops\t1700000000\n" + strings.Repeat("a", 64) + "  /etc/example.conf\n__OPS_FILE_CONTENT__\nsecret contents"
	return sshx.RawResult{Stdout: []byte(stdout)}, nil
}

func (*fileReadToolTransport) Probe(context.Context, sshx.ConnectionSpec) (sshx.HostInfo, error) {
	return sshx.HostInfo{}, nil
}

func (*fileReadToolTransport) ScanHostKey(context.Context, sshx.ConnectionSpec) (sshx.HostKey, error) {
	return sshx.HostKey{}, nil
}

func (*fileReadToolTransport) TrustHostKey(context.Context, sshx.ConnectionSpec, string) (sshx.HostKey, error) {
	return sshx.HostKey{}, nil
}

func (*fileReadToolTransport) StoredHostKey(domain.Host) (sshx.HostKey, bool) {
	return sshx.HostKey{}, false
}

func (*backgroundToolTransport) Probe(context.Context, sshx.ConnectionSpec) (sshx.HostInfo, error) {
	return sshx.HostInfo{}, nil
}

func (*backgroundToolTransport) ScanHostKey(context.Context, sshx.ConnectionSpec) (sshx.HostKey, error) {
	return sshx.HostKey{}, nil
}

func (*backgroundToolTransport) TrustHostKey(context.Context, sshx.ConnectionSpec, string) (sshx.HostKey, error) {
	return sshx.HostKey{}, nil
}

func (*backgroundToolTransport) StoredHostKey(domain.Host) (sshx.HostKey, bool) {
	return sshx.HostKey{}, false
}

func TestToolDescriptorsMatchTheEinoSchemasLoadedByTheAgent(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	engine, err := policy.Load("")
	if err != nil {
		t.Fatal(err)
	}
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, engine, nil, encryptor, security.NewRedactor(), config.Default().Limits)
	loaded, err := BuildTools(svc)
	if err != nil {
		t.Fatal(err)
	}
	descriptors, err := DescribeTools(ctx, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if len(descriptors) != len(loaded) || len(descriptors) < 20 {
		t.Fatalf("catalog=%d loaded=%d", len(descriptors), len(loaded))
	}
	if len(descriptors) != 22 {
		t.Fatalf("built-in catalog size=%d, want 22", len(descriptors))
	}

	seen := make(map[string]bool, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor.Name == "" || descriptor.Description == "" || descriptor.Category == "" || descriptor.Guard == "" {
			t.Fatalf("incomplete descriptor: %#v", descriptor)
		}
		if seen[descriptor.Name] {
			t.Fatalf("duplicate function %q", descriptor.Name)
		}
		seen[descriptor.Name] = true
		if !json.Valid(descriptor.InputSchema) {
			t.Fatalf("invalid schema for %s: %s", descriptor.Name, descriptor.InputSchema)
		}
		schemaText := string(descriptor.InputSchema)
		if strings.Contains(schemaText, `"expected_changes"`) || strings.Contains(schemaText, `"rollback"`) {
			t.Fatalf("%s still exposes retired verbose fields: %s", descriptor.Name, descriptor.InputSchema)
		}
		if descriptor.Name == "ssh_exec" {
			if descriptor.Guard != "policy_checked" || !strings.Contains(schemaText, `"host_id"`) || !strings.Contains(schemaText, `"program"`) ||
				!strings.Contains(schemaText, `"args"`) || !strings.Contains(schemaText, `"background"`) || !strings.Contains(schemaText, `"elevated"`) {
				t.Fatalf("ssh_exec metadata does not reflect its runtime schema: %#v", descriptor)
			}
			var schema struct {
				Required []string `json:"required"`
			}
			if err := json.Unmarshal(descriptor.InputSchema, &schema); err != nil {
				t.Fatal(err)
			}
			for _, required := range schema.Required {
				if required == "background" {
					t.Fatal("ssh_exec background must remain optional and default to false")
				}
			}
		}
		if descriptor.Name == "ssh_run_script" && !strings.Contains(string(descriptor.InputSchema), `"background"`) {
			t.Fatalf("ssh_run_script metadata is missing background: %#v", descriptor)
		}
		if descriptor.Name == "ssh_file_read" {
			schema := string(descriptor.InputSchema)
			if descriptor.Guard != "approval_required" || !strings.Contains(schema, `"metadata_only"`) || !strings.Contains(schema, `"pattern"`) || !strings.Contains(schema, `"match_mode"`) || !strings.Contains(schema, `"literal"`) || !strings.Contains(schema, `"regex"`) || !strings.Contains(schema, `"context_lines"`) || strings.Contains(schema, `"max_matches"`) {
				t.Fatalf("ssh_file_read merged modes are incomplete: %#v", descriptor)
			}
		}
		if descriptor.Name == "workspace_file_read" {
			schema := string(descriptor.InputSchema)
			if descriptor.Guard != "approval_required" || strings.Contains(schema, `"workspace_id"`) || !strings.Contains(schema, `"pattern"`) || !strings.Contains(schema, `"match_mode"`) || !strings.Contains(schema, `"literal"`) || !strings.Contains(schema, `"regex"`) || !strings.Contains(schema, `"context_lines"`) || strings.Contains(schema, `"max_matches"`) {
				t.Fatalf("workspace_file_read merged modes are incomplete: %#v", descriptor)
			}
		}
		if strings.HasPrefix(descriptor.Name, "workspace_") && strings.Contains(string(descriptor.InputSchema), `"workspace_id"`) {
			t.Fatalf("%s still lets the model select a Workspace: %s", descriptor.Name, descriptor.InputSchema)
		}
		if descriptor.Name == "ssh_file_edit" {
			schema := string(descriptor.InputSchema)
			if !strings.Contains(schema, `"diff"`) || strings.Contains(schema, `"expected_sha256"`) || strings.Contains(schema, `"content"`) {
				t.Fatalf("ssh_file_edit still exposes the retired edit contract: %s", schema)
			}
		}
		if descriptor.Name == "workspace_file_edit" {
			schema := string(descriptor.InputSchema)
			if !strings.Contains(schema, `"diff"`) || strings.Contains(schema, `"expected_sha256"`) || strings.Contains(schema, `"content"`) {
				t.Fatalf("workspace_file_edit still exposes the retired edit contract: %s", schema)
			}
		}
		if descriptor.Name == "ssh_task" && (descriptor.Guard != "audited_control" || !strings.Contains(string(descriptor.InputSchema), `"action"`)) {
			t.Fatalf("ssh_task metadata does not expose its audited action: %#v", descriptor)
		}
		if descriptor.Name == "ssh_task" && descriptor.Category != "tasks" {
			t.Fatalf("ssh_task category = %q, want tasks", descriptor.Category)
		}
		if descriptor.Name == "ssh_tunnel" {
			schema := string(descriptor.InputSchema)
			if descriptor.Guard != "approval_required" || !strings.Contains(schema, `"action"`) || !strings.Contains(schema, `"remote_host"`) || !strings.Contains(schema, `"remote_port"`) || !strings.Contains(schema, `"local_port"`) || !strings.Contains(schema, `"tunnel_id"`) {
				t.Fatalf("ssh_tunnel metadata does not reflect its runtime schema: %#v", descriptor)
			}
		}
		if descriptor.Name == "ssh_shell" {
			schema := string(descriptor.InputSchema)
			if descriptor.Guard != "approval_required" || !strings.Contains(schema, `"action"`) ||
				!strings.Contains(schema, `"shell_id"`) || !strings.Contains(schema, `"input"`) ||
				!strings.Contains(schema, `"submit"`) || !strings.Contains(schema, `"after_sequence"`) ||
				!strings.Contains(schema, `"coalesce"`) || !strings.Contains(schema, `"reason"`) ||
				strings.Contains(schema, `"ttl_seconds"`) || strings.Contains(schema, `"extend_seconds"`) {
				t.Fatalf("ssh_shell metadata does not reflect its runtime schema: %#v", descriptor)
			}
		}
		if descriptor.Name == "ssh_history" && descriptor.Category != "history" {
			t.Fatalf("ssh_history category = %q, want history", descriptor.Category)
		}
		if descriptor.Name == "ops_skill" && descriptor.Category != "skills" {
			t.Fatalf("ops_skill category = %q, want skills", descriptor.Category)
		}
		if descriptor.Name == "workspace_shell" && descriptor.Guard != "approval_required" {
			t.Fatalf("workspace_shell must be approval-gated: %#v", descriptor)
		}
		if descriptor.Name == "ssh_file_transfer" && (descriptor.Guard != "approval_required" || !strings.Contains(string(descriptor.InputSchema), `"source_host_id"`) || !strings.Contains(string(descriptor.InputSchema), `"destination_host_id"`)) {
			t.Fatalf("ssh_file_transfer metadata does not reflect its runtime schema: %#v", descriptor)
		}
		if descriptor.Name == "web_extract" && (descriptor.Guard != "read_only" || descriptor.Category != "web" || !strings.Contains(string(descriptor.InputSchema), `"urls"`)) {
			t.Fatalf("web_extract metadata does not reflect its runtime schema: %#v", descriptor)
		}
	}
	for _, retired := range []string{"ssh_approval_status", "ssh_task_start", "ssh_task_status", "ssh_task_tail", "ssh_task_list", "ssh_task_get", "ssh_task_cancel", "ssh_file_write", "ssh_file_apply_patch", "ssh_file_restore", "ssh_file_create", "ssh_file_stat", "ssh_config_apply", "ssh_config_restore", "workspace_list", "workspace_file_apply_patch", "workspace_file_create", "ssh_file_search", "workspace_file_search", "ssh_history_search", "ssh_history_get", "ops_skill_list", "ops_skill_get", "ops_plan_get"} {
		if seen[retired] {
			t.Fatalf("removed %s tool remains in the Agent catalog", retired)
		}
	}
	if !seen["ops_plan_create"] || !seen["ops_plan_step_update"] || !seen["ssh_file_edit"] || !seen["ssh_file_transfer"] || !seen["ssh_tunnel"] || !seen["ssh_shell"] || !seen["workspace_file_edit"] || !seen["workspace_file_upload"] || !seen["workspace_shell"] || !seen["web_search"] || !seen["web_extract"] || !seen["ssh_task"] || !seen["ssh_history"] || !seen["ops_skill"] {
		t.Fatalf("representative functions missing: %#v", seen)
	}
}

func TestSSHShellActionValidationReportsExactFields(t *testing.T) {
	input := SSHShellInput{Action: "input", HostID: "host-wrong", ShellID: "shell-1", Input: "whoami", Submit: true}
	err := validateSSHShellActionFields(input, "input",
		[]string{"action", "shell_id", "input", "submit", "reason"},
		map[string]any{"action": "input", "shell_id": "shell_xxx", "input": "whoami", "submit": true},
	)
	var validation *toolInputValidationError
	if !errors.As(err, &validation) || validation.validation == nil {
		t.Fatalf("structured validation error was not returned: %v", err)
	}
	if len(validation.validation.UnexpectedFields) != 1 || validation.validation.UnexpectedFields[0] != "host_id" {
		t.Fatalf("unexpected field details = %#v", validation.validation)
	}

	valid := SSHShellInput{Action: "status", ShellID: "shell-1", Coalesce: true, Reason: "read new output"}
	if err := validateSSHShellActionFields(valid, "status",
		[]string{"action", "shell_id", "after_sequence", "wait_seconds", "coalesce", "reason"},
		nil,
	); err != nil {
		t.Fatalf("valid status fields were rejected: %v", err)
	}
}

func TestSSHTunnelListAllowsReasonWithoutHidingRealInputErrors(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/tunnel-tools.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	engine, err := policy.Load("")
	if err != nil {
		t.Fatal(err)
	}
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, engine, nil, encryptor, security.NewRedactor(), config.Default().Limits)
	loaded, err := BuildTools(svc)
	if err != nil {
		t.Fatal(err)
	}
	var tunnelTool tool.InvokableTool
	for _, candidate := range loaded {
		info, infoErr := candidate.Info(ctx)
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		if info.Name == "ssh_tunnel" {
			tunnelTool = candidate.(tool.InvokableTool)
			break
		}
	}
	if tunnelTool == nil {
		t.Fatal("ssh_tunnel tool was not loaded")
	}

	resultJSON, err := tunnelTool.InvokableRun(ctx, `{"action":" LIST ","reason":"check existing tunnels before starting one"}`)
	if err != nil {
		t.Fatal(err)
	}
	var tunnels domain.SSHTunnelList
	if err := json.Unmarshal([]byte(resultJSON), &tunnels); err != nil {
		t.Fatal(err)
	}
	if tunnels.Count != 0 || tunnels.Tunnels == nil {
		t.Fatalf("unexpected tunnel list: %#v", tunnels)
	}

	failureJSON, err := tunnelTool.InvokableRun(ctx, `{"action":"list","host_id":"host-unexpected"}`)
	if err != nil {
		t.Fatal(err)
	}
	var failure domain.ToolFailure
	if err := json.Unmarshal([]byte(failureJSON), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.OK || failure.Code != "validation_failed" || !strings.Contains(failure.Message, "host_id") {
		t.Fatalf("unexpected invalid list result: %#v", failure)
	}
}

func TestWorkspaceToolUsesConversationBinding(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	workspaceRoot := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "bound-workspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	engine, _ := policy.Load("")
	encryptor, err := security.NewEncryptor("", dataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = dataDir
	svc := service.New(st, engine, nil, encryptor, security.NewRedactor(), cfg.Limits, cfg)
	if err := svc.InitializeWorkspaces(ctx, workspaceRoot); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteAdminWorkspace(ctx, "default", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateAdminWorkspace(ctx, domain.WorkspaceInput{ID: "project", Access: "read_write"}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "project", "README.md"), []byte("bound Workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PrepareChatSession(ctx, "session-bound-workspace", "project", "test"); err != nil {
		t.Fatal(err)
	}
	tools, err := BuildTools(svc)
	if err != nil {
		t.Fatal(err)
	}
	var listTool tool.InvokableTool
	for _, candidate := range tools {
		info, infoErr := candidate.Info(ctx)
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		if info.Name == "workspace_file_list" {
			listTool = candidate.(tool.InvokableTool)
		}
	}
	if listTool == nil {
		t.Fatal("workspace_file_list is missing")
	}
	resultJSON, err := listTool.InvokableRun(service.WithSessionID(ctx, "session-bound-workspace"), `{"path":"."}`)
	if err != nil {
		t.Fatal(err)
	}
	var result domain.ExecResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Status != "completed" || !strings.Contains(result.Stdout, "README.md") {
		t.Fatalf("bound Workspace listing = %#v", result)
	}
}

func TestWebExtractToolResultExposesPartialAndProviderFailures(t *testing.T) {
	partial, err := NormalizeWebExtractToolResult(domain.WebExtractResponse{
		Results:       []domain.WebExtractResult{{URL: "https://example.com", RawContent: "ok"}},
		FailedResults: []domain.WebExtractFailedResult{{URL: "https://example.org", Error: "failed"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !partial.OK || partial.Code != "partial" || len(partial.FailedResults) != 1 || !partial.ContentIsUntrusted {
		t.Fatalf("partial extraction was not exposed to the model: %#v", partial)
	}
	failed, err := NormalizeWebExtractToolResult(domain.WebExtractResponse{
		FailedResults: []domain.WebExtractFailedResult{{URL: "https://example.org", Error: "blocked"}},
	}, service.ErrWebSearchUpstream)
	if err != nil {
		t.Fatal(err)
	}
	if failed.OK || failed.Code != "provider_failed" || !failed.Retryable || len(failed.FailedResults) != 1 {
		t.Fatalf("provider extraction failure was not exposed to the model: %#v", failed)
	}
}

func TestTaskToolResultsExposeRejectionAndStderr(t *testing.T) {
	partial, err := NormalizeExecToolResult(domain.ExecResult{
		RunID: "run_partial", Status: "partial", ExitCode: 2, Stdout: "matched configuration\n",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !partial.OK || partial.Code != "partial" || partial.Retryable || partial.NextAction == "" {
		t.Fatalf("partial execution was not exposed as usable without automatic retry: %#v", partial)
	}

	execResult, err := NormalizeExecToolResult(domain.ExecResult{
		RunID:               "run_exec_rejected",
		Status:              "rejected",
		OperatorInstruction: "inspect logs instead",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if execResult.OK || execResult.Code != "rejected" || execResult.OperatorInstruction == "" || !strings.Contains(execResult.NextAction, "do not resubmit") {
		t.Fatalf("rejected execution was not exposed as an operator interruption: %#v", execResult)
	}

	task := domain.Task{
		ID:                  "task_rejected",
		RunID:               "run_rejected",
		Status:              "rejected",
		OperatorInstruction: "stop the test and only summarize existing evidence",
	}
	status, err := TaskToolResult(task, domain.ExecResult{Status: "rejected", OperatorInstruction: task.OperatorInstruction}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if status.OK || status.TaskID != task.ID || status.OperatorInstruction != task.OperatorInstruction || !strings.Contains(status.NextAction, "do not resubmit") {
		t.Fatalf("task status lost the operator interruption: %#v", status)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"operator_instruction":"stop the test and only summarize existing evidence"`) {
		t.Fatalf("serialized Tool result lost the operator instruction: %s", encoded)
	}

	failed, err := TaskToolResult(
		domain.Task{ID: "task_failed", RunID: "run_failed", Status: "failed"},
		domain.ExecResult{RunID: "run_failed", Status: "failed", ExitCode: 1, Stderr: "sleep: missing operand"},
		"", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(failed)
	if err != nil {
		t.Fatal(err)
	}
	if failed.OK || failed.Code != "failed" || !strings.Contains(string(encoded), `"stderr":"sleep: missing operand"`) || !strings.Contains(failed.NextAction, "do not repeat unchanged input") {
		t.Fatalf("failed task did not expose stderr to the model: output=%#v json=%s", failed, encoded)
	}
}

func TestRunScriptBackgroundReturnsTaskAndUnifiedTaskToolReturnsOutput(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/background-tools.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	engine, _ := policy.Load("")
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	transport := &backgroundToolTransport{started: make(chan domain.ExecRequest, 1), release: make(chan struct{})}
	defer func() {
		select {
		case <-transport.release:
		default:
			close(transport.release)
		}
	}()
	svc := service.New(st, engine, transport, encryptor, security.NewRedactor(), config.Default().Limits)
	host, err := svc.AddHost(ctx, domain.Host{Name: "background-host", Address: "127.0.0.1", Port: 22, User: "ops", AgentEnabled: true}, "test")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := BuildTools(svc)
	if err != nil {
		t.Fatal(err)
	}
	var scriptTool, taskTool tool.InvokableTool
	for _, candidate := range loaded {
		info, infoErr := candidate.Info(ctx)
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		switch info.Name {
		case "ssh_run_script":
			scriptTool = candidate.(tool.InvokableTool)
		case "ssh_task":
			taskTool = candidate.(tool.InvokableTool)
		}
	}
	if scriptTool == nil || taskTool == nil {
		t.Fatal("merged background tools are missing")
	}
	inputJSON, _ := json.Marshal(map[string]any{
		"host_id": host.ID, "script": "printf 'background complete\\n'", "background": true, "reason": "verify background script execution",
	})
	startedJSON, err := scriptTool.InvokableRun(ctx, string(inputJSON))
	if err != nil {
		t.Fatal(err)
	}
	var started domain.ExecResult
	if err := json.Unmarshal([]byte(startedJSON), &started); err != nil {
		t.Fatal(err)
	}
	if !started.OK || started.Status != "running" || started.TaskID == "" {
		t.Fatalf("background script did not return a running task: %#v", started)
	}
	select {
	case request := <-transport.started:
		if request.Mode != domain.ExecScript || request.Script == "" {
			t.Fatalf("background request lost script mode: %#v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("background script did not reach the SSH transport")
	}
	close(transport.release)

	getInput, _ := json.Marshal(map[string]string{"task_id": started.TaskID, "action": "status"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resultJSON, getErr := taskTool.InvokableRun(ctx, string(getInput))
		if getErr != nil {
			t.Fatal(getErr)
		}
		var result domain.ExecResult
		if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
			t.Fatal(err)
		}
		if result.Status == "completed" {
			if !result.OK || result.TaskID != started.TaskID || result.Stdout != "background complete\n" {
				t.Fatalf("unexpected completed task result: %#v", result)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background task did not complete")
}

func TestApprovalGatedBackgroundScriptReturnsBeforeDecision(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	st, err := store.Open(ctx, t.TempDir()+"/background-approval.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	engine, _ := policy.Load("")
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	transport := &backgroundToolTransport{started: make(chan domain.ExecRequest, 1), release: make(chan struct{})}
	defer close(transport.release)
	svc := service.New(st, engine, transport, encryptor, security.NewRedactor(), config.Default().Limits)
	host, err := svc.AddHost(ctx, domain.Host{Name: "approval-background", Address: "127.0.0.1", Port: 22, User: "ops", AgentEnabled: true}, "test")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := BuildTools(svc)
	if err != nil {
		t.Fatal(err)
	}
	var scriptTool tool.InvokableTool
	for _, candidate := range loaded {
		info, infoErr := candidate.Info(ctx)
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		if info.Name == "ssh_run_script" {
			scriptTool = candidate.(tool.InvokableTool)
			break
		}
	}
	if scriptTool == nil {
		t.Fatal("ssh_run_script was not registered")
	}
	notifications := make(chan domain.ExecResult, 1)
	toolCtx := service.WithApprovalNotifier(service.WithBlockingApprovals(service.WithSessionID(ctx, "background-approval")), func(result domain.ExecResult) {
		notifications <- result
	})
	inputJSON, _ := json.Marshal(map[string]any{
		"host_id": host.ID, "script": "sleep 8; echo bg-finished", "background": true,
		"reason": "verify approval-gated background execution",
	})
	startedAt := time.Now()
	resultJSON, err := scriptTool.InvokableRun(toolCtx, string(inputJSON))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(startedAt) > 500*time.Millisecond {
		t.Fatalf("approval-gated background Tool Call blocked for %s", time.Since(startedAt))
	}
	var result domain.ExecResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatal(err)
	}
	if result.TaskID == "" || result.Status != "running" {
		t.Fatalf("background Tool result = %#v", result)
	}
	var pending domain.ExecResult
	select {
	case pending = <-notifications:
	case <-ctx.Done():
		t.Fatal("timed out waiting for background approval")
	}
	if pending.Status != "approval_required" || pending.ApprovalID == "" {
		t.Fatalf("background approval = %#v", pending)
	}
	if err := svc.Reject(context.Background(), pending.ApprovalID, "test complete", "operator"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		task, taskResult, _, err := svc.GetTask(result.TaskID)
		if err == nil && task.Status == "rejected" {
			if taskResult.Status != "rejected" {
				t.Fatalf("rejected background result = %#v", taskResult)
			}
			select {
			case request := <-transport.started:
				t.Fatalf("rejected background request executed: %#v", request)
			default:
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("rejected background task did not reach a terminal state")
}

func TestSSHExecPersistsArgumentsAndBackgroundWithOriginalToolCall(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/exec-history.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	engine, _ := policy.Load("")
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	transport := &backgroundToolTransport{started: make(chan domain.ExecRequest, 1), release: make(chan struct{})}
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(transport.release) }) })
	svc := service.New(st, engine, transport, encryptor, security.NewRedactor(), config.Default().Limits)
	host, err := svc.AddHost(ctx, domain.Host{Name: "exec-history", Address: "127.0.0.1", Port: 22, User: "ops", AgentEnabled: true}, "test")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := BuildTools(svc)
	if err != nil {
		t.Fatal(err)
	}
	var execTool tool.InvokableTool
	for _, candidate := range loaded {
		info, infoErr := candidate.Info(ctx)
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		if info.Name == "ssh_exec" {
			execTool = candidate.(tool.InvokableTool)
			break
		}
	}
	if execTool == nil {
		t.Fatal("ssh_exec was not registered")
	}
	inputJSON, err := json.Marshal(map[string]any{
		"host_id": host.ID, "program": "printf", "args": []string{"%s", "args-ok"}, "background": true,
		"elevated": false, "reason": "verify exact ssh_exec argument persistence",
	})
	if err != nil {
		t.Fatal(err)
	}
	toolCtx := service.WithExecutionOwner(ctx, "call-exec-history", "ssh_exec", string(inputJSON))
	startedJSON, err := execTool.InvokableRun(toolCtx, string(inputJSON))
	if err != nil {
		t.Fatal(err)
	}
	var started domain.ExecResult
	if err := json.Unmarshal([]byte(startedJSON), &started); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-transport.started:
		if request.Program != "printf" || len(request.Args) != 2 || request.Args[0] != "%s" || request.Args[1] != "args-ok" || !request.Background || request.Elevated {
			t.Fatalf("SSH transport received incomplete request: %#v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("background ssh_exec did not reach the SSH transport")
	}
	runs, err := st.SearchRuns(ctx, "", host.ID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("stored runs = %d, want 1", len(runs))
	}
	run := runs[0]
	var storedRequest domain.ExecRequest
	if err := json.Unmarshal([]byte(run.RequestJSON), &storedRequest); err != nil {
		t.Fatal(err)
	}
	if storedRequest.Program != "printf" || len(storedRequest.Args) != 2 || storedRequest.Args[0] != "%s" || storedRequest.Args[1] != "args-ok" || !storedRequest.Background || storedRequest.Elevated {
		t.Fatalf("normalized execution request lost fields: %#v", storedRequest)
	}
	var original map[string]any
	if err := json.Unmarshal([]byte(run.ToolArgumentsJSON), &original); err != nil {
		t.Fatal(err)
	}
	args, _ := original["args"].([]any)
	if run.ToolName != "ssh_exec" || len(args) != 2 || args[0] != "%s" || args[1] != "args-ok" || original["background"] != true || original["elevated"] != false {
		t.Fatalf("original Tool Call was not persisted exactly: run=%#v arguments=%#v", run, original)
	}
	releaseOnce.Do(func() { close(transport.release) })
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		task, _, _, err := svc.GetTask(started.TaskID)
		if err == nil && task.Status == "completed" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background persistence test task did not complete")
}

func TestUnifiedTaskToolCancelsWithStandardExecResult(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/cancel-task.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	engine, _ := policy.Load("")
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	transport := &backgroundToolTransport{started: make(chan domain.ExecRequest, 1), release: make(chan struct{})}
	defer close(transport.release)
	svc := service.New(st, engine, transport, encryptor, security.NewRedactor(), config.Default().Limits)
	host, err := svc.AddHost(ctx, domain.Host{Name: "cancel-host", Address: "127.0.0.1", Port: 22, User: "ops", AgentEnabled: true}, "test")
	if err != nil {
		t.Fatal(err)
	}
	task, err := svc.StartTask(ctx, domain.ExecRequest{HostID: host.ID, Mode: domain.ExecProgram, Program: "uname", Reason: "verify cancellation"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.started:
	case <-time.After(time.Second):
		t.Fatal("background task did not start")
	}
	result, err := RunTaskTool(svc, TaskInput{TaskID: task.ID, Action: "cancel"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.TaskID != task.ID || result.Status != "cancelled" || result.Code != "cancelled" || result.ToolVersion != "1.1" {
		t.Fatalf("cancel result is not a standard ExecResult: %#v", result)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		runs, searchErr := st.SearchRuns(ctx, "", host.ID, "", 10)
		if searchErr == nil && len(runs) == 1 && runs[0].Status == "interrupted" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cancelled background execution did not stop")
}

func TestFileReadMetadataOnlyKeepsSHA256WithoutContent(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/file-read.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	engine, _ := policy.Load("")
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	transport := &fileReadToolTransport{}
	svc := service.New(st, engine, transport, encryptor, security.NewRedactor(), config.Default().Limits)
	host, err := svc.AddHost(ctx, domain.Host{Name: "file-host", Address: "127.0.0.1", Port: 22, User: "ops", AgentEnabled: true}, "test")
	if err != nil {
		t.Fatal(err)
	}
	runRead := func(input FileReadInput) domain.ExecResult {
		t.Helper()
		base, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		notifications := make(chan domain.ExecResult, 1)
		toolCtx := service.WithApprovalNotifier(service.WithBlockingApprovals(base), func(result domain.ExecResult) {
			notifications <- result
		})
		type outcome struct {
			result domain.ExecResult
			err    error
		}
		done := make(chan outcome, 1)
		beforeCalls := transport.callCount
		go func() {
			result, readErr := RunFileReadTool(toolCtx, svc, input, "test")
			done <- outcome{result: result, err: readErr}
		}()
		var pending domain.ExecResult
		select {
		case pending = <-notifications:
		case <-base.Done():
			t.Fatal("timed out waiting for file-read approval")
		}
		if pending.Status != "approval_required" || pending.Risk != domain.RiskReadOnly || pending.ApprovalID == "" || transport.callCount != beforeCalls {
			t.Fatalf("file read skipped approval: %#v", pending)
		}
		_, approveErr := svc.Approve(ctx, pending.ApprovalID, "reviewed file access", "operator")
		if approveErr != nil {
			t.Fatal(approveErr)
		}
		if transport.callCount != beforeCalls+1 {
			t.Fatalf("approved file read executed %d times", transport.callCount-beforeCalls)
		}
		select {
		case completed := <-done:
			if completed.err != nil {
				t.Fatal(completed.err)
			}
			return completed.result
		case <-base.Done():
			t.Fatal("timed out waiting for approved file read")
			return domain.ExecResult{}
		}
	}
	result := runRead(FileReadInput{HostID: host.ID, Path: "/etc/example.conf", MetadataOnly: true})
	if !result.OK || result.Stdout != "" || result.File == nil || result.File.SHA256 != strings.Repeat("a", 64) || result.File.Size != 15 {
		t.Fatalf("metadata-only result = %#v", result)
	}
	if !strings.Contains(transport.request.Script, "head -c 1") || strings.Contains(transport.request.Script, "tail -n") || strings.Contains(transport.request.Script, "tail -c") {
		t.Fatalf("metadata-only request did not minimize the remote read: %s", transport.request.Script)
	}
	metadataRun, err := st.GetRun(ctx, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var metadataRequest domain.ExecRequest
	if err := json.Unmarshal([]byte(metadataRun.RequestJSON), &metadataRequest); err != nil {
		t.Fatal(err)
	}
	if metadataRequest.Mode != domain.ExecRemoteRead || metadataRequest.RemotePath != "/etc/example.conf" || !metadataRequest.MetadataOnly || metadataRequest.Script != "" {
		t.Fatalf("metadata read was not persisted structurally: %#v", metadataRequest)
	}
	result = runRead(FileReadInput{HostID: host.ID, Path: "/etc/example.conf"})
	if result.Stdout == "" || !strings.Contains(transport.request.Script, "cat -- '/etc/example.conf'") || strings.Contains(transport.request.Script, "head -c") {
		t.Fatalf("default file read did not request complete content: result=%#v script=%s", result, transport.request.Script)
	}
	result = runRead(FileReadInput{HostID: host.ID, Path: "/etc/example.conf", OffsetBytes: -4})
	if !strings.Contains(transport.request.Script, "tail -c 4 -- '/etc/example.conf'") || result.File == nil || result.File.OffsetBytes != 11 {
		t.Fatalf("negative file offset did not read from the end: result=%#v script=%s", result, transport.request.Script)
	}
	result = runRead(FileReadInput{HostID: host.ID, Path: "/etc/example.conf", Pattern: "secret|token", MatchMode: domain.FileSearchRegex, ContextLines: 2})
	if !result.OK || result.Search == nil || !result.Search.Found || !strings.Contains(transport.request.Script, "grep -n -E -C 2 -- 'secret|token' '/etc/example.conf'") || strings.Contains(transport.request.Script, "head -n") {
		t.Fatalf("file read search mode was not dispatched: result=%#v script=%s", result, transport.request.Script)
	}
	searchRun, err := st.GetRun(ctx, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var searchRequest domain.ExecRequest
	if err := json.Unmarshal([]byte(searchRun.RequestJSON), &searchRequest); err != nil {
		t.Fatal(err)
	}
	if searchRequest.Mode != domain.ExecRemoteSearch || searchRequest.RemotePath != "/etc/example.conf" || searchRequest.SearchPattern != "secret|token" || searchRequest.SearchMatchMode != domain.FileSearchRegex || searchRequest.ContextLines != 2 || searchRequest.Script != "" {
		t.Fatalf("remote search was not persisted structurally: %#v", searchRequest)
	}
	result, err = RunFileReadTool(ctx, svc, FileReadInput{HostID: host.ID, Path: "/etc/example.conf", Pattern: "secret", MatchMode: domain.FileSearchLiteral, MaxBytes: 10}, "test")
	if err != nil || result.OK || result.Code != "validation_failed" {
		t.Fatalf("ambiguous file read mode was not rejected: result=%#v err=%v", result, err)
	}
	result, err = RunFileReadTool(ctx, svc, FileReadInput{HostID: host.ID, Path: "/etc/example.conf", Pattern: "secret"}, "test")
	if err != nil || result.OK || result.Code != "validation_failed" || !strings.Contains(result.Message, "match_mode") {
		t.Fatalf("search without match_mode was not rejected: result=%#v err=%v", result, err)
	}
	result, err = RunFileReadTool(ctx, svc, FileReadInput{HostID: host.ID, Path: "/etc/example.conf", MetadataOnly: true, MaxBytes: 10}, "test")
	if err != nil || result.OK || result.Code != "validation_failed" {
		t.Fatalf("ambiguous metadata read was not rejected: result=%#v err=%v", result, err)
	}
}

func TestUnifiedHistoryToolSearchesAndReadsExactRun(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/history.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for _, host := range []domain.Host{
		{ID: "host-a", Name: "host-a", Address: "127.0.0.1", Port: 22, User: "ops", AuthType: "agent", CreatedAt: now, UpdatedAt: now},
		{ID: "host-b", Name: "host-b", Address: "127.0.0.2", Port: 22, User: "ops", AuthType: "agent", CreatedAt: now, UpdatedAt: now},
	} {
		if _, err := st.UpsertHost(ctx, host); err != nil {
			t.Fatal(err)
		}
	}
	reviewJSON := `{"status":"completed","model":"private-review-model","deterministic_risk":"read_only","explanation":{"summary":"SUBAGENT_SUMMARY_SENTINEL","mechanism":"SUBAGENT_MECHANISM_SENTINEL","risks":["SUBAGENT_RISK_SENTINEL"]},"reviewed_at":"2026-07-29T00:00:00Z"}`
	for _, run := range []domain.Run{
		{ID: "run-nginx", SessionID: "session-a", HostID: "host-a", RequestJSON: `{"program":"nginx"}`, RequestDigest: "digest-a", Risk: domain.RiskReadOnly, Status: "completed", AIReviewJSON: reviewJSON, StartedAt: now.Add(-time.Minute)},
		{ID: "run-disk", SessionID: "session-b", HostID: "host-b", RequestJSON: `{"program":"df"}`, RequestDigest: "digest-b", Risk: domain.RiskReadOnly, Status: "completed", StartedAt: now},
	} {
		if err := st.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	storedRun, err := st.GetRun(ctx, "run-nginx")
	if err != nil {
		t.Fatal(err)
	}
	if storedRun.AIReview == nil || storedRun.AIReview.Explanation == nil {
		t.Fatal("history leak fixture did not persist the command explainer review")
	}
	engine, _ := policy.Load("")
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, engine, nil, encryptor, security.NewRedactor(), config.Default().Limits)
	searched, err := ReadHistoryTool(ctx, svc, HistorySearchInput{Query: "nginx", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(searched.Runs) != 1 || searched.Runs[0].ID != "run-nginx" {
		t.Fatalf("history search result = %#v", searched)
	}
	encodedHistory, err := json.Marshal(searched)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"ai_review", "private-review-model", "SUBAGENT_SUMMARY_SENTINEL", "SUBAGENT_MECHANISM_SENTINEL", "SUBAGENT_RISK_SENTINEL"} {
		if strings.Contains(string(encodedHistory), leaked) {
			t.Fatalf("history exposed command explainer data %q: %s", leaked, encodedHistory)
		}
	}
	exact, err := ReadHistoryTool(ctx, svc, HistorySearchInput{RunID: "run-disk"})
	if err != nil {
		t.Fatal(err)
	}
	if len(exact.Runs) != 1 || exact.Runs[0].ID != "run-disk" {
		t.Fatalf("exact history result = %#v", exact)
	}
	sessionCtx := service.WithSessionID(ctx, "session-a")
	sessionRuns, err := ReadHistoryTool(sessionCtx, svc, HistorySearchInput{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessionRuns.Runs) != 1 || sessionRuns.Runs[0].ID != "run-nginx" {
		t.Fatalf("session history leaked another conversation: %#v", sessionRuns)
	}
	if _, err := ReadHistoryTool(sessionCtx, svc, HistorySearchInput{RunID: "run-disk"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("exact history read crossed session boundary: %v", err)
	}
	sessionExact, err := ReadHistoryTool(sessionCtx, svc, HistorySearchInput{RunID: "run-nginx"})
	if err != nil || len(sessionExact.Runs) != 1 || sessionExact.Runs[0].ID != "run-nginx" {
		t.Fatalf("current-session exact history failed: %#v err=%v", sessionExact, err)
	}

	loaded, err := BuildTools(svc)
	if err != nil {
		t.Fatal(err)
	}
	var historyTool tool.InvokableTool
	for _, candidate := range loaded {
		info, infoErr := candidate.Info(ctx)
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		if info.Name == "ssh_history" {
			historyTool = candidate.(tool.InvokableTool)
			break
		}
	}
	if historyTool == nil {
		t.Fatal("ssh_history was not registered")
	}
	failureJSON, err := historyTool.InvokableRun(ctx, `{"run_id":"run-disk","query":"df"}`)
	if err != nil {
		t.Fatal(err)
	}
	var failure domain.ToolFailure
	if err := json.Unmarshal([]byte(failureJSON), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.OK || failure.Code != "validation_failed" || !strings.Contains(failure.Message, "mutually exclusive") {
		t.Fatalf("history input conflict was not structured: %#v", failure)
	}
}

func TestDisabledToolIsExcludedFromRunnerAndRetainedInCatalog(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/disabled-tools.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	engine, err := policy.Load("")
	if err != nil {
		t.Fatal(err)
	}
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, engine, nil, encryptor, security.NewRedactor(), config.Default().Limits)
	if err := svc.SetAgentToolEnabled(ctx, "ssh_exec", false, "test"); err != nil {
		t.Fatal(err)
	}

	loaded, catalog, err := buildToolSet(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	loadedNames := make(map[string]bool, len(loaded))
	for _, candidate := range loaded {
		info, infoErr := candidate.Info(ctx)
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		loadedNames[info.Name] = true
	}
	if loadedNames["ssh_exec"] {
		t.Fatal("disabled ssh_exec was still loaded into the runner")
	}
	foundDisabled := false
	for _, descriptor := range catalog {
		if descriptor.Name == "ssh_exec" {
			foundDisabled = true
			if descriptor.Enabled {
				t.Fatal("disabled ssh_exec was marked enabled in the catalog")
			}
		}
	}
	if !foundDisabled {
		t.Fatal("disabled ssh_exec disappeared from the management catalog")
	}

	if err := svc.SetAgentToolEnabled(ctx, "ssh_exec", true, "test"); err != nil {
		t.Fatal(err)
	}
	reloaded, _, err := buildToolSet(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range reloaded {
		info, infoErr := candidate.Info(ctx)
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		if info.Name == "ssh_exec" {
			return
		}
	}
	t.Fatal("re-enabled ssh_exec was not loaded into the runner")
}

func TestUnifiedSkillToolReadsTheLiveAdministratorRegistry(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/skills.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	engine, _ := policy.Load("")
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	svc := service.New(st, engine, nil, encryptor, security.NewRedactor(), cfg.Limits, cfg)
	if _, err := svc.SaveAdminSkill(ctx, "custom-diagnosis", "# Custom Diagnosis\n\nUse the administrator workflow.", "test"); err != nil {
		t.Fatal(err)
	}
	loaded, err := BuildTools(svc)
	if err != nil {
		t.Fatal(err)
	}
	var skillTool tool.InvokableTool
	for _, candidate := range loaded {
		info, infoErr := candidate.Info(ctx)
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		if info.Name == "ops_skill" {
			skillTool = candidate.(tool.InvokableTool)
		}
	}
	if skillTool == nil {
		t.Fatal("ops_skill was not registered")
	}
	result, err := skillTool.InvokableRun(service.WithSessionID(ctx, "session_skill"), `{"name":"custom-diagnosis"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Use the administrator workflow") {
		t.Fatalf("dynamic skill content was not returned: %s", result)
	}
	if _, err := svc.SetAdminSkillEnabled(ctx, "custom-diagnosis", false, "test"); err != nil {
		t.Fatal(err)
	}
	disabledJSON, err := skillTool.InvokableRun(ctx, `{"name":"custom-diagnosis"}`)
	if err != nil {
		t.Fatalf("disabled skill aborted the ToolNode: %v", err)
	}
	var disabled domain.ToolFailure
	if err := json.Unmarshal([]byte(disabledJSON), &disabled); err != nil {
		t.Fatal(err)
	}
	if disabled.OK || disabled.Status != "failed" || disabled.Code != "configuration_required" || !strings.Contains(disabled.Message, "disabled") {
		t.Fatalf("disabled skill did not return a structured failure: %#v", disabled)
	}
	listed, err := skillTool.InvokableRun(ctx, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listed, "custom-diagnosis") {
		t.Fatalf("disabled skill remained discoverable: %s", listed)
	}
}

func TestPlanStepUpdateReturnsCurrentPlanWithoutAbortingToolNode(t *testing.T) {
	ctx := service.WithSessionID(context.Background(), "session_plan_transition")
	st, err := store.Open(ctx, t.TempDir()+"/tools.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	engine, _ := policy.Load("")
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, engine, nil, encryptor, security.NewRedactor(), config.Default().Limits)
	if _, err := svc.CreateAgentPlan(ctx, "Repair the service", []string{"Inspect", "Repair"}, "test"); err != nil {
		t.Fatal(err)
	}
	tools, err := BuildTools(svc)
	if err != nil {
		t.Fatal(err)
	}
	var updateTool tool.InvokableTool
	for _, candidate := range tools {
		info, infoErr := candidate.Info(ctx)
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		if info.Name == "ops_plan_step_update" {
			updateTool = candidate.(tool.InvokableTool)
			break
		}
	}
	if updateTool == nil {
		t.Fatal("ops_plan_step_update tool was not registered")
	}
	resultJSON, err := updateTool.InvokableRun(ctx, `{"step_number":2,"status":"completed","evidence":"skipped step one"}`)
	if err != nil {
		t.Fatalf("invalid plan transition aborted the ToolNode: %v", err)
	}
	var result planToolResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatal(err)
	}
	if result.OK || result.Status != "failed" || result.Code != "invalid_state" || result.Plan == nil {
		t.Fatalf("unexpected plan transition failure: %#v", result)
	}
	if result.Plan.Steps[0].Status != "in_progress" || result.Plan.Steps[1].Status != "pending" || !strings.Contains(result.NextAction, "step 1") {
		t.Fatalf("current plan state was not returned: %#v", result.Plan)
	}
}

func TestToolErrorMiddlewareKeepsRecoverableFailuresInsideToolNode(t *testing.T) {
	type input struct {
		Value string `json:"value"`
	}
	rawFailure, err := toolutils.InferTool("raw_failure", "fails for testing", func(context.Context, input) (string, error) {
		return "", fmt.Errorf("invalid widget")
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err := compose.NewToolNode(context.Background(), &compose.ToolsNodeConfig{
		Tools:               []tool.BaseTool{rawFailure},
		ToolCallMiddlewares: []compose.ToolMiddleware{{Invokable: normalizeToolCallErrors}},
		UnknownToolsHandler: unknownToolResult,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name      string
		toolName  string
		arguments string
		wantCode  string
	}{
		{name: "business failure", toolName: "raw_failure", arguments: `{"value":"x"}`, wantCode: "validation_failed"},
		{name: "malformed arguments", toolName: "raw_failure", arguments: `{"value":`, wantCode: "validation_failed"},
		{name: "unknown tool", toolName: "missing_tool", arguments: `{}`, wantCode: "unknown_tool"},
	} {
		t.Run(test.name, func(t *testing.T) {
			messages, invokeErr := node.Invoke(context.Background(), schema.AssistantMessage("", []schema.ToolCall{{
				ID: "call-test", Function: schema.FunctionCall{Name: test.toolName, Arguments: test.arguments},
			}}))
			if invokeErr != nil {
				t.Fatalf("recoverable tool failure aborted the ToolNode: %v", invokeErr)
			}
			if len(messages) != 1 {
				t.Fatalf("tool messages = %d", len(messages))
			}
			var failure domain.ToolFailure
			if err := json.Unmarshal([]byte(messages[0].Content), &failure); err != nil {
				t.Fatal(err)
			}
			if failure.OK || failure.Status != "failed" || failure.Code != test.wantCode {
				t.Fatalf("unexpected ToolNode failure: %#v", failure)
			}
		})
	}

	stream, err := node.Stream(context.Background(), schema.AssistantMessage("", []schema.ToolCall{{
		ID: "call-stream", Function: schema.FunctionCall{Name: "raw_failure", Arguments: `{"value":"x"}`},
	}}))
	if err != nil {
		t.Fatalf("streaming ToolNode rejected a recoverable failure: %v", err)
	}
	var streamedFailure domain.ToolFailure
	for {
		messages, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			t.Fatalf("streaming ToolNode aborted while returning failure: %v", recvErr)
		}
		for _, message := range messages {
			if err := json.Unmarshal([]byte(message.Content), &streamedFailure); err != nil {
				t.Fatal(err)
			}
		}
	}
	if streamedFailure.OK || streamedFailure.Status != "failed" || streamedFailure.Code != "validation_failed" {
		t.Fatalf("streaming ToolNode did not return the structured failure: %#v", streamedFailure)
	}
}

func TestToolErrorMiddlewarePreservesCancellation(t *testing.T) {
	cancelTool, err := toolutils.InferTool("cancel_tool", "cancels for testing", func(context.Context, struct{}) (string, error) {
		return "", context.Canceled
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err := compose.NewToolNode(context.Background(), &compose.ToolsNodeConfig{
		Tools:               []tool.BaseTool{cancelTool},
		ToolCallMiddlewares: []compose.ToolMiddleware{{Invokable: normalizeToolCallErrors}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = node.Invoke(context.Background(), schema.AssistantMessage("", []schema.ToolCall{{
		ID: "call-cancel", Function: schema.FunctionCall{Name: "cancel_tool", Arguments: `{}`},
	}}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was converted into a tool result: %v", err)
	}
}

func TestAgentLoopReturnsToolFailureToModelAndContinues(t *testing.T) {
	type input struct {
		Value string `json:"value"`
	}
	rawFailure, err := toolutils.InferTool("raw_failure", "fails for testing", func(context.Context, input) (string, error) {
		return "", fmt.Errorf("invalid widget")
	})
	if err != nil {
		t.Fatal(err)
	}
	chatModel := &toolFailureLoopModel{}
	agentInstance, err := adk.NewChatModelAgent(context.Background(), &adk.ChatModelAgentConfig{
		Name: "tool-failure-test", Description: "tool failure regression", Model: chatModel, MaxIterations: 3,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
			Tools: []tool.BaseTool{rawFailure}, ToolCallMiddlewares: []compose.ToolMiddleware{{Invokable: normalizeToolCallErrors}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := adk.NewRunner(context.Background(), adk.RunnerConfig{Agent: agentInstance})
	iterator := runner.Run(context.Background(), []*schema.Message{schema.UserMessage("run the failing tool")})
	finalAnswer := ""
	for {
		event, ok := iterator.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			t.Fatalf("recoverable tool failure aborted the Agent loop: %v", event.Err)
		}
		if event.Output != nil && event.Output.MessageOutput != nil && event.Output.MessageOutput.Message != nil {
			message := event.Output.MessageOutput.Message
			if message.Role == schema.Assistant && message.Content != "" {
				finalAnswer = message.Content
			}
		}
	}
	if finalAnswer != "handled the tool failure" || chatModel.calls != 2 {
		t.Fatalf("Agent did not recover after the tool failure: calls=%d answer=%q", chatModel.calls, finalAnswer)
	}
	if len(chatModel.inputs) != 2 {
		t.Fatalf("model inputs=%d", len(chatModel.inputs))
	}
	foundFailure := false
	for _, message := range chatModel.inputs[1] {
		if message.Role != schema.Tool || message.ToolCallID != "call-invalid" {
			continue
		}
		var failure domain.ToolFailure
		if err := json.Unmarshal([]byte(message.Content), &failure); err != nil {
			t.Fatalf("tool result was not structured JSON: %v", err)
		}
		if !failure.OK && failure.Status == "failed" && failure.Code == "validation_failed" {
			foundFailure = true
		}
	}
	if !foundFailure {
		t.Fatalf("second model request did not contain the structured tool failure: %#v", chatModel.inputs[1])
	}
}

func TestExecutionToolReturnsStructuredNotFoundWithoutAbortingToolNode(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/tools.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	engine, _ := policy.Load("")
	encryptor, err := security.NewEncryptor("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, engine, nil, encryptor, security.NewRedactor(), config.Default().Limits)
	tools, err := BuildTools(svc)
	if err != nil {
		t.Fatal(err)
	}
	var execTool tool.InvokableTool
	for _, candidate := range tools {
		info, infoErr := candidate.Info(ctx)
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		if info.Name == "ssh_exec" {
			execTool = candidate.(tool.InvokableTool)
		}
	}
	resultJSON, err := execTool.InvokableRun(ctx, `{"host_id":"missing","program":"id","reason":"inspect identity"}`)
	if err != nil {
		t.Fatalf("expected not_found Tool result, got fatal error: %v", err)
	}
	var result domain.ExecResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatal(err)
	}
	if result.OK || result.Code != "not_found" || result.Retryable || result.TaskID != "" {
		t.Fatalf("unexpected structured failure: %#v", result)
	}
}
