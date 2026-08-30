package httpapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/agent"
	"github.com/Enterpr1se0/opsnerva/internal/config"
)

func TestModelTestJobRunsWithoutHoldingStartRequest(t *testing.T) {
	jobs := newModelTestJobs()
	started := make(chan struct{})
	release := make(chan struct{})
	runner := func(context.Context, config.Model) (agent.TestResult, error) {
		close(started)
		<-release
		return agent.TestResult{Model: "test-model", Response: "Hello", LatencyMS: 25}, nil
	}

	job := jobs.start(context.Background(), config.Model{Name: "test-model"}, modelTestIdentity{ProviderID: "provider-1", Name: "Provider"}, runner)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("model test runner did not start")
	}
	current, ok := jobs.get(job.ID)
	if !ok || current.Status != "running" {
		t.Fatalf("model test was not queryable while running: %#v", current)
	}
	subscription := jobs.subscribe([]string{job.ID})
	defer subscription.Unsubscribe()
	if snapshot := subscription.Jobs[job.ID]; snapshot.Status != "running" {
		t.Fatalf("model test subscription snapshot = %#v", snapshot)
	}

	close(release)
	current = receiveModelTestJob(t, subscription.Events)
	if current.Status != "completed" || current.Result == nil {
		t.Fatalf("model test did not complete: %#v", current)
	}
	if current.Result.ProviderID != "provider-1" || current.Result.Name != "Provider" || current.Result.Model != "test-model" || current.Result.Response != "Hello" || current.Result.LatencyMS != 25 {
		t.Fatalf("model test result lost fields: %#v", current.Result)
	}
}

func TestModelTestJobPreservesFailure(t *testing.T) {
	jobs := newModelTestJobs()
	release := make(chan struct{})
	job := jobs.start(context.Background(), config.Model{}, modelTestIdentity{}, func(context.Context, config.Model) (agent.TestResult, error) {
		<-release
		return agent.TestResult{}, errors.New("upstream unavailable")
	})
	subscription := jobs.subscribe([]string{job.ID})
	defer subscription.Unsubscribe()
	close(release)
	current := receiveModelTestJob(t, subscription.Events)
	if current.Status != "failed" || current.Error != "upstream unavailable" || current.Result != nil {
		t.Fatalf("model test failure was not preserved: %#v", current)
	}
}

func receiveModelTestJob(t *testing.T, events <-chan modelTestJob) modelTestJob {
	t.Helper()
	select {
	case job := <-events:
		return job
	case <-time.After(time.Second):
		t.Fatal("model test completion event was not published")
	}
	return modelTestJob{}
}
