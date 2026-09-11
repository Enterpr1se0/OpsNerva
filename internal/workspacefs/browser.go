package workspacefs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

const MaxPreviewBytes int64 = 1 << 20

type Preview struct {
	Size      int64
	SHA256    string
	Content   string
	Binary    bool
	Truncated bool
}

type Download struct {
	Info   os.FileInfo
	Reader io.ReadCloser
}

// ReadDir filters private Workspace names and symlinks. Callers retain their
// existing display policy for ordering and special file types.
func (fs *FS) ReadDir(relative string) ([]os.FileInfo, error) {
	directory, err := fs.Resolve(relative, false)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	result := make([]os.FileInfo, 0, len(entries))
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || IsSensitiveComponent(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		result = append(result, info)
	}
	return result, nil
}

func (fs *FS) Preview(relativePath string) (Preview, error) {
	relativePath = strings.TrimSpace(relativePath)
	path, err := fs.Resolve(relativePath, false)
	if err != nil {
		return Preview{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return Preview{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return Preview{}, fmt.Errorf("workspace preview target is not a regular file")
	}
	digest := sha256.New()
	hashed := io.TeeReader(file, digest)
	data, err := io.ReadAll(io.LimitReader(hashed, MaxPreviewBytes+1))
	if err != nil {
		return Preview{}, err
	}
	truncated := int64(len(data)) > MaxPreviewBytes
	if truncated {
		data = data[:MaxPreviewBytes]
	}
	if _, err := io.Copy(io.Discard, hashed); err != nil {
		return Preview{}, err
	}
	binary := bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data)
	result := Preview{
		Size: info.Size(), SHA256: hex.EncodeToString(digest.Sum(nil)),
		Binary: binary, Truncated: truncated,
	}
	if !binary {
		result.Content = string(data)
	}
	return result, nil
}

func (fs *FS) OpenDownload(relativePath string) (Download, error) {
	relativePath = strings.TrimSpace(relativePath)
	path, err := fs.Resolve(relativePath, false)
	if err != nil {
		return Download{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return Download{}, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return Download{}, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return Download{}, fmt.Errorf("workspace download target is not a regular file")
	}
	return Download{Info: info, Reader: file}, nil
}
