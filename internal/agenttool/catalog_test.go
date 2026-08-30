package agenttool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

func TestDescribeUsesResolvedSchemaAndStablePolicy(t *testing.T) {
	t.Parallel()
	candidate, err := toolutils.InferTool("ssh_exec", SSHExecDescription, func(context.Context, ExecInput) (ExecResult, error) {
		return ExecResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptors, err := Describe(context.Background(), []tool.BaseTool{candidate})
	if err != nil {
		t.Fatal(err)
	}
	if len(descriptors) != 1 {
		t.Fatalf("descriptor count = %d, want 1", len(descriptors))
	}
	descriptor := descriptors[0]
	if descriptor.Name != "ssh_exec" || descriptor.Description != SSHExecDescription || descriptor.Category != "execution" || descriptor.Guard != "approval_required" || !descriptor.Enabled {
		t.Fatalf("unexpected descriptor: %#v", descriptor)
	}
	if !json.Valid(descriptor.InputSchema) {
		t.Fatalf("invalid input schema: %s", descriptor.InputSchema)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(descriptor.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Properties["program"] == nil || schema.Properties["args"] == nil || !contains(schema.Required, "program") {
		t.Fatalf("ssh_exec schema is incomplete: %s", descriptor.InputSchema)
	}
}

func TestCatalogClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, category, guard string
	}{
		{name: "TaskCreate", category: "planning", guard: "agent_state"},
		{name: "skill", category: "skills", guard: "read_only"},
		{name: "mcp__search", category: "mcp", guard: "external_mcp"},
		{name: "workspace_file_read", category: "workspace", guard: "approval_required"},
		{name: "web_search", category: "web", guard: "read_only"},
		{name: "ssh_host_inspect", category: "hosts", guard: "read_only"},
		{name: "ssh_task", category: "tasks", guard: "audited_control"},
		{name: "ssh_file_read", category: "remote_files", guard: "approval_required"},
		{name: "ssh_history", category: "history", guard: "read_only"},
	}
	for _, test := range tests {
		if got := category(test.name); got != test.category {
			t.Errorf("category(%q) = %q, want %q", test.name, got, test.category)
		}
		if got := guard(test.name); got != test.guard {
			t.Errorf("guard(%q) = %q, want %q", test.name, got, test.guard)
		}
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
