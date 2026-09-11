package websearch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

// Done is evaluated by requestTavily after joining the in-flight request.
// This lets concurrent tests synchronize without guessing scheduling delays.
type requestWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *requestWaitContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

func TestSharedRequestSurvivesOneCallerCancellation(t *testing.T) {
	client := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release := make(chan struct{})
	var releaseOnce sync.Once
	var hits atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-release:
			_, _ = w.Write([]byte(`{"results":[{"url":"https://example.com/docs","content":"shared"}]}`))
		case <-r.Context().Done():
		}
	}))
	defer provider.Close()
	defer releaseOnce.Do(func() { close(release) })
	settings := Config{BaseURL: provider.URL, APIKey: "test-key", TimeoutSeconds: 5, MaxResults: 2}
	firstCtx, cancelFirst := context.WithCancel(ctx)
	defer cancelFirst()
	first := &requestWaitContext{Context: firstCtx, waiting: make(chan struct{})}
	second := &requestWaitContext{Context: ctx, waiting: make(chan struct{})}
	type outcome struct {
		result domain.WebSearchResponse
		info   *CallInfo
		err    error
	}
	invoke := func(callCtx context.Context, done chan<- outcome) {
		result, info, err := client.Search(callCtx, settings, domain.WebSearchRequest{Query: "same"})
		done <- outcome{result, info, err}
	}
	firstDone, secondDone := make(chan outcome, 1), make(chan outcome, 1)
	go invoke(first, firstDone)
	go invoke(second, secondDone)
	for _, waiting := range []<-chan struct{}{first.waiting, second.waiting} {
		select {
		case <-waiting:
		case <-ctx.Done():
			t.Fatal("call did not join in-flight request")
		}
	}
	cancelFirst()
	firstResult := <-firstDone
	if !errors.Is(firstResult.err, context.Canceled) || firstResult.info == nil {
		t.Fatalf("canceled caller result: %+v", firstResult)
	}
	releaseOnce.Do(func() { close(release) })
	secondResult := <-secondDone
	if secondResult.err != nil || secondResult.info == nil || secondResult.info.StatusCode != 200 || len(secondResult.result.Results) != 1 {
		t.Fatalf("other caller lost shared result: %+v", secondResult)
	}
	if hits.Load() != 1 || firstResult.info == secondResult.info || firstResult.info.InputDigest != secondResult.info.InputDigest {
		t.Fatalf("shared call isolation: hits=%d first=%+v second=%+v", hits.Load(), firstResult.info, secondResult.info)
	}
}

func TestClientBoundsConcurrentProviderRequests(t *testing.T) {
	client := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}, 6), make(chan struct{})
	var releaseOnce sync.Once
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
			_, _ = w.Write([]byte(`{"results":[]}`))
		case <-r.Context().Done():
		}
	}))
	defer provider.Close()
	defer releaseOnce.Do(func() { close(release) })
	settings := Config{BaseURL: provider.URL, APIKey: "test-key", TimeoutSeconds: 5, MaxResults: 2}
	done := make(chan error, 6)
	for index := 0; index < 6; index++ {
		go func() {
			_, _, err := client.Search(ctx, settings, domain.WebSearchRequest{Query: fmt.Sprintf("query-%d", index)})
			done <- err
		}()
	}
	for index := 0; index < 4; index++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("expected four active provider requests")
		}
	}
	select {
	case <-entered:
		t.Fatal("more than four provider requests ran concurrently")
	case <-time.After(50 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	for index := 0; index < 6; index++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("queued request did not complete")
		}
	}
	if len(entered) != 2 {
		t.Fatalf("queued provider requests = %d, want 2", len(entered))
	}
}

func TestTavilyRequestPreservesContextCancellation(t *testing.T) {
	client := New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var response tavilyExtractResponse
	_, err := client.requestTavily(ctx, Config{
		BaseURL: "http://127.0.0.1:1", TimeoutSeconds: 5,
		APIKey: "test",
	}, "/extract", tavilyExtractRequest{URLs: []string{"https://example.com"}}, &response)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Tavily request returned %v", err)
	}
}

func TestTavilyProviderErrorsAreClassifiedAndRetriedOnce(t *testing.T) {
	testCases := []struct {
		name       string
		status     int
		retryAfter string
		wantCode   string
		wantHits   int32
		wantOK     bool
		retryable  bool
	}{
		{name: "invalid request", status: http.StatusBadRequest, wantCode: ErrorInvalidRequest, wantHits: 1},
		{name: "authentication", status: http.StatusUnauthorized, wantCode: ErrorAuthenticationFailed, wantHits: 1},
		{name: "short rate limit", status: http.StatusTooManyRequests, wantHits: 2, wantOK: true},
		{name: "long rate limit", status: http.StatusTooManyRequests, retryAfter: "10", wantCode: ErrorRateLimited, wantHits: 1},
		{name: "provider unavailable", status: http.StatusServiceUnavailable, wantHits: 2, wantOK: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			client := New(nil)
			var hits atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				current := hits.Add(1)
				if current == 1 || testCase.wantHits == 1 {
					if testCase.retryAfter != "" {
						w.Header().Set("Retry-After", testCase.retryAfter)
					}
					w.WriteHeader(testCase.status)
					_, _ = w.Write([]byte(`{"error":"` + strings.Repeat("x", 8<<10) + `"}`))
					return
				}
				_, _ = w.Write([]byte(`{"results":[]}`))
			}))
			defer provider.Close()
			var output tavilySearchResponse
			meta, err := client.requestTavily(context.Background(), Config{
				BaseURL: provider.URL, TimeoutSeconds: 5, APIKey: "test",
			}, "/search", tavilySearchRequest{Query: "test", SearchDepth: "basic", MaxResults: 1}, &output)
			if testCase.wantOK {
				if err != nil || meta.RetryCount != 1 {
					t.Fatalf("retry result meta=%#v err=%v", meta, err)
				}
			} else {
				var providerError *ProviderError
				if !errors.As(err, &providerError) || providerError.Code != testCase.wantCode || providerError.Retryable != testCase.retryable || len(err.Error()) > maxErrorBytes+256 {
					t.Fatalf("provider error = %#v, err=%v", providerError, err)
				}
			}
			if hits.Load() != testCase.wantHits {
				t.Fatalf("provider hits = %d, want %d", hits.Load(), testCase.wantHits)
			}
		})
	}
}

func TestTavilyIdenticalInflightRequestsAreCoalesced(t *testing.T) {
	client := New(nil)
	var hits atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer provider.Close()
	settings := Config{BaseURL: provider.URL, TimeoutSeconds: 5, APIKey: "test"}
	var group sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for index := 0; index < 2; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			var output tavilySearchResponse
			_, err := client.requestTavily(context.Background(), settings, "/search", tavilySearchRequest{Query: "same", SearchDepth: "basic", MaxResults: 1}, &output)
			errorsSeen <- err
		}()
	}
	<-started
	time.Sleep(20 * time.Millisecond)
	close(release)
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("identical in-flight requests produced %d provider calls", hits.Load())
	}
}
