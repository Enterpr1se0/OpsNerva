package agent

import (
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"

	"github.com/cloudwego/eino/schema"
)

func TestInjectControlPlaneContexts(t *testing.T) {
	base := []*schema.Message{
		schema.UserMessage("earlier request"),
		schema.AssistantMessage("earlier answer", nil),
		schema.UserMessage("list hosts"),
	}
	workspaceContent, err := workspaceContextContent(modelWorkspaceState{ID: "ws1", Access: "read_write", Bound: true})
	if err != nil {
		t.Fatal(err)
	}
	taskContent, err := agentTaskContextContent(domain.AgentTaskList{Items: []domain.AgentTask{{ID: "1", Subject: "Inspect", Status: "in_progress"}}})
	if err != nil {
		t.Fatal(err)
	}
	contents := []string{workspaceContent, taskContent}

	inline, inlineBytes := injectControlPlaneContexts(base, contents, true)
	if len(inline) != len(base) {
		t.Fatalf("inline injection changed message count: got %d, want %d", len(inline), len(base))
	}
	for _, message := range inline {
		if message.Role == schema.System {
			t.Fatal("inline injection produced a system message")
		}
	}
	last := inline[len(inline)-1]
	if last.Role != schema.User || !strings.HasSuffix(last.Content, "list hosts") {
		t.Fatalf("contexts were not folded into the trailing user message: %q", last.Content)
	}
	if got := strings.Count(last.Content, "<system-reminder>"); got != 2 {
		t.Fatalf("expected two reminder blocks, got %d", got)
	}
	if inlineBytes != len(last.Content)-len("list hosts") {
		t.Fatalf("inline context byte count = %d, want %d", inlineBytes, len(last.Content)-len("list hosts"))
	}
	workspaceAt := strings.Index(last.Content, workspaceContent)
	taskAt := strings.Index(last.Content, taskContent)
	if workspaceAt < 0 || taskAt < 0 || workspaceAt > taskAt {
		t.Fatalf("blocks must keep injection order (workspace before tasks): workspace=%d tasks=%d", workspaceAt, taskAt)
	}
	if base[len(base)-1].Content != "list hosts" {
		t.Fatal("inline injection mutated the caller's message slice")
	}

	system, systemBytes := injectControlPlaneContexts(base, contents, false)
	if len(system) != len(base)+2 {
		t.Fatalf("system injection should add one message per context, got %d", len(system))
	}
	if system[1].Role != schema.Assistant || system[2].Role != schema.System || system[3].Role != schema.System || system[4].Content != "list hosts" {
		t.Fatal("system messages must sit between history and the trailing user message")
	}
	if system[2].Content != workspaceContent || system[3].Content != taskContent {
		t.Fatal("system messages must keep injection order (workspace before tasks)")
	}
	if systemBytes != len(workspaceContent)+len(taskContent) {
		t.Fatalf("system context byte count = %d, want %d", systemBytes, len(workspaceContent)+len(taskContent))
	}

	if got, gotBytes := injectControlPlaneContexts(base, nil, true); len(got) != len(base) || gotBytes != 0 {
		t.Fatal("empty contents must be a no-op")
	}
}

func TestInjectControlPlaneContextsMultimodal(t *testing.T) {
	multimodal := multimodalUserMessage("inspect this screenshot", []domain.ChatAttachment{{MIMEType: "image/png", Data: []byte{1, 2, 3}}})
	base := []*schema.Message{multimodal}
	content, err := workspaceContextContent(modelWorkspaceState{ID: "ws1", Bound: true})
	if err != nil {
		t.Fatal(err)
	}

	inline, inlineBytes := injectControlPlaneContexts(base, []string{content}, true)
	last := inline[len(inline)-1]
	if len(last.UserInputMultiContent) != 3 {
		t.Fatalf("expected reminder + text + image parts, got %d parts", len(last.UserInputMultiContent))
	}
	first := last.UserInputMultiContent[0]
	if first.Type != schema.ChatMessagePartTypeText || !strings.Contains(first.Text, "<system-reminder>") {
		t.Fatalf("reminder block was not prepended as a text part: %+v", first)
	}
	if inlineBytes != len(first.Text) {
		t.Fatalf("multimodal context byte count = %d, want %d", inlineBytes, len(first.Text))
	}
	if len(base[0].UserInputMultiContent) != 2 {
		t.Fatal("inline injection mutated the caller's multimodal message")
	}
}
