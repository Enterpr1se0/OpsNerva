package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/agenttool"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/toolresult"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

func TestNormalizeEmptyToolArguments(t *testing.T) {
	var got string
	endpoint := normalizeEmptyToolArguments(func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
		got = input.Arguments
		return &compose.ToolOutput{Result: "ok"}, nil
	})
	for _, test := range []struct{ in, want string }{
		{"", "{}"},
		{"   ", "{}"},
		{"{}", "{}"},
		{`{"host_id":"h1"}`, `{"host_id":"h1"}`},
	} {
		if _, err := endpoint(context.Background(), &compose.ToolInput{Name: "ssh_host_list", Arguments: test.in}); err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Fatalf("arguments %q normalized to %q, want %q", test.in, got, test.want)
		}
	}
}

func TestTypedToolInputErrorIsReportedAsValidationFailure(t *testing.T) {
	failure := toolresult.FailureFromError("ssh_tunnel", agenttool.InvalidInput("action=list does not accept host_id"))
	if failure.OK || failure.Code != "validation_failed" || failure.Retryable || !strings.Contains(failure.Message, "host_id") {
		t.Fatalf("unexpected typed input failure: %#v", failure)
	}
}

func TestAgentHostAccessErrorIsReportedAsDenial(t *testing.T) {
	failure := toolresult.FailureFromError("ssh_exec", service.ErrAgentHostAccessDenied)
	if failure.OK || failure.Code != "denied" || failure.Retryable {
		t.Fatalf("unexpected Agent host access failure: %#v", failure)
	}
}

func TestStructuredToolInputErrorPreservesCorrectionDetails(t *testing.T) {
	failure := toolresult.FailureFromError("ssh_shell", agenttool.StructuredInputError(
		"action=input received unsupported fields: host_id",
		domain.ToolValidationDetails{
			Action: "input", AllowedFields: []string{"action", "shell_id", "input", "submit", "reason"},
			GotFields: []string{"action", "host_id"}, UnexpectedFields: []string{"host_id"},
			Example: map[string]any{"action": "input", "shell_id": "shell_xxx", "input": "whoami", "submit": true},
		},
	))
	if failure.Code != "validation_failed" || failure.Validation == nil {
		t.Fatalf("structured validation details were lost: %#v", failure)
	}
	if failure.Validation.Action != "input" || len(failure.Validation.UnexpectedFields) != 1 ||
		failure.Validation.UnexpectedFields[0] != "host_id" {
		t.Fatalf("unexpected validation details: %#v", failure.Validation)
	}
}

func TestSSHShellCredentialPromptRequiresPrivateOperatorInput(t *testing.T) {
	failure := toolresult.FailureFromError("ssh_shell", &shellCredentialPromptTestError{})
	if failure.OK || failure.Code != "operator_input_required" || failure.Retryable {
		t.Fatalf("unexpected credential prompt failure: %#v", failure)
	}
	if !strings.Contains(failure.NextAction, "private Web terminal") || !strings.Contains(failure.NextAction, "retry the credential") {
		t.Fatalf("credential prompt next action is unclear: %#v", failure)
	}
}

type shellCredentialPromptTestError struct{}

func (*shellCredentialPromptTestError) Error() string {
	return "the remote terminal is requesting a credential; wait for the operator to use the private Web terminal input"
}

func TestToolCallActivityIsPublishedBeforeExecution(t *testing.T) {
	var activities []toolCallActivity
	notified := false
	executed := false
	ctx := withToolActivityNotifier(context.Background(), func(value toolCallActivity) {
		if len(activities) == 0 && executed {
			t.Fatal("tool activity was published after execution started")
		}
		notified = true
		activities = append(activities, value)
	})
	endpoint := normalizeToolCallErrors(nil, func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
		executed = true
		if !notified {
			t.Fatal("tool endpoint started before its activity was published")
		}
		return &compose.ToolOutput{Result: "ok"}, nil
	})
	input := &compose.ToolInput{CallID: "call-live", Name: "ssh_exec", Arguments: `{"host_id":"host-1","program":"uptime"}`}
	if _, err := endpoint(ctx, input); err != nil {
		t.Fatal(err)
	}
	if len(activities) != 2 || activities[0].Status != domain.ChatToolCallRunning || activities[1].Status != domain.ChatToolCallCompleted {
		t.Fatalf("published activities = %#v", activities)
	}
	if activities[0].CallID != input.CallID || activities[0].Name != input.Name || activities[0].Arguments != input.Arguments {
		t.Fatalf("published start activity = %#v", activities[0])
	}
}

func TestCompletedToolActivityTreatsStartedResourcesAsTerminalCalls(t *testing.T) {
	status, _, _ := completedToolActivity(&compose.ToolOutput{Result: `{"status":"running","run_id":"run-background","task_id":"task-one"}`}, nil)
	if status != domain.ChatToolCallCompleted {
		t.Fatalf("background start lifecycle status = %q", status)
	}
	status, _, _ = completedToolActivity(&compose.ToolOutput{Result: `{"ok":false,"status":"unknown","code":"outcome_unknown"}`}, nil)
	if status != domain.ChatToolCallUnknown {
		t.Fatalf("unknown lifecycle status = %q", status)
	}
}

func TestToolCallTrackerNormalizesEmptyArguments(t *testing.T) {
	calls := []schema.ToolCall{{ID: "call1", Function: schema.FunctionCall{Name: "ssh_host_list", Arguments: ""}}}

	normalizing := newToolCallTracker("ws1", true)
	normalizing.add(calls)
	if captured := normalizing.take("call1", "ssh_host_list"); captured == nil || captured.CallID != "call1" || captured.Arguments != "{}" {
		t.Fatalf("normalizing tracker should record {} for empty arguments, got %+v", captured)
	}

	passthrough := newToolCallTracker("ws1", false)
	passthrough.add(calls)
	if captured := passthrough.take("call1", "ssh_host_list"); captured == nil || captured.Arguments != "" {
		t.Fatalf("passthrough tracker should keep raw arguments, got %+v", captured)
	}
}
