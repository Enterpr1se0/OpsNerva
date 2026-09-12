package workspacefs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type DeleteResult struct {
	Path   string
	Type   string
	Size   int64
	SHA256 string
}

// ValidateDeleteTarget checks explicit recursive intent without computing a
// digest. Delete repeats this check against the current filesystem state.
func (fs *FS) ValidateDeleteTarget(relativePath string, recursive bool) error {
	_, _, err := fs.deleteTarget(relativePath, recursive)
	return err
}

func (fs *FS) deleteTarget(relativePath string, recursive bool) (string, os.FileInfo, error) {
	relativePath = strings.TrimSpace(relativePath)
	if relativePath == "" || relativePath == "." {
		return "", nil, fmt.Errorf("Workspace root cannot be deleted")
	}
	path, err := fs.Resolve(relativePath, false)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return "", nil, fmt.Errorf("only regular Workspace files and directories can be deleted")
	}
	if info.IsDir() && !recursive {
		directory, err := os.Open(path)
		if err != nil {
			return "", nil, err
		}
		entries, readErr := directory.ReadDir(1)
		closeErr := directory.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", nil, readErr
		}
		if closeErr != nil {
			return "", nil, closeErr
		}
		if len(entries) != 0 {
			return "", nil, fmt.Errorf("workspace directory is not empty; set recursive=true to delete it")
		}
	}
	return path, info, nil
}

// Delete preserves the file digest used by audit. Cancellation is checked while
// hashing and immediately before removal; RemoveAll itself is not transactional.
func (fs *FS) Delete(ctx context.Context, relativePath string, recursive bool) (DeleteResult, error) {
	if err := ctx.Err(); err != nil {
		return DeleteResult{}, err
	}
	path, info, err := fs.deleteTarget(relativePath, recursive)
	if err != nil {
		return DeleteResult{}, err
	}
	entryType := "directory"
	var size int64
	var sha256Sum string
	if info.Mode().IsRegular() {
		entryType = "file"
		size = info.Size()
		file, err := os.Open(path)
		if err != nil {
			return DeleteResult{}, err
		}
		digest := sha256.New()
		_, copyErr := copyWithContext(ctx, digest, file)
		closeErr := file.Close()
		if copyErr != nil {
			return DeleteResult{}, copyErr
		}
		if closeErr != nil {
			return DeleteResult{}, closeErr
		}
		sha256Sum = hex.EncodeToString(digest.Sum(nil))
	}
	if err := ctx.Err(); err != nil {
		return DeleteResult{}, err
	}
	normalizedPath := filepath.ToSlash(filepath.Clean(strings.TrimSpace(relativePath)))
	if info.IsDir() && recursive {
		err = os.RemoveAll(path)
	} else {
		err = os.Remove(path)
	}
	if err != nil {
		return DeleteResult{}, err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return DeleteResult{}, err
	}
	return DeleteResult{Path: normalizedPath, Type: entryType, Size: size, SHA256: sha256Sum}, nil
}
