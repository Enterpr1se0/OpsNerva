package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestAgentPlanCommitAndConcurrentTransition(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.ReplaceAgentPlan(ctx, domain.AgentPlan{SessionID: "s", Goal: "Goal", Status: "active", Steps: []domain.AgentPlanStep{{Number: 1, Title: "A", Status: "in_progress"}, {Number: 2, Title: "B", Status: "pending"}}}); err != nil {
		t.Fatal(err)
	}
	changes := make(chan domain.AgentPlan, 2)
	unsubscribe := st.SubscribeChanges(func(change Change) {
		if change.Topic != ChangeChatState || change.SessionID != "s" {
			return
		}
		plan, err := st.GetAgentPlan(ctx, "s")
		if err != nil {
			t.Error(err)
			return
		}
		changes <- plan
	})
	defer unsubscribe()
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.TransitionAgentPlanStep(ctx, "s", 1, "completed")
			results <- err
		}()
	}
	wg.Wait()
	first, second := <-results, <-results
	if (first == nil) == (second == nil) {
		t.Fatalf("want one committed transition, got %v / %v", first, second)
	}
	select {
	case plan := <-changes:
		if plan.Steps[0].Status != "completed" || plan.CurrentStep().Number != 2 {
			t.Fatalf("event before committed plan: %#v", plan)
		}
	case <-time.After(time.Second):
		t.Fatal("missing plan change event")
	}
	select {
	case <-changes:
		t.Fatal("failed update published an event")
	default:
	}
	if _, err := st.ReviseAgentPlanRemaining(ctx, "s", nil); !errors.Is(err, domain.ErrInvalidAgentPlan) {
		t.Fatalf("store bypassed domain validation: %v", err)
	}
	plan, err := st.GetAgentPlan(ctx, "s")
	if err != nil || plan.CurrentStep().Number != 2 || len(plan.Steps) != 2 {
		t.Fatalf("failed revision changed stored plan: %#v %v", plan, err)
	}
	select {
	case <-changes:
		t.Fatal("failed revision published an event")
	default:
	}
}

