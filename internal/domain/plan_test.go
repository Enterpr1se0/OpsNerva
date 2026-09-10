package domain

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAgentPlanCreationRules(t *testing.T) {
	titles := []string{" Inspect ", "Verify"}
	plan, err := NewAgentPlan(" Repair service ", titles)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Goal != "Repair service" || plan.Status != "active" || plan.CurrentStep().Title != "Inspect" || plan.Steps[1].Number != 2 || plan.Steps[1].Status != "pending" {
		t.Fatalf("created plan: %#v", plan)
	}
	if titles[0] != " Inspect " || plan.SessionID != "" || !plan.CreatedAt.IsZero() {
		t.Fatal("domain construction changed input or introduced application state")
	}
	for _, input := range []struct {
		goal   string
		titles []string
	}{
		{" ", titles},
		{strings.Repeat("界", 501), titles},
		{"Goal", []string{"Only one"}},
		{"Goal", []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}},
		{"Goal", []string{"Same", " SAME "}},
		{"Goal", []string{"Valid", " "}},
		{"Goal", []string{"Valid", strings.Repeat("界", 241)}},
	} {
		if _, err := NewAgentPlan(input.goal, input.titles); !errors.Is(err, ErrInvalidAgentPlan) {
			t.Fatalf("expected domain validation for %#v, got %v", input, err)
		}
	}
}

func TestAgentPlanTransitionsPreserveSnapshots(t *testing.T) {
	plan, err := NewAgentPlan("Repair service", []string{"Inspect", "Repair", "Verify"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	plan.SessionID, plan.CreatedAt = "s", now.Add(-time.Hour)
	plan.Steps[0].Description = "Preserved history"
	before := plan
	for _, input := range []struct {
		number int
		status string
	}{{2, "completed"}, {0, "completed"}, {1, "blocked"}, {1, "in_progress"}} {
		failed, err := plan.TransitionStep(input.number, input.status, now)
		if !errors.Is(err, ErrInvalidAgentPlan) || !reflect.DeepEqual(failed, before) {
			t.Fatalf("invalid transition changed plan: %#v %v", failed, err)
		}
	}
	advanced, err := plan.TransitionStep(1, "completed", now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan, before) || plan.Steps[0].Status != "in_progress" || plan.Steps[1].Status != "pending" {
		t.Fatal("transition mutated the previous snapshot")
	}
	if advanced.CurrentStep().Number != 2 || !advanced.UpdatedAt.Equal(now) || !advanced.Steps[0].UpdatedAt.Equal(now) || !advanced.Steps[1].UpdatedAt.Equal(now) || !advanced.CreatedAt.Equal(plan.CreatedAt) {
		t.Fatalf("advanced plan: %#v", advanced)
	}
	revised, err := advanced.ReviseRemaining([]string{" Adjust ", "Retest"}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if advanced.Steps[1].Title != "Repair" || advanced.Steps[2].Title != "Verify" {
		t.Fatal("revision overwrote the previous slice backing array")
	}
	if !reflect.DeepEqual(revised.Steps[0], advanced.Steps[0]) || revised.CurrentStep().Title != "Adjust" || revised.SessionID != "s" || !revised.UpdatedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("revision lost history: %#v", revised)
	}
	for _, titles := range [][]string{nil, {"duplicate", "DUPLICATE"}} {
		failed, err := revised.ReviseRemaining(titles, now)
		if !errors.Is(err, ErrInvalidAgentPlan) || !reflect.DeepEqual(failed, revised) {
			t.Fatalf("invalid revision changed plan: %#v %v", failed, err)
		}
	}
	skipped, err := revised.TransitionStep(2, "skipped", now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	completed, err := skipped.TransitionStep(3, "completed", now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "completed" || completed.CurrentStep() != nil || completed.Steps[1].Status != "skipped" || revised.Steps[1].Status != "in_progress" {
		t.Fatalf("completion: %#v", completed)
	}
	if _, err := completed.TransitionStep(3, "completed", now); !errors.Is(err, ErrInvalidAgentPlan) {
		t.Fatalf("repeated completed transition: %v", err)
	}
	if _, err := completed.ReviseRemaining([]string{"Redo"}, now); !errors.Is(err, ErrInvalidAgentPlan) {
		t.Fatalf("completed history was revised: %v", err)
	}
}
