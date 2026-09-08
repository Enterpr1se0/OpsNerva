package sshx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type deletionFileInfo struct {
	name string
	mode os.FileMode
}

func (info deletionFileInfo) Name() string       { return info.name }
func (info deletionFileInfo) Size() int64        { return 0 }
func (info deletionFileInfo) Mode() os.FileMode  { return info.mode }
func (info deletionFileInfo) ModTime() time.Time { return time.Time{} }
func (info deletionFileInfo) IsDir() bool        { return info.mode.IsDir() }
func (info deletionFileInfo) Sys() any           { return nil }

// A deterministic filesystem model detects parent-before-child mistakes and
// holds deletion responses until the requested concurrency is reached.
type deletionTestClient struct {
	mu        sync.Mutex
	entries   map[string]os.FileMode
	readError map[string]error
	failures  map[string]error
	before    func(string, bool) error
	active    int
	maximum   int
	reads     []string
}

func (client *deletionTestClient) ReadDirContext(ctx context.Context, target string) ([]os.FileInfo, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.reads = append(client.reads, target)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := client.readError[target]; err != nil {
		return nil, err
	}
	var entries []os.FileInfo
	for name, mode := range client.entries {
		if name != target && path.Dir(name) == target {
			entries = append(entries, deletionFileInfo{name: path.Base(name), mode: mode})
		}
	}
	return entries, nil
}

func (client *deletionTestClient) Remove(target string) error {
	return client.remove(target, false)
}

func (client *deletionTestClient) RemoveDirectory(target string) error {
	return client.remove(target, true)
}

