package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
)

func TestCurrentDateTimeMiddlewareInjectsFreshTimeEachRun(t *testing.T) {
	current := time.Date(2026, time.August, 31, 14, 20, 0, 0, time.FixedZone("CST", 8*60*60))
	middleware := newCurrentDateTimeMiddleware(func() time.Time { return current })

	_, first, err := middleware.BeforeAgent(context.Background(), &adk.ChatModelAgentContext{Instruction: "base"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Instruction, "Current time: 2026-08-31 14:20 +08:00") {
		t.Fatalf("first runtime prompt has the wrong date and time: %q", first.Instruction)
	}

	current = current.Add(90 * time.Minute)
	_, second, err := middleware.BeforeAgent(context.Background(), &adk.ChatModelAgentContext{Instruction: "base"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.Instruction, "Current time: 2026-08-31 15:50 +08:00") {
		t.Fatalf("second runtime prompt did not refresh the date and time: %q", second.Instruction)
	}
	if strings.Contains(first.Instruction, "15:50") {
		t.Fatalf("first runtime prompt changed after the clock advanced: %q", first.Instruction)
	}
}

func TestCurrentDateTimeMiddlewareRejectsMissingAgentContext(t *testing.T) {
	_, _, err := newCurrentDateTimeMiddleware(time.Now).BeforeAgent(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "Agent context is nil") {
		t.Fatalf("missing Agent context error = %v", err)
	}
}
