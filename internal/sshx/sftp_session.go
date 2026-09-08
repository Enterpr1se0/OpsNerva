package sshx

import (
	"context"
	"fmt"
	"sync"

	"github.com/pkg/sftp"
)

type sftpLease struct {
	transport *NativeSSHTransport
	session   *sftpSession
	client    *sftp.Client
	ctx       context.Context
	stop      func() bool
	canceled  chan struct{}
	once      sync.Once
}

func (lease *sftpLease) Close() error {
	lease.once.Do(func() {
		if !lease.stop() {
			<-lease.canceled
		}
		if lease.ctx.Err() != nil {
			lease.session.cancel()
		}
		lease.transport.releaseSFTPSession(lease.session)
	})
	return nil
}

func (t *NativeSSHTransport) openSFTP(ctx context.Context, connection ConnectionSpec) (_ *sftpLease, resultErr error) {
	if err := validateNativeConnection(connection); err != nil {
		return nil, err
	}
	timing := newSFTPTiming(ctx, "open", connection.Target.ID)
	defer func() { timing.finish(resultErr) }()
	session, fresh, err := t.acquireSFTPSession(ctx, connection)
	timing.mark("pool_wait")
	if err != nil {
		return nil, err
	}
	lease := &sftpLease{transport: t, session: session, ctx: ctx, canceled: make(chan struct{})}
	lease.stop = context.AfterFunc(ctx, func() {
		session.cancel()
		close(lease.canceled)
	})
	if fresh {
		if err := t.startSFTPSession(session, connection, timing); err != nil {
			_ = lease.Close()
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		_ = lease.Close()
		return nil, err
	}
	lease.client = session.client
	return lease, nil
}

func (t *NativeSSHTransport) startSFTPSession(session *sftpSession, connection ConnectionSpec, timing *sftpTiming) (resultErr error) {
	var nativeClosed <-chan struct{}
	defer func() {
		if resultErr != nil {
			session.cancel()
			if nativeClosed != nil {
				<-nativeClosed
			}
			t.forgetSFTPSession(session)
			close(session.done)
		}
	}()
	native, err := t.connect(session.ctx, connection, nil, false)
	timing.mark("connection_acquire")
	if err != nil {
		return fmt.Errorf("connect native SSH for SFTP: %w", err)
	}
	// Close the outer TCP connection first, before any SFTP/file locks. This also
	// unblocks channel opens, subsystem negotiation and the entire ProxyJump chain.
	closed := make(chan struct{})
	nativeClosed = closed
	context.AfterFunc(session.ctx, func() {
		_ = native.clients[0].Close()
		_ = native.Close()
		close(closed)
	})
	client, err := sftp.NewClient(native.client, sftp.MaxConcurrentRequestsPerFile(sftpRequestsPerFile), sftp.UseFstat(true))
	timing.mark("subsystem_start")
	if err != nil {
		return fmt.Errorf("start native SFTP: %w", err)
	}
	session.client = client
	go func() {
		_ = client.Wait()
		session.cancel()
		<-closed
		_ = client.Close()
		t.forgetSFTPSession(session)
		close(session.done)
	}()
	return nil
}
