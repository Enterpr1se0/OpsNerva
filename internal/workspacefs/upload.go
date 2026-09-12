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

	"github.com/Enterpr1se0/opsnerva/internal/transfer"
)

type UploadOptions struct {
	ExpectedSHA256 string
	Total          int64
	Progress       transfer.Reporter
}

type UploadResult struct {
	Path string
	WriteResult
}

type SHA256MismatchError struct {
	Expected string
	Actual   string
}

func (err *SHA256MismatchError) Error() string {
	return fmt.Sprintf("source version conflict: expected SHA256 %s, got %s", err.Expected, err.Actual)
}

// ValidateUploadDestination is a preflight check, not a reservation. Upload
// resolves the destination again and commits without replacing existing files.
func (fs *FS) ValidateUploadDestination(targetPath, originalFilename string) (string, error) {
	relative, _, err := fs.uploadDestination(targetPath, originalFilename)
	return relative, err
}

func (fs *FS) uploadDestination(targetPath, originalFilename string) (string, string, error) {
	targetPath = strings.TrimSpace(targetPath)
	if targetPath == "" {
		targetPath = filepath.Base(strings.ReplaceAll(originalFilename, "\\", "/"))
	}
	if targetPath == "" || targetPath == "." || len(targetPath) > 1024 {
		return "", "", fmt.Errorf("invalid workspace destination path")
	}
	target, err := fs.Resolve(targetPath, true)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("workspace destination parent directory does not exist")
		}
		return "", "", err
	}
	if _, err := os.Lstat(target); err == nil {
		return "", "", fmt.Errorf("workspace file already exists; choose a new path instead of overwriting it")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	parent := filepath.Dir(target)
	parentInfo, err := os.Stat(parent)
	if err != nil || !parentInfo.IsDir() {
		return "", "", fmt.Errorf("workspace destination parent directory does not exist")
	}
	return targetPath, target, nil
}

// Upload streams a source into a synced staging file in the destination
// directory, verifies its optional digest, then creates the target exclusively.
// The source owner is responsible for interrupting an in-progress blocking Read.
func (fs *FS) Upload(ctx context.Context, targetPath, originalFilename string, source io.Reader, options UploadOptions) (UploadResult, error) {
	if err := ctx.Err(); err != nil {
		return UploadResult{}, err
	}
	relative, target, err := fs.uploadDestination(targetPath, originalFilename)
	if err != nil {
		return UploadResult{}, err
	}
	parent := filepath.Dir(target)
	temporary, err := os.CreateTemp(parent, ".opsnerva-upload-*")
	if err != nil {
		return UploadResult{}, err
	}
	temporaryPath := temporary.Name()
	defer func() {
		temporary.Close()
		os.Remove(temporaryPath)
	}()
	digest := sha256.New()
	progressWriter := transfer.NewWriter(io.MultiWriter(temporary, digest), options.Total, options.Progress)
	written, copyErr := copyWithContext(ctx, progressWriter, source)
	progressWriter.Finish()
	if copyErr != nil {
		return UploadResult{}, copyErr
	}
	actualSHA256 := hex.EncodeToString(digest.Sum(nil))
	if options.ExpectedSHA256 != "" && actualSHA256 != options.ExpectedSHA256 {
		return UploadResult{}, &SHA256MismatchError{Expected: options.ExpectedSHA256, Actual: actualSHA256}
	}
	if err := temporary.Chmod(0o644); err != nil {
		return UploadResult{}, err
	}
	if err := temporary.Sync(); err != nil {
		return UploadResult{}, err
	}
	if err := temporary.Close(); err != nil {
		return UploadResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return UploadResult{}, err
	}
	if err := os.Link(temporaryPath, target); err != nil {
		if errors.Is(err, os.ErrExist) {
			return UploadResult{}, fmt.Errorf("workspace file already exists; choose a new path instead of overwriting it")
		}
		return UploadResult{}, err
	}
	if err := syncDirectory(parent); err != nil {
		_ = os.Remove(target)
		return UploadResult{}, err
	}
	return UploadResult{Path: relative, WriteResult: WriteResult{Size: written, SHA256: actualSHA256}}, nil
}
