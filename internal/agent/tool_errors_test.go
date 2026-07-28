package agent

import (
	"context"
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

func TestToolCallTrackerNormalizesEmptyArguments(t *testing.T) {
	calls := []schema.ToolCall{{ID: "call1", Function: schema.FunctionCall{Name: "ssh_host_list", Arguments: ""}}}

	normalizing := newToolCallTracker("ws1", true)
	normalizing.add(calls)
	if captured := normalizing.take("call1", "ssh_host_list"); captured == nil || captured.Arguments != "{}" {
		t.Fatalf("normalizing tracker should record {} for empty arguments, got %+v", captured)
	}

	passthrough := newToolCallTracker("ws1", false)
	passthrough.add(calls)
	if captured := passthrough.take("call1", "ssh_host_list"); captured == nil || captured.Arguments != "" {
		t.Fatalf("passthrough tracker should keep raw arguments, got %+v", captured)
	}
}
