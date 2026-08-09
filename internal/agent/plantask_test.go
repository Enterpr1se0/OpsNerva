package agent

import (
	"context"
	"strings"
	"testing"

	"eino-ops-agent/internal/service"
	"eino-ops-agent/internal/store"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
)

func TestPlantaskMiddlewarePersistsFrameworkTasksBySession(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/tasks.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	handler, tools, err := newPlantaskMiddleware(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if handler == nil || len(tools) != 4 {
		t.Fatalf("handler=%v tools=%d", handler, len(tools))
	}
	byName := invokableToolsByName(t, ctx, tools)
	for _, name := range []string{"TaskCreate", "TaskGet", "TaskUpdate", "TaskList"} {
		if byName[name] == nil {
			t.Fatalf("framework task tool %s is missing", name)
		}
	}

	sessionCtx := service.WithSessionID(ctx, "session-a")
	if _, err := byName["TaskCreate"].InvokableRun(sessionCtx, `{"subject":"Inspect","description":"Inspect the service","activeForm":"Inspecting"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := byName["TaskCreate"].InvokableRun(sessionCtx, `{"subject":"Repair","description":"Repair the service","activeForm":"Repairing"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := byName["TaskUpdate"].InvokableRun(sessionCtx, `{"taskId":"1","status":"in_progress","addBlocks":["2"]}`); err != nil {
		t.Fatal(err)
	}

	tasks, err := st.ListAgentTasks(ctx, "session-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks.Items) != 2 || tasks.Items[0].ID != "1" || tasks.Items[0].Status != "in_progress" || len(tasks.Items[0].Blocks) != 1 || len(tasks.Items[1].BlockedBy) != 1 {
		t.Fatalf("persisted tasks = %#v", tasks)
	}
	isolated, err := byName["TaskList"].InvokableRun(service.WithSessionID(ctx, "session-b"), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(isolated, "No tasks found") {
		t.Fatalf("other session saw task state: %s", isolated)
	}

	if _, err := byName["TaskUpdate"].InvokableRun(sessionCtx, `{"taskId":"1","status":"completed"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := byName["TaskUpdate"].InvokableRun(sessionCtx, `{"taskId":"2","status":"in_progress"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := byName["TaskUpdate"].InvokableRun(sessionCtx, `{"taskId":"2","status":"completed"}`); err != nil {
		t.Fatal(err)
	}
	tasks, err = st.ListAgentTasks(ctx, "session-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks.Items) != 0 {
		t.Fatalf("framework did not clear the completed task list: %#v", tasks.Items)
	}
	created, err := byName["TaskCreate"].InvokableRun(sessionCtx, `{"subject":"Verify","description":"Verify the result","activeForm":"Verifying"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(created, "Task #3") {
		t.Fatalf("task high watermark was not preserved: %s", created)
	}
}

func TestPlantaskMiddlewareFiltersDisabledFrameworkTool(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/tasks.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	handler, _, err := newPlantaskMiddleware(ctx, st, map[string]bool{"TaskCreate": false})
	if err != nil {
		t.Fatal(err)
	}
	_, runCtx, err := handler.BeforeAgent(ctx, &adk.ChatModelAgentContext{})
	if err != nil {
		t.Fatal(err)
	}
	byName := invokableToolsByName(t, ctx, runCtx.Tools)
	if byName["TaskCreate"] != nil || len(byName) != 3 {
		t.Fatalf("filtered tools = %#v", byName)
	}
}

func invokableToolsByName(t *testing.T, ctx context.Context, tools []tool.BaseTool) map[string]tool.InvokableTool {
	t.Helper()
	result := make(map[string]tool.InvokableTool, len(tools))
	for _, candidate := range tools {
		info, err := candidate.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		invokable, ok := candidate.(tool.InvokableTool)
		if !ok {
			t.Fatalf("tool %s is not invokable", info.Name)
		}
		result[info.Name] = invokable
	}
	return result
}
