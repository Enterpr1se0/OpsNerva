package sshx

import (
	"context"
	"fmt"
	"os"
	"path"
	"sync"
	"time"
)

const sftpDeleteConcurrency = 8
const sftpDeleteProgressInterval = 200 * time.Millisecond

type SFTPDeleteProgress struct {
	Discovered   int64  `json:"discovered"`
	Removed      int64  `json:"removed"`
	Failed       int64  `json:"failed"`
	Skipped      int64  `json:"skipped"`
	CurrentPath  string `json:"current_path,omitempty"`
	FirstError   string `json:"first_error,omitempty"`
	ScanComplete bool   `json:"scan_complete"`
	RootRemoved  bool   `json:"root_removed"`
}

func ValidateSFTPDeletePath(remotePath string) error {
	if _, err := cleanSFTPPath(remotePath); err != nil {
		return err
	}
	if remotePath == "/" {
		return fmt.Errorf("remote root cannot be deleted")
	}
	return nil
}

type sftpDeleteClient interface {
	ReadDirContext(context.Context, string) ([]os.FileInfo, error)
	Remove(string) error
	RemoveDirectory(string) error
}

type sftpDeleteJob struct {
	path      string
	directory bool
}

// Directory reads discover files incrementally. Only directory paths are kept
// for bottom-up removal; there is no whole-tree pre-scan or goroutine per file.
func deleteSFTPTree(ctx context.Context, client sftpDeleteClient, root string, directory, recursive bool, report func(SFTPDeleteProgress)) error {
	var mu sync.Mutex
	progress := SFTPDeleteProgress{Discovered: 1, CurrentPath: root}
	blocked := make(map[string]bool)
	lastReport := time.Time{}
	emit := func(force bool) {
		if report != nil && (force || time.Since(lastReport) >= sftpDeleteProgressInterval) {
			lastReport = time.Now()
			report(progress)
		}
	}
	fail := func(target string, err error) {
		progress.Failed++
		if progress.FirstError == "" {
			progress.FirstError = fmt.Sprintf("%s: %v", target, err)
		}
		if target != root {
			for parent := path.Dir(target); ; parent = path.Dir(parent) {
				blocked[parent] = true
				if parent == root || parent == "/" {
					break
				}
			}
		}
	}
	jobs := make(chan sftpDeleteJob, sftpDeleteConcurrency)
	var pending, workers sync.WaitGroup
	for range sftpDeleteConcurrency {
		workers.Go(func() {
			for job := range jobs {
				if ctx.Err() == nil {
					var err error
					if job.directory {
						err = client.RemoveDirectory(job.path)
					} else {
						err = client.Remove(job.path)
					}
					mu.Lock()
					progress.CurrentPath = job.path
					if err == nil {
						progress.Removed++
						progress.RootRemoved = progress.RootRemoved || job.path == root
					} else if ctx.Err() == nil {
						fail(job.path, err)
					}
					emit(false)
					mu.Unlock()
				}
				pending.Done()
			}
		})
	}
	defer func() { close(jobs); workers.Wait() }()
	enqueue := func(job sftpDeleteJob) {
		pending.Add(1)
		select {
		case jobs <- job:
		case <-ctx.Done():
			pending.Done()
		}
	}
	emit(true)
	if !directory || !recursive {
		enqueue(sftpDeleteJob{path: root, directory: directory})
		mu.Lock()
		progress.ScanComplete = true
		mu.Unlock()
	} else {
		type directoryItem struct {
			path  string
			depth int
		}
		stack := []directoryItem{{path: root}}
		var levels [][]string
		for len(stack) > 0 && ctx.Err() == nil {
			item := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			entries, err := client.ReadDirContext(ctx, item.path)
			mu.Lock()
			progress.CurrentPath = item.path
			if err != nil && ctx.Err() == nil {
				fail(item.path, err)
			}
			if err == nil {
				progress.Discovered += int64(len(entries))
			}
			emit(false)
			mu.Unlock()
			if err != nil {
				continue
			}
			for len(levels) <= item.depth {
				levels = append(levels, nil)
			}
			levels[item.depth] = append(levels[item.depth], item.path)
			for _, entry := range entries {
				if ctx.Err() != nil {
					break
				}
				child := path.Join(item.path, entry.Name())
				if entry.IsDir() && entry.Mode()&os.ModeSymlink == 0 {
					stack = append(stack, directoryItem{path: child, depth: item.depth + 1})
				} else {
					enqueue(sftpDeleteJob{path: child})
				}
			}
		}
		pending.Wait()
		mu.Lock()
		progress.ScanComplete = ctx.Err() == nil
		emit(true)
		mu.Unlock()
		for depth := len(levels) - 1; depth >= 0 && ctx.Err() == nil; depth-- {
			for _, target := range levels[depth] {
				if ctx.Err() != nil {
					break
				}
				mu.Lock()
				skip := blocked[target]
				if skip {
					progress.Skipped++
				}
				mu.Unlock()
				if !skip {
					enqueue(sftpDeleteJob{path: target, directory: true})
				}
			}
			// No parent can be queued while a child directory is still in flight.
			pending.Wait()
		}
	}
	pending.Wait()
	emit(true)
	if progress.RootRemoved {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("delete incomplete: %d removed, %d failed, %d skipped; %s", progress.Removed, progress.Failed, progress.Skipped, progress.FirstError)
}
