package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/store"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type summaryFixtureModel struct {
	inputs [][]*schema.Message
	err    error
}

func (m *summaryFixtureModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.inputs = append(m.inputs, input)
	if m.err != nil {
		return nil, m.err
	}
	return schema.AssistantMessage("<analysis>private</analysis><summary>durable compressed memory</summary>", nil), nil
}

func (m *summaryFixtureModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func TestModelContextUsesDurableSummaryAndAdvancesCompleteTurnBoundary(t *testing.T) {
	history := make([]domain.ChatMessage, 0, 10)
	for turn := 1; turn <= 5; turn++ {
		history = append(history,
			domain.ChatMessage{ID: fmt.Sprintf("user-%d", turn), Role: "user", Content: fmt.Sprintf("question %d", turn), Status: "completed"},
			domain.ChatMessage{ID: fmt.Sprintf("assistant-%d", turn), Role: "assistant", Content: fmt.Sprintf("answer %d", turn), Status: "completed"},
		)
	}
	summary := domain.ChatContextSummary{
		SessionID: "session", Summary: "turn one summary", ThroughMessageID: "assistant-1",
	}
	messages, stats := buildMultimodalModelContextWithSummaryForProvider(history,
		domain.ChatMessage{Role: "user", Content: "current question"}, "", summary)
	if len(messages) != 10 {
		t.Fatalf("model messages = %d, want summary + four turns + current", len(messages))
	}
	if messages[0].Role != schema.System || !strings.Contains(messages[0].Content, "turn one summary") {
		t.Fatalf("durable summary message = %#v", messages[0])
	}
	if messages[1].Content != "question 2" || messages[len(messages)-1].Content != "current question" {
		t.Fatalf("summary boundary was not applied: first=%q last=%q", messages[1].Content, messages[len(messages)-1].Content)
	}
	if stats.CompressionBoundaryID != "assistant-3" {
		t.Fatalf("next compression boundary = %q", stats.CompressionBoundaryID)
	}
}

func TestContextSummaryIsIgnoredWhenBoundaryIsMissing(t *testing.T) {
	history := []domain.ChatMessage{
		{ID: "user-1", Role: "user", Content: "question", Status: "completed"},
		{ID: "assistant-1", Role: "assistant", Content: "answer", Status: "completed"},
	}
	messages, _ := buildMultimodalModelContextWithSummaryForProvider(history,
		domain.ChatMessage{Role: "user", Content: "current"}, "", domain.ChatContextSummary{Summary: "stale", ThroughMessageID: "missing"})
	if messages[0].Role != schema.User || messages[0].Content != "question" {
		t.Fatalf("stale summary hid transcript: %#v", messages)
	}
}

func TestAutoContextCompressionTriggerUsesKnownModelWindow(t *testing.T) {
	tests := []struct {
		name          string
		contextWindow int
		percent       int
		want          int
	}{
		{name: "unknown window uses fallback", contextWindow: 0, percent: 70, want: contextCompressionFallbackTokens},
		{name: "large window is not capped at fallback", contextWindow: 500_000, percent: 70, want: 350_000},
		{name: "smaller known window can be below fallback", contextWindow: 128_000, percent: 70, want: 89_600},
		{name: "configured percentage is respected", contextWindow: 200_000, percent: 90, want: 180_000},
		{name: "invalid percentage uses default", contextWindow: 500_000, percent: 1, want: 350_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := autoContextCompressionTrigger(tt.contextWindow, tt.percent); got != tt.want {
				t.Fatalf("autoContextCompressionTrigger(%d, %d) = %d, want %d", tt.contextWindow, tt.percent, got, tt.want)
			}
		})
	}
}

func TestEstimateContextTokensIncludesToolArgumentsAndSchema(t *testing.T) {
	base, err := estimateContextTokens([]*schema.Message{schema.UserMessage("inspect")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	arguments := strings.Repeat("a", 8<<10)
	messages := []*schema.Message{
		schema.UserMessage("inspect"),
		schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-large", Function: schema.FunctionCall{Name: "ssh_run_script", Arguments: arguments},
		}}),
	}
	withArguments, err := estimateContextTokens(messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	if withArguments-base < len(arguments)/4 {
		t.Fatalf("tool arguments were not fully estimated: base=%d with_arguments=%d", base, withArguments)
	}
	tools := []*schema.ToolInfo{{
		Name: "fixture_tool",
		Desc: "fixture",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"content": {Type: schema.String, Desc: strings.Repeat("schema", 1024), Required: true},
		}),
	}}
	withSchema, err := estimateContextTokens(messages, tools)
	if err != nil {
		t.Fatal(err)
	}
	if withSchema <= withArguments {
		t.Fatalf("tool schema was not estimated: without=%d with=%d", withArguments, withSchema)
	}
}

