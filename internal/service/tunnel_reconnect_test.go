package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

func TestOperatorTunnelManualRetrySkipsBackoff(t *testing.T) {
	for _, direction := range []domain.SSHTunnelDirection{domain.SSHTunnelDirectionLocal, domain.SSHTunnelDirectionReverse} {
		t.Run(string(direction), func(t *testing.T) {
			svc, transport, host := newTestService(t)
			events, _, unsubscribe := svc.SubscribeStateEvents()
			defer unsubscribe()
			config := domain.SSHTunnelConfig{Direction: direction, RemotePort: 8080}
			if direction == domain.SSHTunnelDirectionReverse {
				config.LocalPort, config.RemotePort = 8080, 0
			}
			started, err := svc.StartOperatorSSHTunnel(context.Background(), host.ID, config, "app")
			if err != nil {
				t.Fatal(err)
			}
			defer svc.StopOperatorSSHTunnel(context.Background(), started.ID, "app")
			if err := svc.RetryOperatorSSHTunnel(context.Background(), started.ID); err == nil {
				t.Fatal("manual retry accepted a running tunnel")
			}
			transport.mu.Lock()
			client := transport.tunnelClients[0]
			transport.tunnelOpenErrs = []error{errors.New("temporary connection failure")}
			transport.mu.Unlock()
			_ = client.Close()
			for attempt := 1; attempt <= 2; attempt++ {
				waitForTunnelStateEvent(t, events, started.ID, time.Second, func(tunnel domain.SSHTunnel) bool {
					return tunnel.Status == "retrying" && tunnel.ReconnectAttempt == attempt
				})
				if err := svc.RetryOperatorSSHTunnel(context.Background(), started.ID); err != nil {
					t.Fatal(err)
				}
			}
			// Automatic attempt 2 waits two seconds. Manual retry must wake it now.
			reconnected := waitForTunnelStateEvent(t, events, started.ID, time.Second, func(tunnel domain.SSHTunnel) bool {
				return tunnel.Status == "running" && tunnel.ReconnectAttempt == 0
			})
			if reconnected.ID != started.ID || reconnected.LocalPort != started.LocalPort || reconnected.RemotePort != started.RemotePort {
				t.Fatalf("manual retry changed tunnel identity or endpoints: %#v", reconnected)
			}
			transport.mu.Lock()
			clients := len(transport.tunnelClients)
			transport.mu.Unlock()
			if clients != 2 {
				t.Fatalf("opened %d SSH clients, want initial plus one replacement", clients)
			}
		})
	}
}

func TestOperatorTunnelManualRetriesCoalesceAndRespectStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state := &sshTunnelState{ctx: ctx, retry: make(chan struct{}, 1), tunnel: domain.SSHTunnel{ID: "retry", Status: "retrying"}}
	svc := &Service{tunnels: map[string]*sshTunnelState{"retry": state}}
	var callers sync.WaitGroup
	for range 20 {
		callers.Go(func() {
			if err := svc.RetryOperatorSSHTunnel(context.Background(), "retry"); err != nil {
				t.Error(err)
			}
		})
	}
	callers.Wait()
	if len(state.retry) != 1 {
		t.Fatalf("pending retries = %d, want 1", len(state.retry))
	}
	cancel()
	if err := svc.RetryOperatorSSHTunnel(context.Background(), "retry"); err == nil {
		t.Fatal("cancelled tunnel accepted a manual retry")
	}
	if err := svc.RetryOperatorSSHTunnel(ctx, "retry"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request = %v", err)
	}
	if err := svc.RetryOperatorSSHTunnel(context.Background(), "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown tunnel = %v", err)
	}
}
