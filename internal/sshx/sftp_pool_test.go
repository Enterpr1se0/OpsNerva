package sshx

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"golang.org/x/crypto/ssh"
)

func testSFTPConnection(t *testing.T, server *testSSHServer) (*NativeSSHTransport, ConnectionSpec) {
	t.Helper()
	transport := NewNativeSSHTransport(config.SSH{DefaultKnownHosts: filepath.Join(t.TempDir(), "known_hosts")}, config.Default().Limits)
	t.Cleanup(func() { _ = transport.Close() })
	connection := ConnectionSpec{Target: server.host()}
	key, err := transport.ScanHostKey(context.Background(), connection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.TrustHostKey(context.Background(), connection, key.Fingerprint); err != nil {
		t.Fatal(err)
	}
	return transport, connection
}

func awaitSFTP(t *testing.T, done <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out: %s", description)
	}
}

func TestSFTPPoolReusesSessionAndBoundsWaiters(t *testing.T) {
	server := startTestSSHServer(t, "sftp-pool-password")
	transport, connection := testSFTPConnection(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	first, err := transport.openSFTP(ctx, connection)
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	// Canceling an already-returned lease must not cancel its next borrower.
	cancel()
	reused, err := transport.openSFTP(context.Background(), connection)
	if err != nil || reused.client != first.client {
		t.Fatalf("SFTP session was not reused: %v", err)
	}
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	second, err := transport.openSFTP(secondCtx, connection)
	if err != nil || second.client == reused.client {
		t.Fatalf("concurrent leases share a session: %v", err)
	}
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelWait()
	if _, err := transport.openSFTP(waitCtx, connection); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("full pool did not respect waiting context: %v", err)
	}
	transport.sftpPoolMu.Lock()
	count := len(transport.sftpPool[sftpConnectionKey(connection)].sessions)
	transport.sftpPoolMu.Unlock()
	if count != sftpSessionsPerConnection {
		t.Fatalf("pool exceeded its bound: %d", count)
	}
	cancelSecond()
	awaitSFTP(t, second.session.done, "canceled session shutdown")
	_ = second.Close()
	if _, err := reused.client.Getwd(); err != nil {
		t.Fatalf("canceling another lease interrupted the retained session: %v", err)
	}
	_ = reused.Close()
	third, err := transport.openSFTP(context.Background(), connection)
	if err != nil || third.client != reused.client {
		t.Fatalf("healthy session was lost after cancellation: %v", err)
	}
	transport.sftpPoolMu.Lock()
	third.session.lastUsed = time.Now().Add(-sftpConnectionIdleTimeout)
	transport.sftpPoolMu.Unlock()
	transport.expireSFTPSession(third.session)
	if _, err := third.client.Getwd(); err != nil {
		t.Fatalf("stale idle timer interrupted an active borrower: %v", err)
	}
	_ = third.Close()
	transport.sftpPoolMu.Lock()
	third.session.lastUsed = time.Now().Add(-sftpConnectionIdleTimeout)
	transport.sftpPoolMu.Unlock()
	transport.expireSFTPSession(third.session)
	awaitSFTP(t, third.session.done, "idle session eviction")
}

func TestSFTPPoolEvictsClosedSubsystem(t *testing.T) {
	server := startTestSSHServer(t, "sftp-closed-password")
	transport, connection := testSFTPConnection(t, server)
	lease, err := transport.openSFTP(context.Background(), connection)
	if err != nil {
		t.Fatal(err)
	}
	_ = lease.Close()
	_ = lease.client.Close()
	awaitSFTP(t, lease.session.done, "failed idle subsystem eviction")
	replacement, err := transport.openSFTP(context.Background(), connection)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if replacement.client == lease.client {
		t.Fatal("closed subsystem was reused")
	}
	if _, err := replacement.client.Getwd(); err != nil {
		t.Fatalf("replacement session is unusable: %v", err)
	}
}

func TestSFTPPoolShutdownUnblocksWaiters(t *testing.T) {
	server := startTestSSHServer(t, "sftp-shutdown-password")
	transport, connection := testSFTPConnection(t, server)
	for range sftpSessionsPerConnection {
		lease, err := transport.openSFTP(context.Background(), connection)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Close()
	}
	done := make(chan error, 1)
	go func() { _, err := transport.openSFTP(context.Background(), connection); done <- err }()
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed pool accepted an operation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pool shutdown left a waiter blocked")
	}
}

func TestSFTPPoolCancellationDuringSubsystemStartup(t *testing.T) {
	for _, test := range []struct {
		name        string
		acknowledge bool
		jump        bool
	}{
		{name: "subsystem request"},
		{name: "version exchange", acknowledge: true},
		{name: "ProxyJump version exchange", acknowledge: true, jump: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			entered := make(chan struct{})
			server := startTestSSHServer(t, "sftp-startup-password", func(server *testSSHServer) {
				server.sftpHandler = func(channel ssh.Channel, request *ssh.Request) {
					if test.acknowledge {
						_ = request.Reply(true, nil)
					}
					close(entered)
					_, _ = io.Copy(io.Discard, channel)
				}
			})
			transport, connection := testSFTPConnection(t, server)
			if test.jump {
				jump := startTestSSHServer(t, "sftp-jump-password")
				// This cleanup must precede the jump server's cleanup as well.
				t.Cleanup(func() { _ = transport.Close() })
				jumpConnection := ConnectionSpec{Target: jump.host()}
				key, err := transport.ScanHostKey(context.Background(), jumpConnection)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := transport.TrustHostKey(context.Background(), jumpConnection, key.Fingerprint); err != nil {
					t.Fatal(err)
				}
				connection.Jumps = append(connection.Jumps, jump.host())
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := transport.openSFTP(ctx, connection); done <- err }()
			awaitSFTP(t, entered, "subsystem request")
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("canceled subsystem startup succeeded")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("subsystem startup ignored cancellation")
			}
		})
	}
}
