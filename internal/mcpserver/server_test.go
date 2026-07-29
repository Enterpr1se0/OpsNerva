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
	fileTransferFound := false
	fileReadFound := false
	fileEditFound := false
	historyFound := false
	backgroundInputs := map[string]bool{"ssh_exec": false, "ssh_run_script": false}
	for _, registered := range result.Tools {
		for _, retired := range []string{"ssh_approval_status", "ssh_task_start", "ssh_task_status", "ssh_task_tail", "ssh_task_list", "ssh_task_get", "ssh_task_cancel", "ssh_file_write", "ssh_file_apply_patch", "ssh_file_restore", "ssh_file_create", "ssh_file_stat", "ssh_config_apply", "ssh_config_restore", "workspace_list", "workspace_file_list", "workspace_file_read", "workspace_file_edit", "workspace_file_upload", "workspace_shell", "workspace_file_apply_patch", "workspace_file_create", "ssh_file_search", "workspace_file_search", "ssh_history_search", "ssh_history_get", "ops_skill", "ops_skill_list", "ops_skill_get"} {
			if registered.Name == retired {
				t.Fatalf("retired %s tool remains in the MCP catalog", retired)
			}
		}
		if registered.Name == "ssh_file_edit" {
			fileEditFound = true
		}
		if registered.Name == "ssh_task" {
			schemaJSON, marshalErr := json.Marshal(registered.InputSchema)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			taskFound = strings.Contains(string(schemaJSON), `"action"`)
		}
		if registered.Name == "ssh_file_read" {
			schemaJSON, marshalErr := json.Marshal(registered.InputSchema)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			schema := string(schemaJSON)
			fileReadFound = strings.Contains(schema, `"metadata_only"`) && strings.Contains(schema, `"pattern"`) && strings.Contains(schema, `"match_mode"`) && strings.Contains(schema, `"regex"`) && !strings.Contains(schema, `"max_matches"`)
		}
		if registered.Name == "ssh_history" {
			historyFound = true
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
			backgroundInputs[registered.Name] = strings.Contains(string(schemaJSON), `"background"`)
		}
	}
	if !fileEditFound {
		t.Fatal("ssh_file_edit is missing from the MCP catalog")
	}
	if !taskFound || !backgroundInputs["ssh_exec"] || !backgroundInputs["ssh_run_script"] {
		t.Fatalf("merged background task interface is incomplete: task=%v inputs=%#v", taskFound, backgroundInputs)
	}
	if !fileTransferFound || !fileReadFound || !historyFound {
		t.Fatalf("merged read tools are incomplete: transfer=%v file_read=%v history=%v", fileTransferFound, fileReadFound, historyFound)
	}
}
