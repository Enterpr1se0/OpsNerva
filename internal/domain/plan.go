package domain

import "time"

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
