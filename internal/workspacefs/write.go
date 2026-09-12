package workspacefs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

// copyWithContext checks cancellation between writes without starting a worker
// that could outlive its caller. The source must unblock its own pending reads.
func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	written, err := io.Copy(contextWriter{ctx: ctx, destination: destination}, source)
	if err != nil {
		return written, err
	}
	return written, ctx.Err()
}

type contextWriter struct {
	ctx         context.Context
	destination io.Writer
}

func (writer contextWriter) Write(content []byte) (int, error) {
	if err := writer.ctx.Err(); err != nil {
		return 0, err
	}
	return writer.destination.Write(content)
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
	if err := writeSyncedFile(temporary, []byte(content), info.Mode().Perm()); err != nil {
		return WriteResult{}, err
	}
	defer os.Remove(temporary)
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	if err := os.Rename(temporary, path); err != nil {
		return WriteResult{}, err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return WriteResult{}, err
	}
	digest := sha256.Sum256([]byte(content))
	return WriteResult{
		Size: int64(len(content)), SHA256: hex.EncodeToString(digest[:]),
	}, nil
}

// writeSyncedFile creates a staging file at an already validated path. It syncs
// content and permissions, and never replaces an existing file.
func writeSyncedFile(path string, content []byte, mode os.FileMode) error {
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

// CreateDirectory creates one directory. An existing directory is
// accepted so folder uploads can merge trees without overwriting existing files.
func (fs *FS) CreateDirectory(ctx context.Context, relativePath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	relativePath = strings.TrimSpace(relativePath)
	if relativePath == "" || relativePath == "." || len(relativePath) > 1024 {
		return fmt.Errorf("invalid workspace directory path")
	}
	target, err := fs.Resolve(relativePath, true)
	if err != nil {
		return err
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		info, statErr := os.Stat(target)
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() {
			return fmt.Errorf("workspace directory conflicts with an existing file")
		}
		return nil
	}
	return syncDirectory(filepath.Dir(target))
}
