package agent

import (
	"context"
	"strings"
	"testing"

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
