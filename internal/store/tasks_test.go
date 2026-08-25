package store

import (
	"context"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestTasksPersistAndActiveTasksBecomeInterrupted(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/tasks.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	host, err := st.UpsertHost(ctx, domain.Host{Name: "task-host", Address: "127.0.0.1", Port: 22, User: "ops", AuthType: "agent", CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	task := domain.Task{ID: "task_1", SessionID: "session_1", HostID: host.ID, Status: "running", Revision: 3, StartedAt: time.Now().UTC()}
	if err := st.UpsertTask(ctx, task, domain.ExecResult{Status: "running", Stdout: "partial"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.InterruptActiveTasks(ctx); err != nil {
		t.Fatal(err)
	}
	loaded, result, taskErr, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SessionID != task.SessionID || loaded.Revision != task.Revision+1 || loaded.Status != "interrupted" || loaded.EndedAt.IsZero() || result.Stdout != "partial" || taskErr == "" {
		t.Fatalf("unexpected persisted task: %#v result=%#v error=%q", loaded, result, taskErr)
	}
}

func TestTaskRevisionPreventsStaleCheckpointOverwrite(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/task-revisions.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	host, err := st.UpsertHost(ctx, domain.Host{Name: "task-revision-host", Address: "127.0.0.1", Port: 22, User: "ops", AuthType: "agent", CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC()
	stale := domain.Task{ID: "task_revision", SessionID: "session_revision", HostID: host.ID, Status: "running", Revision: 3, StartedAt: startedAt}
	terminal := stale
	terminal.Status = "cancelled"
	terminal.Revision = 4
	terminal.EndedAt = startedAt.Add(time.Second)
	if err := st.UpsertTask(ctx, stale, domain.ExecResult{Status: "running", Stdout: "checkpoint"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertTask(ctx, terminal, domain.ExecResult{Status: "cancelled", Stdout: "checkpoint"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertTask(ctx, stale, domain.ExecResult{Status: "running", Stdout: "stale"}, ""); err != nil {
		t.Fatal(err)
	}
	loaded, result, _, err := st.GetTask(ctx, terminal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != terminal.Revision || loaded.Status != terminal.Status || result.Status != "cancelled" || result.Stdout != "checkpoint" {
		t.Fatalf("stale checkpoint overwrote terminal task: task=%#v result=%#v", loaded, result)
	}
}
