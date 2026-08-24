package mcpserver

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/agent"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func mustAgentInputSchema[T any](t *testing.T) json.RawMessage {
	t.Helper()
	schema, err := agent.InputSchemaJSON[T]()
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func normalizedSchema(t *testing.T, value any) any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		t.Fatal(err)
	}
	return normalized
}

func assertSchemaValidation(t *testing.T, schema *jsonschema.Schema, arguments string, valid bool) {
	t.Helper()
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	var input any
	if err := json.Unmarshal([]byte(arguments), &input); err != nil {
		t.Fatal(err)
	}
	err = resolved.Validate(input)
	if valid && err != nil {
		t.Fatalf("schema rejected valid arguments %s: %v", arguments, err)
	}
	if !valid && err == nil {
		t.Fatalf("schema accepted invalid arguments %s", arguments)
	}
}

func TestMergedToolSchemasExpressTheirModes(t *testing.T) {
	tunnel := inputSchema[agent.SSHTunnelInput]()
	assertSchemaValidation(t, tunnel, `{"action":"list"}`, true)
	assertSchemaValidation(t, tunnel, `{"action":"list","reason":"unused"}`, false)
	assertSchemaValidation(t, tunnel, `{"action":"start","host_id":"host_x","direction":"-L","remote_port":22,"reason":"test"}`, false)
	assertSchemaValidation(t, tunnel, `{"action":"start","host_id":"host_x","remote_port":22,"reason":"test"}`, true)
	assertSchemaValidation(t, tunnel, `{"action":"start","host_id":"host_x","direction":"reverse","local_port":8080,"reason":"test"}`, true)
	assertSchemaValidation(t, tunnel, `{"action":"stop","tunnel_id":"tunnel_x"}`, true)

	task := inputSchema[agent.TaskInput]()
	assertSchemaValidation(t, task, `{"action":"status","task_id":"task_x","wait_seconds":10,"block_until":"terminal"}`, true)
	assertSchemaValidation(t, task, `{"action":"status","task_id":"task_x","wait_seconds":10,"block_until":"completed"}`, false)
	assertSchemaValidation(t, task, `{"action":"status","task_id":"task_x","block_until":"output"}`, false)
	assertSchemaValidation(t, task, `{"action":"status","task_id":"task_x","wait_seconds":0,"block_until":"output"}`, false)
	assertSchemaValidation(t, task, `{"action":"status","task_id":"task_x","max_output_bytes":0,"output_view":"tail"}`, false)
	assertSchemaValidation(t, task, `{"action":"cancel","task_id":"task_x","wait_seconds":10}`, false)

	sshShell := inputSchema[agent.SSHShellInput]()
	assertSchemaValidation(t, sshShell, `{"action":"list"}`, true)
	assertSchemaValidation(t, sshShell, `{"action":"list","host_id":"host_x"}`, false)
	assertSchemaValidation(t, sshShell, `{"action":"input","shell_id":"shell_x","input":"top","submit":true}`, true)

	workspaceShell := inputSchema[agent.WorkspaceShellInput]()
	assertSchemaValidation(t, workspaceShell, `{"action":"run","script":"go test ./...","reason":"test"}`, true)
	assertSchemaValidation(t, workspaceShell, `{"action":"completed"}`, false)

	fileRead := inputSchema[agent.FileReadInput]()
	assertSchemaValidation(t, fileRead, `{"host_id":"host_x","path":"/tmp/a","pattern":"needle"}`, false)
	assertSchemaValidation(t, fileRead, `{"host_id":"host_x","path":"/tmp/a","pattern":"needle","match_mode":"literal"}`, true)
	assertSchemaValidation(t, fileRead, `{"host_id":"host_x","path":"/tmp/a","pattern":"needle","match_mode":"literal","metadata_only":false}`, true)
	assertSchemaValidation(t, fileRead, `{"host_id":"host_x","path":"/tmp/a","pattern":"needle","match_mode":"literal","metadata_only":true}`, false)
	assertSchemaValidation(t, fileRead, `{"host_id":"host_x","path":"/tmp/a","pattern":"needle","match_mode":"literal","tail_lines":10}`, false)
	assertSchemaValidation(t, fileRead, `{"host_id":"host_x","path":"/tmp/a","tail_lines":10,"offset_bytes":2}`, false)

	history := inputSchema[agent.HistorySearchInput]()
	assertSchemaValidation(t, history, `{"output_view":"head"}`, false)
	assertSchemaValidation(t, history, `{"run_id":"run_x","output_view":"head"}`, true)
	assertSchemaValidation(t, history, `{"run_id":"run_x","limit":5}`, false)
	assertSchemaValidation(t, history, `{"run_id":"run_x","query":"error","limit":5}`, true)
	assertSchemaValidation(t, history, `{"run_id":"run_x","after_stdout_bytes":-1}`, false)

	webSearch := inputSchema[agent.WebSearchInput]()
	assertSchemaValidation(t, webSearch, `{"query":"release","time_range":"week","start_date":"2026-08-01"}`, false)
	assertSchemaValidation(t, webSearch, `{"query":"release","search_depth":"basic","chunks_per_source":1}`, false)
	assertSchemaValidation(t, webSearch, `{"query":"release","search_depth":"advanced","chunks_per_source":1}`, true)

	webExtract := inputSchema[agent.WebExtractInput]()
	assertSchemaValidation(t, webExtract, `{"urls":["https://example.com"],"chunks_per_source":1}`, false)
	assertSchemaValidation(t, webExtract, `{"urls":["https://example.com"],"query":"install","chunks_per_source":1}`, true)
}

