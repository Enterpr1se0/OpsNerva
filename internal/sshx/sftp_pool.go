package sshx

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"strconv"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/pkg/sftp"
)

const (
	// One transfer and one interactive directory operation can run independently.
	sftpSessionsPerConnection = 2
	sftpConnectionIdleTimeout = 2 * time.Minute
	sftpRequestsPerFile       = 32
)

type sftpHostPool struct {
	key      string
	sessions map[*sftpSession]struct{}
	changed  chan struct{}
}

// A pooled session owns its entire SSH chain. It is leased exclusively, so
// canceling a blocked subsystem/file operation cannot abort another request.
type sftpSession struct {
	pool      *sftpHostPool
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	client    *sftp.Client
	busy      bool
	idleTimer *time.Timer
	lastUsed  time.Time
}

func sftpConnectionKey(connection ConnectionSpec) string {
	digest := sha256.New()
	writeSFTPKeyField(digest, []byte("target"))
	writeSFTPHostKey(digest, connection.Target)
	writeSFTPKeyField(digest, []byte(strconv.Itoa(len(connection.Jumps))))
	for _, jump := range connection.Jumps {
		writeSFTPKeyField(digest, []byte("jump"))
		writeSFTPHostKey(digest, jump)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func writeSFTPHostKey(target hash.Hash, host domain.Host) {
	for _, value := range [][]byte{
		[]byte(host.ID),
		[]byte(host.Address),
		[]byte(strconv.Itoa(host.Port)),
		[]byte(host.User),
		[]byte(host.AuthType),
		[]byte(host.KnownHostsFile),
		[]byte(host.ProxyURL),
		[]byte(strconv.FormatInt(host.UpdatedAt.UnixNano(), 10)),
		[]byte(strconv.FormatInt(host.ProxyUpdatedAt.UnixNano(), 10)),
		[]byte(host.Password),
		host.PrivateKey,
		[]byte(host.ProxyUsername),
		[]byte(host.ProxyPassword),
	} {
		writeSFTPKeyField(target, value)
	}
}

func writeSFTPKeyField(target hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = target.Write(size[:])
	_, _ = target.Write(value)
}

func (pool *sftpHostPool) notify() {
	close(pool.changed)
	pool.changed = make(chan struct{})
}

func (t *NativeSSHTransport) acquireSFTPSession(ctx context.Context, connection ConnectionSpec) (*sftpSession, bool, error) {
	key := sftpConnectionKey(connection)
	for {
		t.sftpPoolMu.Lock()
		if err := ctx.Err(); err != nil {
			t.sftpPoolMu.Unlock()
			return nil, false, err
		}
		if t.sftpPoolDone {
			t.sftpPoolMu.Unlock()
			return nil, false, errors.New("SSH transport is closed")
		}
		pool := t.sftpPool[key]
		if pool == nil {
			pool = &sftpHostPool{key: key, sessions: make(map[*sftpSession]struct{}), changed: make(chan struct{})}
			t.sftpPool[key] = pool
		}
		for session := range pool.sessions {
			if !session.busy && session.ctx.Err() == nil {
				session.busy = true
				if session.idleTimer != nil {
					session.idleTimer.Stop()
					session.idleTimer = nil
				}
				t.sftpPoolMu.Unlock()
				return session, false, nil
			}
		}
		if len(pool.sessions) < sftpSessionsPerConnection {
			lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
			session := &sftpSession{pool: pool, ctx: lifetime, cancel: cancel, done: make(chan struct{}), busy: true}
			pool.sessions[session] = struct{}{}
			t.sftpPoolMu.Unlock()
			return session, true, nil
		}
		changed := pool.changed
		t.sftpPoolMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-changed:
		}
	}
}

func (t *NativeSSHTransport) releaseSFTPSession(session *sftpSession) {
	t.sftpPoolMu.Lock()
	defer t.sftpPoolMu.Unlock()
	if session.ctx.Err() != nil || t.sftpPoolDone {
		return
	}
	session.busy = false
	session.lastUsed = time.Now()
	session.idleTimer = time.AfterFunc(sftpConnectionIdleTimeout, func() { t.expireSFTPSession(session) })
	session.pool.notify()
}

func (t *NativeSSHTransport) expireSFTPSession(session *sftpSession) {
	t.sftpPoolMu.Lock()
	defer t.sftpPoolMu.Unlock()
	// A timer that already fired may race with a checkout and a later return.
	if !session.busy && time.Since(session.lastUsed) >= sftpConnectionIdleTimeout {
		session.cancel()
	}
}

func (t *NativeSSHTransport) forgetSFTPSession(session *sftpSession) {
	t.sftpPoolMu.Lock()
	defer t.sftpPoolMu.Unlock()
	if session.idleTimer != nil {
		session.idleTimer.Stop()
	}
	delete(session.pool.sessions, session)
	if len(session.pool.sessions) == 0 && t.sftpPool[session.pool.key] == session.pool {
		delete(t.sftpPool, session.pool.key)
	}
	session.pool.notify()
}

func (t *NativeSSHTransport) closeSFTPPool() {
	t.sftpPoolMu.Lock()
	t.sftpPoolDone = true
	var sessions []*sftpSession
	for _, pool := range t.sftpPool {
		for session := range pool.sessions {
			session.cancel()
			sessions = append(sessions, session)
		}
		pool.notify()
	}
	t.sftpPoolMu.Unlock()
	for _, session := range sessions {
		<-session.done
	}
}
