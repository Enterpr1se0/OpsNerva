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
	task := domain.Task{ID: "task_1", HostID: host.ID, Status: "running", StartedAt: time.Now().UTC()}
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
	if loaded.Status != "interrupted" || loaded.EndedAt.IsZero() || result.Stdout != "partial" || taskErr == "" {
		t.Fatalf("unexpected persisted task: %#v result=%#v error=%q", loaded, result, taskErr)
	}
}
