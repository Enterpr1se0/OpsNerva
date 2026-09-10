// Package plan implements ordered work-plan rules without persistence or Agent dependencies.
package plan

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

var ErrInvalid = errors.New("invalid agent plan")

// New defines the work sequence. The application binds its session;
// persistence assigns creation timestamps when the plan is committed.
func New(goal string, titles []string) (domain.AgentPlan, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" || utf8.RuneCountInString(goal) > 500 {
		return domain.AgentPlan{}, fmt.Errorf("%w: goal must contain 1-500 characters", ErrInvalid)
	}
	normalized, err := normalizeTitles(titles, 2)
	if err != nil {
		return domain.AgentPlan{}, err
	}
	steps := make([]domain.AgentPlanStep, len(normalized))
	for i, title := range normalized {
		status := "pending"
		if i == 0 {
			status = "in_progress"
		}
		steps[i] = domain.AgentPlanStep{Number: i + 1, Title: title, Status: status}
	}
	return domain.AgentPlan{Goal: goal, Status: "active", Steps: steps}, nil
}

func CurrentStep(p domain.AgentPlan) *domain.AgentPlanStep {
	for i := range p.Steps {
		if p.Steps[i].Status == "in_progress" {
			return &p.Steps[i]
		}
	}
	return nil
}

// TransitionStep returns a new snapshot without mutating the previous plan.
// The caller supplies the time so transitions can be tested without a clock.
func TransitionStep(p domain.AgentPlan, number int, status string, now time.Time) (domain.AgentPlan, error) {
	step := CurrentStep(p)
	if step == nil || step.Number != number || (status != "completed" && status != "skipped") {
		return p, fmt.Errorf("%w: complete or skip the current step", ErrInvalid)
	}
	p.Steps = slices.Clone(p.Steps)
	step = CurrentStep(p)
	step.Status, step.UpdatedAt = status, now
	p.Status, p.UpdatedAt = "completed", now
	for i := range p.Steps {
		if p.Steps[i].Status == "pending" {
			p.Steps[i].Status, p.Steps[i].UpdatedAt = "in_progress", now
			p.Status = "active"
			break
		}
	}
	return p, nil
}

func ReviseRemaining(p domain.AgentPlan, titles []string, now time.Time) (domain.AgentPlan, error) {
	normalized, err := normalizeTitles(titles, 1)
	if err != nil {
		return p, err
	}
	if p.Status == "completed" {
		return p, fmt.Errorf("%w: completed plans cannot be revised; create a new plan", ErrInvalid)
	}
	retained := 0
	for _, step := range p.Steps {
		if step.Status != "completed" && step.Status != "skipped" {
			break
		}
		retained++
	}
	steps := make([]domain.AgentPlanStep, retained+len(normalized))
	copy(steps, p.Steps[:retained])
	for i, title := range normalized {
		status := "pending"
		if i == 0 {
			status = "in_progress"
		}
		steps[retained+i] = domain.AgentPlanStep{Number: retained + i + 1, Title: title, Status: status, UpdatedAt: now}
	}
	p.Steps, p.UpdatedAt = steps, now
	return p, nil
}

func normalizeTitles(titles []string, minimum int) ([]string, error) {
	if len(titles) < minimum || len(titles) > 8 {
		return nil, fmt.Errorf("%w: provide %d-8 ordered steps", ErrInvalid, minimum)
	}
	result := make([]string, len(titles))
	seen := make(map[string]bool, len(titles))
	for i, title := range titles {
		title = strings.TrimSpace(title)
		if title == "" || utf8.RuneCountInString(title) > 240 || seen[strings.ToLower(title)] {
			return nil, fmt.Errorf("%w: step %d must have a unique title of 1-240 characters", ErrInvalid, i+1)
		}
		seen[strings.ToLower(title)] = true
		result[i] = title
	}
	return result, nil
}
