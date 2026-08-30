package httpapi

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/agent"
	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/ids"
	"github.com/Enterpr1se0/opsnerva/internal/store"
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
	mu               sync.Mutex
	jobs             map[string]modelTestJob
	subscribers      map[uint64]*modelTestSubscriber
	nextSubscriberID uint64
}

type modelTestSubscriber struct {
	jobIDs     map[string]struct{}
	events     chan modelTestJob
	overflow   chan struct{}
	overflowed bool
}

type modelTestSubscription struct {
	Jobs        map[string]modelTestJob
	Events      <-chan modelTestJob
	Overflow    <-chan struct{}
	unsubscribe func()
}

func (s modelTestSubscription) Unsubscribe() {
	if s.unsubscribe != nil {
		s.unsubscribe()
	}
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
	j.publishLocked(job)
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
	j.publishLocked(job)
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

func (j *modelTestJobs) subscribe(jobIDs []string) modelTestSubscription {
	events := make(chan modelTestJob, 16)
	overflow := make(chan struct{}, 1)
	ids := make(map[string]struct{}, len(jobIDs))
	for _, id := range jobIDs {
		ids[id] = struct{}{}
	}

	j.mu.Lock()
	j.pruneLocked(time.Now().UTC())
	snapshot := make(map[string]modelTestJob, len(ids))
	for id := range ids {
		if job, ok := j.jobs[id]; ok {
			snapshot[id] = job
		}
	}
	if j.subscribers == nil {
		j.subscribers = make(map[uint64]*modelTestSubscriber)
	}
	j.nextSubscriberID++
	subscriberID := j.nextSubscriberID
	j.subscribers[subscriberID] = &modelTestSubscriber{jobIDs: ids, events: events, overflow: overflow}
	j.mu.Unlock()

	var once sync.Once
	return modelTestSubscription{
		Jobs:     snapshot,
		Events:   events,
		Overflow: overflow,
		unsubscribe: func() {
			once.Do(func() {
				j.mu.Lock()
				delete(j.subscribers, subscriberID)
				j.mu.Unlock()
			})
		},
	}
}

func (j *modelTestJobs) publishLocked(job modelTestJob) {
	for _, subscriber := range j.subscribers {
		if subscriber.overflowed {
			continue
		}
		if _, subscribed := subscriber.jobIDs[job.ID]; !subscribed {
			continue
		}
		select {
		case subscriber.events <- job:
		default:
			subscriber.overflowed = true
			subscriber.overflow <- struct{}{}
		}
	}
}

func (s *Server) testModelConfiguration(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil {
		writeErrorStatus(w, agent.ErrUnavailable, http.StatusServiceUnavailable)
		return
	}
	var input domain.ModelTestInput
	if !decode(w, r, &input) {
		return
	}
	cfg, err := s.service.ModelTestConfig(r.Context(), input)
	if err != nil {
		writeError(w, err)
		return
	}
	job := s.modelTests.start(context.WithoutCancel(r.Context()), cfg, modelTestIdentity{}, s.agent.TestProvider)
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) testModelProvider(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil {
		writeErrorStatus(w, agent.ErrUnavailable, http.StatusServiceUnavailable)
		return
	}
	cfg, provider, err := s.service.ModelProviderConfig(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	job := s.modelTests.start(context.WithoutCancel(r.Context()), cfg, modelTestIdentity{ProviderID: provider.ID, Name: provider.Name}, s.agent.TestProvider)
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) getModelTest(w http.ResponseWriter, r *http.Request) {
	if s.modelTests == nil {
		writeErrorStatus(w, store.ErrNotFound, http.StatusNotFound)
		return
	}
	job, ok := s.modelTests.get(r.PathValue("id"))
	if !ok {
		writeErrorStatus(w, store.ErrNotFound, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, job)
}
