package domain

import (
	"encoding/json"
	"testing"
)

func TestOptionalZeroTimesAreOmitted(t *testing.T) {
	tests := []struct {
		name  string
		value any
		key   string
	}{
		{name: "execution result", value: ExecResult{Status: "running"}, key: "completed_at"},
		{name: "SSH shell", value: SSHShell{Status: "running"}, key: "ended_at"},
		{name: "run", value: Run{Status: "running"}, key: "completed_at"},
		{name: "approval", value: Approval{Status: "pending"}, key: "decided_at"},
		{name: "task", value: Task{Status: "running"}, key: "ended_at"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if _, exists := fields[test.key]; exists {
				t.Fatalf("zero %s was serialized: %s", test.key, encoded)
			}
		})
	}
}

func TestDefaultWorkspaceShellModeUsesHostOutsideLinux(t *testing.T) {
	tests := map[string]string{
		"linux":   WorkspaceShellModeSandbox,
		"windows": WorkspaceShellModeHost,
		"darwin":  WorkspaceShellModeHost,
	}
	for goos, expected := range tests {
		if actual := DefaultWorkspaceShellMode(goos); actual != expected {
			t.Fatalf("default Workspace Shell mode for %s = %q, want %q", goos, actual, expected)
		}
	}
}
