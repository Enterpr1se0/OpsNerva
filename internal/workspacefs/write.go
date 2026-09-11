package workspacefs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Enterpr1se0/opsnerva/internal/ids"
)

const MaxTextFileBytes = 100 << 20

type WriteResult struct {
	Size   int64
	SHA256 string
}

func (fs *FS) SaveText(ctx context.Context, relativePath, content string) (WriteResult, error) {
	if len(content) > MaxTextFileBytes {
		return WriteResult{}, fmt.Errorf("workspace text file exceeds 100 MiB")
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	relativePath = strings.TrimSpace(relativePath)
	path, err := fs.Resolve(relativePath, false)
	if err != nil {
		return WriteResult{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return WriteResult{}, err
	}
	if !info.Mode().IsRegular() {
		return WriteResult{}, fmt.Errorf("workspace edit target is not a regular file")
	}
	if info.Size() > MaxTextFileBytes {
		return WriteResult{}, fmt.Errorf("workspace text file exceeds 100 MiB")
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return WriteResult{}, err
	}
	if bytes.IndexByte(current, 0) >= 0 || !utf8.Valid(current) {
		return WriteResult{}, fmt.Errorf("workspace edit target is binary")
	}
	suffix := time.Now().UTC().Format("20060102T150405Z") + "-" + ids.New("file")
	temporary := filepath.Join(filepath.Dir(path), ".opsnerva-"+filepath.Base(path)+"-"+suffix+".tmp")
	if err := WriteSyncedFile(temporary, []byte(content), info.Mode().Perm()); err != nil {
		return WriteResult{}, err
	}
	defer os.Remove(temporary)
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	if err := os.Rename(temporary, path); err != nil {
		return WriteResult{}, err
	}
	if err := SyncDirectory(filepath.Dir(path)); err != nil {
		return WriteResult{}, err
	}
	digest := sha256.Sum256([]byte(content))
	return WriteResult{
		Size: int64(len(content)), SHA256: hex.EncodeToString(digest[:]),
	}, nil
}

// WriteSyncedFile creates a staging file at an already validated path. It syncs
// content and permissions, and never replaces an existing file.
func WriteSyncedFile(path string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(content); err != nil {
		file.Close()
		return err
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	succeeded = true
	return nil
}
