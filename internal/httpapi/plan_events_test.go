package httpapi

import (
	"encoding/json"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"golang.org/x/net/websocket"
)

func TestApplicationWebSocketPushesCompletedPlan(t *testing.T) {
	ctx, st, connection := openApplicationStateSocket(t)
	const sessionID = "plan-state-delta"
	if _, err := st.CreateChatSession(ctx, sessionID, ""); err != nil {
		t.Fatal(err)
	}
	if err := websocket.JSON.Send(connection, applicationWebSocketCommand{Type: "subscribe", Topics: []string{"chat_state"}, SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	initial := receiveApplicationEvent(t, connection)
	var state map[string]json.RawMessage
	if err := json.Unmarshal(initial.Data, &state); err != nil {
		t.Fatal(err)
	}
	if string(state["plan"]) != "null" {
		t.Fatalf("initial plan: %s", initial.Data)
	}
	if _, err := st.ReplaceAgentPlan(ctx, domain.AgentPlan{SessionID: sessionID, Goal: "Check", Status: "active", Steps: []domain.AgentPlanStep{{Number: 1, Title: "Inspect", Status: "in_progress"}}}); err != nil {
		t.Fatal(err)
	}
	created := receiveApplicationEvent(t, connection)
	if created.Mode != "delta" {
		t.Fatalf("create event: %#v", created)
	}
	if _, err := st.TransitionAgentPlanStep(ctx, sessionID, 1, "completed"); err != nil {
		t.Fatal(err)
	}
	completed := receiveApplicationEvent(t, connection)
	var payload struct {
		Plan *domain.AgentPlan `json:"plan"`
	}
	if err := json.Unmarshal(completed.Data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Plan == nil || payload.Plan.Status != "completed" || len(payload.Plan.Steps) != 1 {
		t.Fatalf("completed plan was cleared: %s", completed.Data)
	}
}
