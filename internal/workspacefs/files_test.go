package workspacefs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestReadRangesReturnWholeFileDigest(t *testing.T) {
	root := t.TempDir()
	content := "first\r\nsecond\r\nlast"
	if err := os.WriteFile(filepath.Join(root, "lines.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := New(root)
	digest := sha256.Sum256([]byte(content))
	for _, test := range []struct {
		name       string
		maxBytes   int
		offset     int64
		tail       int
		want       string
		wantOffset int64
	}{
		{name: "complete", want: content},
		{name: "bounded", maxBytes: 5, want: "first"},
		{name: "negative offset", offset: -4, want: "last", wantOffset: 15},
		{name: "tail", tail: 2, want: "second\r\nlast", wantOffset: 7},
		{name: "past start", offset: -100, want: content},
		{name: "past end", offset: 100, wantOffset: 100},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := fs.Read("lines.txt", test.maxBytes, test.offset, test.tail)
			if err != nil {
				t.Fatal(err)
			}
			if string(result.Content) != test.want || result.Offset != test.wantOffset || result.Info.Size() != int64(len(content)) || result.SHA256 != hex.EncodeToString(digest[:]) {
				t.Fatalf("unexpected read: %+v content=%q", result, result.Content)
			}
		})
	}
	if _, err := fs.Read("lines.txt", 0, 1, 1); err == nil {
		t.Fatal("combined offset and tail accepted")
	}
	if _, err := fs.Read(".", 0, 0, 0); err == nil {
		t.Fatal("directory read accepted")
	}
}

func TestBrowserFiltersPrivateNamesAndKeepsDownloadBytes(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"visible.txt", ".env", "credentials.txt", "master.key", ".opsnerva-file.tmp"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("\x00raw\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	fs := New(root)
	entries, err := fs.ReadDir(".")
	if err != nil || len(entries) != 2 || entries[0].Name() != "src" || entries[1].Name() != "visible.txt" {
		t.Fatalf("directory entries: %v err=%v", entries, err)
	}
	preview, err := fs.Preview("visible.txt")
	if err != nil || !preview.Binary || preview.Content != "" {
		t.Fatalf("binary preview: %+v err=%v", preview, err)
	}
	download, err := fs.OpenDownload("visible.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer download.Reader.Close()
	content, err := io.ReadAll(download.Reader)
	if err != nil || string(content) != "\x00raw\r\n" || download.Info.Name() != "visible.txt" {
		t.Fatalf("download changed bytes: %q err=%v", content, err)
	}
	if _, err := fs.OpenDownload("src"); err == nil {
		t.Fatal("directory download accepted")
	}
}

func TestPreviewIsBoundedAndHashesWholeFile(t *testing.T) {
	root := t.TempDir()
	content := bytes.Repeat([]byte("x"), int(MaxPreviewBytes)+100)
	if err := os.WriteFile(filepath.Join(root, "large.txt"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := New(root).Preview("large.txt")
	digest := sha256.Sum256(content)
	if err != nil || result.Binary || !result.Truncated || int64(len(result.Content)) != MaxPreviewBytes || result.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("preview: size=%d truncated=%v sha=%s err=%v", len(result.Content), result.Truncated, result.SHA256, err)
	}
}

func TestSearchKeepsLiteralRegexAndContextSemantics(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "log.txt"), []byte("before\na.b\naXb\nafter\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := New(root)
	literal, err := fs.Search("log.txt", "a.b", domain.FileSearchLiteral, 1)
	if err != nil || string(literal) != "1-before\n2:a.b\n3-aXb\n" {
		t.Fatalf("literal search: %q err=%v", literal, err)
	}
	regex, err := fs.Search("log.txt", "a.b", domain.FileSearchRegex, 0)
	if err != nil || string(regex) != "2:a.b\n3:aXb\n" {
		t.Fatalf("regex search: %q err=%v", regex, err)
	}
	if _, err := fs.Search("log.txt", "[", domain.FileSearchRegex, 0); err == nil {
		t.Fatal("invalid regex accepted")
	}
	if _, err := fs.Search("log.txt", "text", domain.FileSearchLiteral, -1); err == nil {
		t.Fatal("negative context accepted")
	}
}

func TestSaveTextPreservesBytesModeAndCancellation(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "edit.txt")
	if err := os.WriteFile(target, []byte("original"), 0o640); err != nil {
		t.Fatal(err)
	}
	fs := New(root)
	content := "\xef\xbb\xbfupdated\r\nno-final-newline"
	result, err := fs.SaveText(context.Background(), "edit.txt", content)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(content))
	if result.Size != int64(len(content)) || result.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("save result: %+v", result)
	}
	actual, err := os.ReadFile(target)
	if err != nil || string(actual) != content {
		t.Fatalf("saved content: %q err=%v", actual, err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed: %v", info.Mode())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fs.SaveText(ctx, "edit.txt", "cancelled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled save: %v", err)
	}
	if err := writeSyncedFile(target, []byte("overwrite"), 0o600); !errors.Is(err, os.ErrExist) {
		t.Fatalf("exclusive write: %v", err)
	}
	actual, err = os.ReadFile(target)
	if err != nil || string(actual) != content {
		t.Fatal("failed write touched original")
	}
	if err := os.WriteFile(filepath.Join(root, "binary"), []byte{0, 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.SaveText(context.Background(), "binary", "text"); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("binary save: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".opsnerva-") {
			t.Fatalf("temporary file was not removed: %s", entry.Name())
		}
	}
}
