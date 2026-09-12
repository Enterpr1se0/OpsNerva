package workspacefs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/fileedit"
	"github.com/Enterpr1se0/opsnerva/internal/ids"
)

// EditConflictError means the requested block or create destination is no longer
// applicable. The caller must reread the target before proposing another edit.
type EditConflictError struct{ Message string }

func (err *EditConflictError) Error() string { return err.Message }

// Edit owns one staged replacement. The caller may validate StagedPath before
// Commit and must defer Close after preparation. It is used by a single caller;
// the content recheck is conflict detection, not a filesystem-wide lock or CAS.
type Edit struct {
	target         string
	staged         string
	existed        bool
	originalDigest [sha256.Size]byte
	result         WriteResult
}

// PrepareEdit stages a normalized fileedit.Build result without changing the
// target. Approval, write access and validator selection belong to the caller.
func (fs *FS) PrepareEdit(ctx context.Context, relative string, edit domain.TextEdit) (*Edit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := fs.Resolve(relative, true)
	if err != nil {
		return nil, err
	}
	info, statErr := os.Stat(path)
	existed := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	creating := edit.OldText == ""
	if !existed && !creating {
		return nil, fmt.Errorf("workspace edit target does not exist")
	}
	if existed && creating {
		return nil, &EditConflictError{Message: "workspace create target already exists"}
	}
	if existed && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("workspace edit target is not a regular file")
	}
	prepared := &Edit{target: path, existed: existed}
	mode := os.FileMode(0o600)
	var updated []byte
	if existed {
		mode = info.Mode().Perm()
		original, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		prepared.originalDigest = sha256.Sum256(original)
		updated, err = fileedit.Apply(original, edit)
		if err != nil {
			return nil, &EditConflictError{Message: err.Error()}
		}
	} else {
		updated = []byte(edit.NewText)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	suffix := time.Now().UTC().Format("20060102T150405Z") + "-" + ids.New("file")
	prepared.staged = filepath.Join(filepath.Dir(path), ".opsnerva-"+filepath.Base(path)+"-"+suffix+".tmp")
	if err := writeSyncedFile(prepared.staged, updated, mode); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(updated)
	prepared.result = WriteResult{Size: int64(len(updated)), SHA256: hex.EncodeToString(digest[:])}
	return prepared, nil
}

func (edit *Edit) StagedPath() string { return edit.staged }

// Commit replaces an existing target only if its bytes still match the original.
// Creation uses an exclusive hard link so an intervening file is never replaced.
func (edit *Edit) Commit(ctx context.Context) (WriteResult, error) {
	if edit.staged == "" {
		return WriteResult{}, os.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	if edit.existed {
		current, err := os.ReadFile(edit.target)
		if err != nil {
			return WriteResult{}, err
		}
		if sha256.Sum256(current) != edit.originalDigest {
			return WriteResult{}, &EditConflictError{Message: "workspace file edit conflict: target changed during validation"}
		}
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	if edit.existed {
		if err := os.Rename(edit.staged, edit.target); err != nil {
			return WriteResult{}, err
		}
	} else if err := os.Link(edit.staged, edit.target); err != nil {
		if errors.Is(err, os.ErrExist) {
			return WriteResult{}, &EditConflictError{Message: "workspace file edit conflict: create target appeared during validation"}
		}
		return WriteResult{}, err
	}
	if err := edit.Close(); err != nil {
		return WriteResult{}, fmt.Errorf("workspace file was committed but removing its staging file failed: %w", err)
	}
	if err := syncDirectory(filepath.Dir(edit.target)); err != nil {
		return WriteResult{}, fmt.Errorf("workspace file was committed but syncing its directory failed: %w", err)
	}
	return edit.result, nil
}

// Close discards the staged file without modifying the target. It is safe after
// Commit or a previous Close, including when Commit failed.
func (edit *Edit) Close() error {
	if edit.staged == "" {
		return nil
	}
	err := os.Remove(edit.staged)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	edit.staged = ""
	return nil
}
