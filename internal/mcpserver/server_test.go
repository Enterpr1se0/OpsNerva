package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

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
	backgroundInputs := map[string]bool{"ssh_exec": false, "ssh_run_script": false}
	for _, registered := range result.Tools {
		for _, retired := range []string{"ssh_approval_status", "ssh_task_start", "ssh_task_status", "ssh_task_tail", "ssh_task_list", "ssh_task_get", "ssh_task_cancel", "ssh_file_write", "ssh_file_apply_patch", "ssh_file_restore", "ssh_file_create", "ssh_file_stat", "ssh_config_apply", "ssh_config_restore", "workspace_list", "workspace_file_list", "workspace_file_read", "workspace_file_edit", "workspace_file_upload", "workspace_shell", "workspace_file_apply_patch", "workspace_file_create", "ssh_file_search", "workspace_file_search", "ssh_history", "ssh_history_search", "ssh_history_get", "skill", "ops_skill", "ops_skill_list", "ops_skill_get"} {
			if registered.Name == retired {
				t.Fatalf("retired %s tool remains in the MCP catalog", retired)
			}
		}
		if registered.Name == "ssh_file_edit" {
			schemaJSON, marshalErr := json.Marshal(registered.InputSchema)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			fileEditFound = strings.Contains(string(schemaJSON), `"validator_id"`) && !strings.Contains(string(schemaJSON), `"validator"`)
		}
		if registered.Name == "ssh_shell" {
			schemaJSON, marshalErr := json.Marshal(registered.InputSchema)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			shellFound = strings.Contains(string(schemaJSON), `"action"`) && strings.Contains(string(schemaJSON), `"shell_id"`)
		}
		if registered.Name == "ssh_tunnel" {
			schemaJSON, marshalErr := json.Marshal(registered.InputSchema)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			tunnelFound = strings.Contains(string(schemaJSON), `"remote_port"`) && strings.Contains(string(schemaJSON), `"tunnel_id"`)
		}
		readOnly := registered.Name == "ssh_host_inspect" || registered.Name == "ssh_host_list" || registered.Name == "ssh_file_read" || registered.Name == "ssh_file_list"
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
