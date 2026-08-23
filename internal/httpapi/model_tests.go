package httpapi

import (
	"context"
	"sync"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/agent"
	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/ids"
)

const modelTestRetention = 10 * time.Minute

type modelTestIdentity struct {
	ProviderID string
	Name       string
}

type modelTestResult struct {
	ProviderID string `json:"provider_id,omitempty"`
	Name       string `json:"name,omitempty"`
	Model      string `json:"model"`
	Response   string `json:"response"`
	LatencyMS  int64  `json:"latency_ms"`
}

type modelTestJob struct {
	ID         string           `json:"id"`
	Status     string           `json:"status"`
	Result     *modelTestResult `json:"result,omitempty"`
	Error      string           `json:"error,omitempty"`
	CreatedAt  time.Time        `json:"created_at"`
	FinishedAt *time.Time       `json:"finished_at,omitempty"`
}

type modelTestRunner func(context.Context, config.Model) (agent.TestResult, error)

type modelTestJobs struct {
	mu   sync.Mutex
	jobs map[string]modelTestJob
}

func newModelTestJobs() *modelTestJobs {
	return &modelTestJobs{jobs: make(map[string]modelTestJob)}
}

func (j *modelTestJobs) start(ctx context.Context, cfg config.Model, identity modelTestIdentity, run modelTestRunner) modelTestJob {
	now := time.Now().UTC()
	job := modelTestJob{ID: ids.New("model_test"), Status: "running", CreatedAt: now}
	j.mu.Lock()
	j.pruneLocked(now)
	j.jobs[job.ID] = job
	j.mu.Unlock()

	go func() {
		result, err := run(ctx, cfg)
		j.finish(job.ID, identity, result, err)
	}()
	return job
}

func (j *modelTestJobs) finish(id string, identity modelTestIdentity, result agent.TestResult, err error) {
	now := time.Now().UTC()
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.jobs[id]
	if !ok {
		return
	}
	job.FinishedAt = &now
	if err != nil {
		job.Status = "failed"
		job.Error = err.Error()
	} else {
		job.Status = "completed"
		job.Result = &modelTestResult{
			ProviderID: identity.ProviderID, Name: identity.Name, Model: result.Model,
			Response: result.Response, LatencyMS: result.LatencyMS,
		}
	}
	j.jobs[id] = job
}

func (j *modelTestJobs) get(id string) (modelTestJob, bool) {
	if j == nil {
		return modelTestJob{}, false
	}
	now := time.Now().UTC()
	j.mu.Lock()
	defer j.mu.Unlock()
	j.pruneLocked(now)
	job, ok := j.jobs[id]
	return job, ok
}

func (j *modelTestJobs) pruneLocked(now time.Time) {
	for id, job := range j.jobs {
		if job.FinishedAt != nil && now.Sub(*job.FinishedAt) > modelTestRetention {
			delete(j.jobs, id)
		}
	}
}
