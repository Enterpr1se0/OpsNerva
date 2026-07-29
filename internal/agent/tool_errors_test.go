package agent

import (
	"context"
	"strings"
	"testing"

	"eino-ops-agent/internal/domain"

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
	failure := toolFailureFromError("ssh_tunnel", invalidToolInput("action=list does not accept host_id"))
	if failure.OK || failure.Code != "validation_failed" || failure.Retryable || !strings.Contains(failure.Message, "host_id") {
		t.Fatalf("unexpected typed input failure: %#v", failure)
	}
}

func TestStructuredToolInputErrorPreservesCorrectionDetails(t *testing.T) {
	failure := toolFailureFromError("ssh_shell", invalidStructuredToolInput(
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
	failure := toolFailureFromError("ssh_shell", &shellCredentialPromptTestError{})
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
	var activity toolCallActivity
	notified := false
	executed := false
	ctx := withToolActivityNotifier(context.Background(), func(value toolCallActivity) {
		if executed {
			t.Fatal("tool activity was published after execution started")
		}
		notified = true
		activity = value
	})
	endpoint := normalizeToolCallErrors(func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
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
	if activity.CallID != input.CallID || activity.Name != input.Name || activity.Arguments != input.Arguments {
		t.Fatalf("published activity = %#v", activity)
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