func TestAgentPlanReplaceDoesNotMutateInput(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	plan, err := domain.NewAgentPlan("Goal", []string{"Inspect", "Verify"})
	if err != nil {
		t.Fatal(err)
	}
	plan.SessionID = "s"
	stored, err := st.ReplaceAgentPlan(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.CreatedAt.IsZero() || !plan.Steps[0].UpdatedAt.IsZero() || stored.CreatedAt.IsZero() || !stored.UpdatedAt.Equal(stored.Steps[0].UpdatedAt) {
		t.Fatalf("persistence modified caller snapshot: %#v / %#v", plan, stored)
	}
}

func TestMigrateAgentTaskPlansPreservesProgress(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE agent_task_files(session_id TEXT,file_path TEXT,content TEXT,created_at TEXT,updated_at TEXT,PRIMARY KEY(session_id,file_path))`)
	if err != nil {
		t.Fatal(err)
	}
	stamp := "2026-09-10T01:00:00Z"
	for _, row := range []struct{ path, subject, status string }{
		{"agent-tasks/1.json", "First pending", "pending"},
		{"agent-tasks/2.json", "Finished", "completed"},
		{"agent-tasks/10.json", "Last pending", "in_progress"},
	} {
		content, _ := json.Marshal(map[string]any{"subject": row.subject, "description": "kept description", "status": row.status, "blockedBy": []string{"99"}})
		if _, err := db.Exec(`INSERT INTO agent_task_files VALUES(?,?,?,?,?)`, "s", row.path, string(content), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO agent_task_files VALUES('s','agent-tasks/.highwatermark','10',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := st.GetAgentPlan(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 3 || plan.Steps[0].Title != "Finished" || plan.Steps[0].Status != "completed" || plan.CurrentStep().Title != "First pending" || plan.Steps[2].Status != "pending" || plan.Steps[2].Description != "kept description" {
		t.Fatalf("migrated plan: %#v", plan)
	}
	if _, err := st.TransitionAgentPlanStep(ctx, "s", 2, "completed"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	plan, err = st.GetAgentPlan(ctx, "s")
	if err != nil || plan.CurrentStep().Number != 3 {
		t.Fatalf("migration repeated on restart: %#v %v", plan, err)
	}
}

func TestMigrateOriginalBlockedPlan(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "original.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// The original schema had no description column and allowed blocked steps.
	_, err = db.Exec(`CREATE TABLE agent_plans(session_id TEXT PRIMARY KEY,goal TEXT NOT NULL,status TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE agent_plan_steps(session_id TEXT NOT NULL,step_number INTEGER NOT NULL,title TEXT NOT NULL,status TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(session_id,step_number),FOREIGN KEY(session_id) REFERENCES agent_plans(session_id) ON DELETE CASCADE);
INSERT INTO agent_plans VALUES('s','Original goal','blocked','2026-08-08T00:00:00Z','2026-08-08T01:00:00Z');
INSERT INTO agent_plan_steps VALUES('s',1,'Inspect','completed','2026-08-08T01:00:00Z'),('s',2,'Repair','blocked','2026-08-08T01:00:00Z')`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	plan, err := st.GetAgentPlan(ctx, "s")
	if err != nil || plan.Status != "active" || plan.CurrentStep() == nil || plan.CurrentStep().Number != 2 || plan.Steps[0].Status != "completed" || plan.Steps[0].Description != "" {
		t.Fatalf("original plan did not resume: %#v %v", plan, err)
	}
	if _, err := st.TransitionAgentPlanStep(ctx, "s", 2, "completed"); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateAgentPlansRollsBackOnFailure(t *testing.T) {
	for _, kind := range []string{"invalid_json", "write_failure"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			st, err := Open(ctx, filepath.Join(t.TempDir(), "rollback.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			original, err := st.ReplaceAgentPlan(ctx, domain.AgentPlan{SessionID: "s", Goal: "Original", Status: "blocked", Steps: []domain.AgentPlanStep{{Number: 1, Title: "Original step", Status: "blocked"}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SetAgentToolEnabled(ctx, "TaskCreate", false); err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.Exec(`CREATE TABLE agent_task_files(session_id TEXT,file_path TEXT,content TEXT,created_at TEXT,updated_at TEXT,PRIMARY KEY(session_id,file_path))`); err != nil {
				t.Fatal(err)
			}
			content := `{"subject":"Migrated","status":"pending"}`
			if kind == "invalid_json" {
				content = `{"subject":`
			} else if _, err := st.db.Exec(`CREATE TRIGGER reject_migrated_step BEFORE INSERT ON agent_plan_steps WHEN NEW.title='Migrated' BEGIN SELECT RAISE(ABORT,'migration write failed'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.Exec(`INSERT INTO agent_task_files VALUES('s','agent-tasks/1.json',?,'2026-09-10T00:00:00Z','2026-09-10T00:00:00Z')`, content); err != nil {
				t.Fatal(err)
			}
			if err := st.migrateAgentPlans(ctx); err == nil {
				t.Fatal("expected migration failure")
			}
			plan, err := st.GetAgentPlan(ctx, "s")
			if err != nil || plan.Goal != original.Goal || plan.Status != "blocked" || len(plan.Steps) != 1 || plan.Steps[0].Title != "Original step" || plan.Steps[0].Status != "blocked" || !plan.UpdatedAt.Equal(original.UpdatedAt) {
				t.Fatalf("migration partially changed original plan: %#v %v", plan, err)
			}
			var saved string
			if err := st.db.QueryRow(`SELECT content FROM agent_task_files WHERE session_id='s'`).Scan(&saved); err != nil || saved != content {
				t.Fatalf("legacy task lost: %q %v", saved, err)
			}
			states, err := st.AgentToolStates(ctx)
			if _, exists := states["TaskCreate"]; err != nil || !exists {
				t.Fatalf("settings removed before migration committed: %#v %v", states, err)
			}
		})
	}
}
