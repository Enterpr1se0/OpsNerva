package agent

import (
	"context"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/store"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

func TestGenerateSessionTitleParsesAndNormalizesModelOutput(t *testing.T) {
	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{{
		adk.EventFromMessage(schema.AssistantMessage(`{"title":"磁盘空间排查。"}`, nil), nil, schema.Assistant, ""),
	}}}
	title, err := generateSessionTitle(context.Background(), runner, sessionTitleInput{Request: "帮我检查服务器磁盘空间"})
	if err != nil {
		t.Fatal(err)
	}
	if title != "磁盘空间排查" {
		t.Fatalf("title = %q", title)
	}
}

func TestQueryGeneratesTitleBeforeDone(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	runner := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{{
		adk.EventFromMessage(schema.AssistantMessage("磁盘空间正常。", nil), nil, schema.Assistant, ""),
	}}}
	titleGenerator := &scriptedAgentRunner{attempts: [][]*adk.AgentEvent{{
		adk.EventFromMessage(schema.AssistantMessage(`{"title":"磁盘空间检查"}`, nil), nil, schema.Assistant, ""),
	}}}
	runtime := &Runtime{runner: runner, titleGenerator: titleGenerator, store: st}
	var events []Event
	if _, err := runtime.Query(ctx, "session-title", "检查磁盘空间", func(event Event) { events = append(events, event) }); err != nil {
		t.Fatal(err)
	}
	session, err := st.GetChatSession(ctx, "session-title")
	if err != nil {
		t.Fatal(err)
	}
	if session.Title != "磁盘空间检查" || !session.TitleSet {
		t.Fatalf("session title = %q, set=%v", session.Title, session.TitleSet)
	}
	titleIndex, doneIndex := -1, -1
	for index, event := range events {
		switch event.Type {
		case "title":
			titleIndex = index
		case "done":
			doneIndex = index
		}
	}
	if titleIndex < 0 || doneIndex < 0 || titleIndex > doneIndex {
		t.Fatalf("title and done event order = %#v", events)
	}
}
