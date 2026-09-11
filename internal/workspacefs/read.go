package workspacefs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

type ReadResult struct {
	Info    os.FileInfo
	Offset  int64
	SHA256  string
	Content []byte
}

func (fs *FS) Read(relative string, maxBytes int, offset int64, tailLines int) (ReadResult, error) {
	if maxBytes < 0 || tailLines < 0 || (offset != 0 && tailLines != 0) {
		return ReadResult{}, fmt.Errorf("invalid Workspace file read range: max_bytes and tail_lines must be non-negative; tail_lines cannot be combined with offset_bytes")
	}
	path, err := fs.Resolve(relative, false)
	if err != nil {
		return ReadResult{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return ReadResult{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return ReadResult{}, fmt.Errorf("workspace target is not a regular file")
	}
	resolvedOffset := offset
	if offset < 0 {
		resolvedOffset = max(0, info.Size()+offset)
	}
	if tailLines > 0 {
		resolvedOffset, err = workspaceTailOffset(file, info.Size(), tailLines)
		if err != nil {
			return ReadResult{}, err
		}
	}
	if _, err := file.Seek(resolvedOffset, io.SeekStart); err != nil {
		return ReadResult{}, err
	}
	var content []byte
	if maxBytes > 0 {
		content, err = io.ReadAll(io.LimitReader(file, int64(maxBytes)))
	} else {
		content, err = io.ReadAll(file)
	}
	if err != nil {
		return ReadResult{}, err
	}
	digest := sha256.New()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return ReadResult{}, err
	}
	if _, err := io.Copy(digest, file); err != nil {
		return ReadResult{}, err
	}
	return ReadResult{Info: info, Offset: resolvedOffset, SHA256: hex.EncodeToString(digest.Sum(nil)), Content: content}, nil
}

func workspaceTailOffset(file *os.File, size int64, lines int) (int64, error) {
	if lines <= 0 || size <= 0 {
		return 0, nil
	}
	const blockSize int64 = 32 << 10
	remaining := lines
	position := size
	ignoreTrailingNewline := true
	buffer := make([]byte, blockSize)
	for position > 0 {
		start := position - blockSize
		if start < 0 {
			start = 0
		}
		length := position - start
		read, err := file.ReadAt(buffer[:length], start)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		for index := read - 1; index >= 0; index-- {
			if buffer[index] != '\n' {
				ignoreTrailingNewline = false
				continue
			}
			if ignoreTrailingNewline {
				ignoreTrailingNewline = false
				continue
			}
			remaining--
			if remaining == 0 {
				return start + int64(index) + 1, nil
			}
		}
		position = start
	}
	return 0, nil
}
