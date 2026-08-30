package agenttool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

type passthroughResultPolicy struct{}

func (passthroughResultPolicy) NormalizeExec(result domain.ExecResult, err error) (domain.ExecResult, error) {
	return result, err
}

func (passthroughResultPolicy) Value(_ context.Context, _ string, value any, err error) (any, error) {
	return value, err
}

func TestNormalizeTaskResultPreservesOperatorInterruption(t *testing.T) {
	ssh := NewSSH(SSHDependencies{Results: passthroughResultPolicy{}})
	task := domain.Task{
		ID:                  "task_rejected",
		RunID:               "run_rejected",
		Status:              "rejected",
		OperatorInstruction: "stop the test and only summarize existing results",
	}
	full, err := ssh.normalizeTaskResult(task, domain.ExecResult{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	result := ProjectExecResult(full)
	if result.Status != "rejected" || result.TaskID != task.ID || result.OperatorInstruction != task.OperatorInstruction {
		t.Fatalf("task status lost the operator interruption: %#v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"operator_instruction":"stop the test and only summarize existing results"`) {
		t.Fatalf("serialized result lost the operator instruction: %s", encoded)
	}
}

func TestNormalizeTaskResultPreservesFailureOutput(t *testing.T) {
	ssh := NewSSH(SSHDependencies{Results: passthroughResultPolicy{}})
	full, err := ssh.normalizeTaskResult(
		domain.Task{ID: "task_failed", RunID: "run_failed", Status: "failed"},
		domain.ExecResult{RunID: "run_failed", ExitCode: 1, Stderr: "sleep: missing operand"},
		"", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	result := ProjectExecResult(full)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || !strings.Contains(string(encoded), `"stderr":"sleep: missing operand"`) {
		t.Fatalf("failed task did not expose stderr: output=%#v json=%s", result, encoded)
	}
}
