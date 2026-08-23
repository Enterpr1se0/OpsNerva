package sshx

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"strconv"
	"sync"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

const sftpConnectionIdleTimeout = 30 * time.Second

type sftpPoolEntry struct {
	key       string
	client    *nativeClient
	ready     chan struct{}
	readyOnce sync.Once
	refs      int
	stale     bool
	idleTimer *time.Timer
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

func (t *NativeSSHTransport) acquireSFTPConnection(ctx context.Context, connection ConnectionSpec) (*sftpPoolEntry, error) {
	key := sftpConnectionKey(connection)
	for {
		t.sftpPoolMu.Lock()
		if t.sftpPoolDone {
			t.sftpPoolMu.Unlock()
			return nil, errors.New("SSH transport is closed")
		}
		if t.sftpPool == nil {
			t.sftpPool = make(map[string]*sftpPoolEntry)
		}
		entry := t.sftpPool[key]
		if entry == nil {
			entry = &sftpPoolEntry{key: key, ready: make(chan struct{})}
			t.sftpPool[key] = entry
			t.sftpPoolMu.Unlock()
			client, err := t.connect(ctx, connection, nil, false)
			t.sftpPoolMu.Lock()
			if err != nil {
				if t.sftpPool[key] == entry {
					delete(t.sftpPool, key)
				}
				entry.readyOnce.Do(func() { close(entry.ready) })
				t.sftpPoolMu.Unlock()
				return nil, err
			}
			if t.sftpPoolDone || t.sftpPool[key] != entry {
				entry.readyOnce.Do(func() { close(entry.ready) })
				t.sftpPoolMu.Unlock()
				_ = client.Close()
				return nil, errors.New("SSH transport is closed")
			}
			entry.client = client
			entry.refs = 1
			entry.readyOnce.Do(func() { close(entry.ready) })
			t.sftpPoolMu.Unlock()
			return entry, nil
		}
		if entry.client == nil {
			ready := entry.ready
			t.sftpPoolMu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ready:
				continue
			}
		}
		if entry.stale {
			delete(t.sftpPool, key)
			t.sftpPoolMu.Unlock()
			continue
		}
		if entry.idleTimer != nil {
			entry.idleTimer.Stop()
			entry.idleTimer = nil
		}
		entry.refs++
		t.sftpPoolMu.Unlock()
		return entry, nil
	}
}

func (t *NativeSSHTransport) releaseSFTPConnection(entry *sftpPoolEntry, stale bool) {
	if entry == nil {
		return
	}
	var closeClient *nativeClient
	t.sftpPoolMu.Lock()
	if entry.refs > 0 {
		entry.refs--
	}
	if stale {
		entry.stale = true
		if t.sftpPool[entry.key] == entry {
			delete(t.sftpPool, entry.key)
		}
	}
	if entry.refs == 0 {
		if entry.stale {
			closeClient = entry.client
		} else if entry.idleTimer == nil {
			entry.idleTimer = time.AfterFunc(sftpConnectionIdleTimeout, func() {
				t.expireSFTPConnection(entry)
			})
		}
	}
	t.sftpPoolMu.Unlock()
	if closeClient != nil {
		_ = closeClient.Close()
	}
}

func (t *NativeSSHTransport) expireSFTPConnection(entry *sftpPoolEntry) {
	var closeClient *nativeClient
	t.sftpPoolMu.Lock()
	entry.idleTimer = nil
	if entry.refs == 0 && t.sftpPool[entry.key] == entry {
		delete(t.sftpPool, entry.key)
		entry.stale = true
		closeClient = entry.client
	}
	t.sftpPoolMu.Unlock()
	if closeClient != nil {
		_ = closeClient.Close()
	}
}
