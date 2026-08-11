package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/store"

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
	middleware, err := newContextSummarizationMiddleware(ctx, fixture, st, 1)
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
	middleware, err := newContextSummarizationMiddleware(ctx, fixture, st, 1)
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
