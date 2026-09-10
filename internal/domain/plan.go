package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrInvalidAgentPlan = errors.New("invalid agent plan")

// AgentPlan is the session's ordered work plan. Completed steps remain visible
// until a new plan replaces it; execution activity is tracked by the Agent run.
type AgentPlan struct {
	SessionID string          `json:"session_id"`
	Goal      string          `json:"goal"`
	Status    string          `json:"status"`
	Steps     []AgentPlanStep `json:"steps"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type AgentPlanStep struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Status      string    `json:"status"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// NewAgentPlan defines the work sequence. The application binds its session;
// persistence assigns creation timestamps when the plan is committed.
func NewAgentPlan(goal string, titles []string) (AgentPlan, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" || utf8.RuneCountInString(goal) > 500 {
		return AgentPlan{}, fmt.Errorf("%w: goal must contain 1-500 characters", ErrInvalidAgentPlan)
	}
	normalized, err := normalizePlanTitles(titles, 2)
	if err != nil {
		return AgentPlan{}, err
	}
	steps := make([]AgentPlanStep, len(normalized))
	for i, title := range normalized {
		status := "pending"
		if i == 0 {
			status = "in_progress"
		}
		steps[i] = AgentPlanStep{Number: i + 1, Title: title, Status: status}
	}
	return AgentPlan{Goal: goal, Status: "active", Steps: steps}, nil
}

func (p AgentPlan) CurrentStep() *AgentPlanStep {
	for i := range p.Steps {
		if p.Steps[i].Status == "in_progress" {
			return &p.Steps[i]
		}
	}
	return nil
}

// TransitionStep returns a new snapshot without mutating the previous plan.
// The caller supplies the time so transitions can be tested without a clock.
func (p AgentPlan) TransitionStep(number int, status string, now time.Time) (AgentPlan, error) {
	step := p.CurrentStep()
	if step == nil || step.Number != number || (status != "completed" && status != "skipped") {
		return p, fmt.Errorf("%w: complete or skip the current step", ErrInvalidAgentPlan)
	}
	p.Steps = slices.Clone(p.Steps)
	step = p.CurrentStep()
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

func (p AgentPlan) ReviseRemaining(titles []string, now time.Time) (AgentPlan, error) {
	normalized, err := normalizePlanTitles(titles, 1)
	if err != nil {
		return p, err
	}
	if p.Status == "completed" {
		return p, fmt.Errorf("%w: completed plans cannot be revised; create a new plan", ErrInvalidAgentPlan)
	}
	retained := 0
	for _, step := range p.Steps {
		if step.Status != "completed" && step.Status != "skipped" {
			break
		}
		retained++
	}
	steps := make([]AgentPlanStep, retained+len(normalized))
	copy(steps, p.Steps[:retained])
	for i, title := range normalized {
		status := "pending"
		if i == 0 {
			status = "in_progress"
		}
		steps[retained+i] = AgentPlanStep{Number: retained + i + 1, Title: title, Status: status, UpdatedAt: now}
	}
	p.Steps, p.UpdatedAt = steps, now
	return p, nil
}

func normalizePlanTitles(titles []string, minimum int) ([]string, error) {
	if len(titles) < minimum || len(titles) > 8 {
		return nil, fmt.Errorf("%w: provide %d-8 ordered steps", ErrInvalidAgentPlan, minimum)
	}
	result := make([]string, len(titles))
	seen := make(map[string]bool, len(titles))
	for i, title := range titles {
		title = strings.TrimSpace(title)
		if title == "" || utf8.RuneCountInString(title) > 240 || seen[strings.ToLower(title)] {
			return nil, fmt.Errorf("%w: step %d must have a unique title of 1-240 characters", ErrInvalidAgentPlan, i+1)
		}
		seen[strings.ToLower(title)] = true
		result[i] = title
	}
	return result, nil
}