func TestContextSummarizerUpdatesThresholdAfterWindowDetection(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "summary-threshold.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fixture := &summaryFixtureModel{}
	middleware, err := newContextSummarizationMiddleware(ctx, fixture, st, 0, 70)
	if err != nil {
		t.Fatal(err)
	}
	window, percent, trigger := middleware.compressionThreshold()
	if window != 0 || percent != 70 || trigger != contextCompressionFallbackTokens {
		t.Fatalf("initial threshold = window %d, percent %d, trigger %d", window, percent, trigger)
	}
	if got := middleware.updateContextWindow(500_000); got != 350_000 {
		t.Fatalf("updated trigger = %d, want 350000", got)
	}
	window, percent, trigger = middleware.compressionThreshold()
	if window != 500_000 || percent != 70 || trigger != 350_000 {
		t.Fatalf("updated threshold = window %d, percent %d, trigger %d", window, percent, trigger)
	}
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{
		schema.SystemMessage("agent instruction"),
		schema.UserMessage("old question"), schema.AssistantMessage(strings.Repeat("x", 400_000), nil),
		schema.UserMessage("recent one"), schema.AssistantMessage("answer one", nil),
		schema.UserMessage("recent two"), schema.AssistantMessage("answer two", nil),
		schema.UserMessage("current"),
	}}
	runCtx := withContextCompressionState(ctx, &contextCompressionRunState{
		sessionID: "session", boundaryID: "boundary", trigger: "auto", model: "fixture", hasCurrent: true,
	})
	_, next, err := middleware.BeforeModelRewriteState(runCtx, state, &adk.ModelContext{})
	if err != nil {
		t.Fatal(err)
	}
	if next != state || len(fixture.inputs) != 0 {
		t.Fatal("context below the detected-window threshold was compressed")
	}
}

func TestEinoContextSummarizerForcePersistsCleanSummary(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "summary-middleware.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateChatSession(ctx, "session", ""); err != nil {
		t.Fatal(err)
	}
	boundary, err := st.AppendPendingChatMessage(ctx, "session", "assistant", "old result")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetChatMessageStatus(ctx, boundary, "completed"); err != nil {
		t.Fatal(err)
	}
	fixture := &summaryFixtureModel{}
	middleware, err := newContextSummarizationMiddleware(ctx, fixture, st, 1, 70)
	if err != nil {
		t.Fatal(err)
	}
	run := &contextCompressionRunState{sessionID: "session", boundaryID: boundary, trigger: "manual", model: "fixture"}
	runCtx := withContextCompressionState(ctx, run)
	result, err := middleware.Force(runCtx, &adk.ChatModelAgentState{Messages: []*schema.Message{
		schema.SystemMessage("agent instruction"),
		schema.UserMessage("old question"), schema.AssistantMessage("old answer", nil),
		schema.UserMessage("recent one"), schema.AssistantMessage("answer one", nil),
		schema.UserMessage("recent two"), schema.AssistantMessage("answer two", nil),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Summary != "durable compressed memory" || strings.Contains(result.Summary.Summary, "private") {
		t.Fatalf("persisted summary = %#v", result.Summary)
	}
	if result.Summary.ThroughMessageID != boundary || result.Summary.Trigger != "manual" || result.Summary.Revision != 1 {
		t.Fatalf("summary metadata = %#v", result.Summary)
	}
	if len(fixture.inputs) != 1 || len(fixture.inputs[0]) < 3 {
		t.Fatalf("summarization model inputs = %#v", fixture.inputs)
	}
}

func TestAutomaticContextSummarizationFailsOpen(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "summary-fail-open.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fixture := &summaryFixtureModel{err: errors.New("summary unavailable")}
	middleware, err := newContextSummarizationMiddleware(ctx, fixture, st, 1, 70)
	if err != nil {
		t.Fatal(err)
	}
	original := &adk.ChatModelAgentState{Messages: []*schema.Message{
		schema.SystemMessage("agent instruction"),
		schema.UserMessage("old question"), schema.AssistantMessage("old answer", nil),
		schema.UserMessage("recent one"), schema.AssistantMessage("answer one", nil),
		schema.UserMessage("recent two"), schema.AssistantMessage("answer two", nil),
		schema.UserMessage("current"),
	}}
	runCtx := withContextCompressionState(ctx, &contextCompressionRunState{
		sessionID: "session", boundaryID: "boundary", trigger: "auto", hasCurrent: true,
	})
	_, next, err := middleware.BeforeModelRewriteState(runCtx, original, &adk.ModelContext{})
	if err != nil {
		t.Fatalf("automatic compression blocked the model call: %v", err)
	}
	if next != original {
		t.Fatal("failed automatic compression replaced the original state")
	}
}
