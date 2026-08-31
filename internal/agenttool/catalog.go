package agenttool

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

// Descriptor exposes the resolved function schema to the control plane.
type Descriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Category    string          `json:"category"`
	Guard       string          `json:"guard"`
	Enabled     bool            `json:"enabled"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type Catalog struct {
	Loaded        bool         `json:"loaded"`
	Agent         string       `json:"agent"`
	Framework     string       `json:"framework"`
	ExecutionMode string       `json:"execution_mode"`
	ProviderID    string       `json:"provider_id,omitempty"`
	Model         string       `json:"model,omitempty"`
	LoadedAt      string       `json:"loaded_at,omitempty"`
	Count         int          `json:"count"`
	Total         int          `json:"total"`
	Tools         []Descriptor `json:"tools"`
}

// Describe reads Eino's resolved ToolInfo instead of maintaining a second
// hand-written schema for the control plane.
func Describe(ctx context.Context, tools []tool.BaseTool) ([]Descriptor, error) {
	descriptors := make([]Descriptor, 0, len(tools))
	for _, candidate := range tools {
		info, err := candidate.Info(ctx)
		if err != nil {
			return nil, err
		}
		schemaJSON := json.RawMessage(`{"type":"object","properties":{}}`)
		if info.ParamsOneOf != nil {
			inputSchema, err := info.ParamsOneOf.ToJSONSchema()
			if err != nil {
				return nil, err
			}
			if inputSchema != nil {
				schemaJSON, err = json.Marshal(inputSchema)
				if err != nil {
					return nil, err
				}
			}
		}
		descriptors = append(descriptors, Descriptor{
			Name: info.Name, Description: info.Desc, Category: category(info.Name), Guard: guard(info.Name), Enabled: true, InputSchema: schemaJSON,
		})
	}
	return descriptors, nil
}

// InputSchemaJSON returns the same Eino-derived JSON Schema used by InferTool.
func InputSchemaJSON[T any]() (json.RawMessage, error) {
	params, err := toolutils.GoStruct2ParamsOneOf[T]()
	if err != nil {
		return nil, err
	}
	inputSchema, err := params.ToJSONSchema()
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(inputSchema)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}

func category(name string) string {
	switch {
	case isPlanningTool(name):
		return "planning"
	case name == "skill":
		return "skills"
	case strings.HasPrefix(name, "mcp__"):
		return "mcp"
	case strings.HasPrefix(name, "workspace_"):
		return "workspace"
	case strings.HasPrefix(name, "web_"):
		return "web"
	case strings.HasPrefix(name, "ssh_host_"):
		return "hosts"
	case name == "ssh_task":
		return "tasks"
	case strings.HasPrefix(name, "ssh_file_"):
		return "remote_files"
	case name == "ssh_history":
		return "history"
	default:
		return "execution"
	}
}

func guard(name string) string {
	switch name {
	case "TaskCreate", "TaskGet", "TaskUpdate", "TaskList":
		return "agent_state"
	case "ssh_exec", "ssh_run_script":
		return "approval_required"
	case "ssh_tunnel", "ssh_shell", "ssh_file_read", "workspace_file_read", "ssh_file_edit", "ssh_file_transfer", "workspace_file_edit", "workspace_file_delete", "workspace_file_upload", "workspace_file_download", "workspace_shell":
		return "approval_required"
	case "ssh_task":
		return "audited_control"
	default:
		if strings.HasPrefix(name, "mcp__") {
			return "external_mcp"
		}
		return "read_only"
	}
}

func isPlanningTool(name string) bool {
	switch name {
	case "TaskCreate", "TaskGet", "TaskUpdate", "TaskList":
		return true
	default:
		return false
	}
}