func TestServerExposesMergedBackgroundTaskTools(t *testing.T) {
	ctx := context.Background()
	instance := New(nil, "test")
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := instance.server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	result, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	taskFound := false
	shellFound := false
	tunnelFound := false
	fileTransferFound := false
	fileReadFound := false
	fileEditFound := false
	hostListFound := false
	historyFound := false
	backgroundInputs := map[string]bool{"ssh_exec": false, "ssh_run_script": false}
	expectedSchemas := map[string]json.RawMessage{
		"ssh_host_inspect":  mustAgentInputSchema[agent.HostInput](t),
		"ssh_host_list":     mustAgentInputSchema[struct{}](t),
		"ssh_exec":          mustAgentInputSchema[agent.ExecInput](t),
		"ssh_run_script":    mustAgentInputSchema[agent.ScriptInput](t),
		"ssh_task":          mustAgentInputSchema[agent.TaskInput](t),
		"ssh_file_read":     mustAgentInputSchema[agent.FileReadInput](t),
		"ssh_file_list":     mustAgentInputSchema[agent.FileListInput](t),
		"ssh_file_edit":     mustAgentInputSchema[agent.FileEditInput](t),
		"ssh_file_transfer": mustAgentInputSchema[agent.SSHFileTransferInput](t),
		"ssh_tunnel":        mustAgentInputSchema[agent.SSHTunnelInput](t),
		"ssh_shell":         mustAgentInputSchema[agent.SSHShellInput](t),
		"ssh_history":       mustAgentInputSchema[agent.HistorySearchInput](t),
	}
	for _, registered := range result.Tools {
		if expected, ok := expectedSchemas[registered.Name]; ok && !reflect.DeepEqual(normalizedSchema(t, registered.InputSchema), normalizedSchema(t, expected)) {
			actual, _ := json.Marshal(registered.InputSchema)
			t.Fatalf("%s MCP schema diverged from the Agent schema: actual=%s expected=%s", registered.Name, actual, expected)
		}
		for _, retired := range []string{"ssh_approval_status", "ssh_task_start", "ssh_task_status", "ssh_task_tail", "ssh_task_list", "ssh_task_get", "ssh_task_cancel", "ssh_file_write", "ssh_file_apply_patch", "ssh_file_restore", "ssh_file_create", "ssh_file_stat", "ssh_config_apply", "ssh_config_restore", "workspace_list", "workspace_file_list", "workspace_file_read", "workspace_file_edit", "workspace_file_upload", "workspace_shell", "workspace_file_apply_patch", "workspace_file_create", "ssh_file_search", "workspace_file_search", "ssh_history_search", "ssh_history_get", "skill", "ops_skill", "ops_skill_list", "ops_skill_get"} {
			if registered.Name == retired {
				t.Fatalf("retired %s tool remains in the MCP catalog", retired)
			}
		}
		if registered.Name == "ssh_file_edit" {
			schemaJSON, marshalErr := json.Marshal(registered.InputSchema)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			schema := string(schemaJSON)
			fileEditFound = strings.Contains(schema, `"old_text"`) && strings.Contains(schema, `"new_text"`) && strings.Contains(schema, `"validator_id"`) && !strings.Contains(schema, `"diff"`) && !strings.Contains(schema, `"validator"`)
		}
		if registered.Name == "ssh_shell" {
			schemaJSON, marshalErr := json.Marshal(registered.InputSchema)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			schema := string(schemaJSON)
			shellFound = strings.Contains(schema, `"action"`) && strings.Contains(schema, `"shell_id"`) && strings.Contains(schema, `"wait_seconds"`) && strings.Contains(schema, `"after_sequence"`) && strings.Contains(schema, `"max_output_bytes"`)
		}
		if registered.Name == "ssh_tunnel" {
			schemaJSON, marshalErr := json.Marshal(registered.InputSchema)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			tunnelFound = strings.Contains(string(schemaJSON), `"remote_port"`) && strings.Contains(string(schemaJSON), `"tunnel_id"`)
		}
		readOnly := registered.Name == "ssh_host_inspect" || registered.Name == "ssh_host_list" || registered.Name == "ssh_file_read" || registered.Name == "ssh_file_list" || registered.Name == "ssh_history"
		if registered.Name == "ssh_host_list" {
			hostListFound = true
		}
		if registered.Name == "ssh_history" {
			schemaJSON, marshalErr := json.Marshal(registered.InputSchema)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			schema := string(schemaJSON)
			historyFound = strings.Contains(schema, `"run_id"`) && strings.Contains(schema, `"cursor"`) && strings.Contains(schema, `"match_mode"`)
		}
		if readOnly && (registered.Annotations == nil || !registered.Annotations.ReadOnlyHint || registered.Annotations.DestructiveHint == nil || *registered.Annotations.DestructiveHint) {
			t.Fatalf("read-only MCP tool %s has inaccurate annotations: %#v", registered.Name, registered.Annotations)
		}
		if !readOnly && registered.Name == "ssh_exec" && (registered.Annotations == nil || registered.Annotations.ReadOnlyHint || registered.Annotations.DestructiveHint == nil || !*registered.Annotations.DestructiveHint) {
			t.Fatalf("dynamic MCP tool %s has inaccurate annotations: %#v", registered.Name, registered.Annotations)
		}
		if registered.Name == "ssh_task" {
			schemaJSON, marshalErr := json.Marshal(registered.InputSchema)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			taskFound = strings.Contains(string(schemaJSON), `"action"`) && strings.Contains(string(schemaJSON), `"wait_seconds"`) && strings.Contains(string(schemaJSON), `"block_until"`)
		}
		if registered.Name == "ssh_file_read" {
			schemaJSON, marshalErr := json.Marshal(registered.InputSchema)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			schema := string(schemaJSON)
			fileReadFound = strings.Contains(schema, `"metadata_only"`) && strings.Contains(schema, `"pattern"`) && strings.Contains(schema, `"match_mode"`) && strings.Contains(schema, `"regex"`) && !strings.Contains(schema, `"max_matches"`)
		}
		if registered.Name == "ssh_file_transfer" {
			schemaJSON, marshalErr := json.Marshal(registered.InputSchema)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			fileTransferFound = strings.Contains(string(schemaJSON), `"source_host_id"`) && strings.Contains(string(schemaJSON), `"destination_host_id"`)
		}
		if _, tracked := backgroundInputs[registered.Name]; tracked {
			schemaJSON, marshalErr := json.Marshal(registered.InputSchema)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			backgroundInputs[registered.Name] = strings.Contains(string(schemaJSON), `"background"`) && strings.Contains(string(schemaJSON), `"max_output_bytes"`) && strings.Contains(string(schemaJSON), `"output_view"`)
		}
	}
	if !fileEditFound {
		t.Fatal("ssh_file_edit is missing from the MCP catalog")
	}
	if !hostListFound {
		t.Fatal("ssh_host_list is missing from the MCP Server catalog")
	}
	if !historyFound {
		t.Fatal("ssh_history is missing from the MCP Server catalog")
	}
	if !taskFound || !backgroundInputs["ssh_exec"] || !backgroundInputs["ssh_run_script"] {
		t.Fatalf("merged background task interface is incomplete: task=%v inputs=%#v", taskFound, backgroundInputs)
	}
	if !fileTransferFound || !fileReadFound {
		t.Fatalf("merged read tools are incomplete: transfer=%v file_read=%v", fileTransferFound, fileReadFound)
	}
	if !shellFound || !tunnelFound {
		t.Fatalf("interactive MCP tools are incomplete: shell=%v tunnel=%v", shellFound, tunnelFound)
	}
}