func (client *deletionTestClient) remove(target string, directory bool) error {
	client.mu.Lock()
	client.active++
	client.maximum = max(client.maximum, client.active)
	client.mu.Unlock()
	defer func() { client.mu.Lock(); client.active--; client.mu.Unlock() }()
	if client.before != nil {
		if err := client.before(target, directory); err != nil {
			return err
		}
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if err := client.failures[target]; err != nil {
		return err
	}
	mode, exists := client.entries[target]
	if !exists {
		return os.ErrNotExist
	}
	if directory != mode.IsDir() {
		return fmt.Errorf("wrong removal operation for %s", target)
	}
	if directory {
		for name := range client.entries {
			if strings.HasPrefix(name, target+"/") {
				return fmt.Errorf("directory not empty: %s contains %s", target, name)
			}
		}
	}
	delete(client.entries, target)
	return nil
}

func TestSFTPDeleteBoundedConcurrencyAndDirectoryBarriers(t *testing.T) {
	client := &deletionTestClient{entries: map[string]os.FileMode{
		"/tree": os.ModeDir, "/tree/child": os.ModeDir, "/tree/child/deep": os.ModeDir,
		"/tree/link": os.ModeSymlink, "/outside": os.ModeDir, "/outside/keep": 0,
	}}
	for index := range 40 {
		client.entries[fmt.Sprintf("/tree/child/deep/file-%02d", index)] = 0
	}
	started := make(chan struct{}, 48)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	client.before = func(_ string, directory bool) error {
		if !directory {
			started <- struct{}{}
			<-release
		}
		return nil
	}
	var progress []SFTPDeleteProgress
	done := make(chan error, 1)
	go func() {
		done <- deleteSFTPTree(context.Background(), client, "/tree", true, true, func(update SFTPDeleteProgress) { progress = append(progress, update) })
	}()
	for range sftpDeleteConcurrency {
		awaitSFTP(t, started, "parallel deletion request")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("recursive deletion did not finish")
	}
	if client.maximum != sftpDeleteConcurrency {
		t.Fatalf("concurrency = %d", client.maximum)
	}
	final := progress[len(progress)-1]
	if final.Discovered != 44 || final.Removed != 44 || final.Failed != 0 || final.Skipped != 0 || !final.RootRemoved || !final.ScanComplete {
		t.Fatalf("final progress = %+v", final)
	}
	if len(client.entries) != 2 {
		t.Fatalf("deleted outside target: %+v", client.entries)
	}
	for _, read := range client.reads {
		if read == "/tree/link" {
			t.Fatal("followed a symlink")
		}
	}
	for index := 1; index < len(progress); index++ {
		if progress[index].Removed < progress[index-1].Removed || progress[index].Discovered < progress[index-1].Discovered {
			t.Fatalf("progress went backwards: %+v", progress)
		}
	}
}

func TestSFTPDeletePartialFailureKeepsAncestorsAndRemovesSiblings(t *testing.T) {
	client := &deletionTestClient{
		entries: map[string]os.FileMode{
			"/tree": os.ModeDir, "/tree/a": 0, "/tree/fail": os.ModeDir, "/tree/fail/protected": 0,
			"/tree/read-denied": os.ModeDir, "/tree/good": os.ModeDir, "/tree/good/file": 0, "/tree/link": os.ModeSymlink,
		},
		readError: map[string]error{"/tree/read-denied": os.ErrPermission},
		failures:  map[string]error{"/tree/fail/protected": os.ErrPermission},
	}
	var final SFTPDeleteProgress
	err := deleteSFTPTree(context.Background(), client, "/tree", true, true, func(update SFTPDeleteProgress) { final = update })
	if err == nil || final.Removed != 4 || final.Failed != 2 || final.Skipped != 2 || final.Discovered != 8 || final.FirstError == "" || final.RootRemoved {
		t.Fatalf("partial result = %+v, error = %v", final, err)
	}
	for _, target := range []string{"/tree", "/tree/fail", "/tree/fail/protected", "/tree/read-denied"} {
		if _, exists := client.entries[target]; !exists {
			t.Fatalf("failed subtree disappeared: %s", target)
		}
	}
}

func TestSFTPDeleteCancelDrainsQueuedWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &deletionTestClient{entries: map[string]os.FileMode{"/tree": os.ModeDir}}
	for index := range 100 {
		client.entries[fmt.Sprintf("/tree/%d", index)] = 0
	}
	started := make(chan struct{}, sftpDeleteConcurrency)
	client.before = func(string, bool) error { started <- struct{}{}; <-ctx.Done(); return ctx.Err() }
	var final SFTPDeleteProgress
	done := make(chan error, 1)
	go func() {
		done <- deleteSFTPTree(ctx, client, "/tree", true, true, func(update SFTPDeleteProgress) { final = update })
	}()
	for range sftpDeleteConcurrency {
		awaitSFTP(t, started, "cancel test request")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || final.Failed != 0 || final.Removed != 0 || final.RootRemoved {
			t.Fatalf("cancel result = %+v, error = %v", final, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel left queued work blocked")
	}
}

func TestSFTPDeleteNonRecursiveAndRootProtection(t *testing.T) {
	for _, target := range []string{"/", "", "/tree/..", "/tree/", "relative"} {
		if ValidateSFTPDeletePath(target) == nil {
			t.Fatalf("unsafe target accepted: %q", target)
		}
	}
	client := &deletionTestClient{entries: map[string]os.FileMode{"/tree": os.ModeDir, "/tree/keep": 0}}
	var final SFTPDeleteProgress
	err := deleteSFTPTree(context.Background(), client, "/tree", true, false, func(update SFTPDeleteProgress) { final = update })
	if err == nil || final.Failed != 1 || final.Removed != 0 || len(client.reads) != 0 || len(client.entries) != 2 {
		t.Fatalf("non-recursive deletion traversed directory: %+v, error = %v", final, err)
	}
}

func TestNativeSFTPDeletePipelinesAndCancelsBlockedRequests(t *testing.T) {
	for _, cancelDelete := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelDelete), func(t *testing.T) {
			threshold := int32(sftpDeleteConcurrency)
			if cancelDelete {
				threshold = 0
			}
			server, gates := gatedSFTPServer(t, 13, threshold) // SSH_FXP_REMOVE
			transport, connection := testSFTPConnection(t, server)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			lease, err := transport.openSFTP(ctx, connection)
			if err != nil {
				t.Fatal(err)
			}
			gate := <-gates
			defer gate.unblock()
			_ = lease.Close()
			directory := filepath.Join(server.root, "delete-tree")
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			for index := range 32 {
				if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("%d.txt", index)), []byte("test"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var progress SFTPDeleteProgress
			done := make(chan error, 1)
			go func() {
				_, err := transport.RemoveSFTPEntry(ctx, connection, testSFTPPath(directory), true, func(update SFTPDeleteProgress) { progress = update })
				done <- err
			}()
			awaitSFTP(t, gate.entered, "first deletion packet")
			if cancelDelete {
				cancel()
			}
			select {
			case err := <-done:
				if cancelDelete {
					if !errors.Is(err, context.Canceled) || progress.RootRemoved {
						t.Fatalf("blocked cancellation = %+v, %v", progress, err)
					}
				} else if err != nil || progress.Removed != 33 || !progress.RootRemoved {
					t.Fatalf("pipelined deletion = %+v, %v", progress, err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("deletion remained blocked")
			}
			if !cancelDelete && gate.count.Load() != 32 {
				t.Fatalf("remove packets = %d", gate.count.Load())
			}
			// A cancelled lease must not poison subsequent directory requests.
			if _, err := transport.ListSFTPFiles(context.Background(), connection, testSFTPPath(server.root)); err != nil {
				t.Fatalf("list after delete: %v", err)
			}
		})
	}
}
