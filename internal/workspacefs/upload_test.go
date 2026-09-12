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

	"github.com/Enterpr1se0/opsnerva/internal/transfer"
)

type uploadReadFunc func([]byte) (int, error)

func (read uploadReadFunc) Read(buffer []byte) (int, error) { return read(buffer) }

func TestUploadStreamsBytesAndCoalescedProgress(t *testing.T) {
	root := t.TempDir()
	content := bytes.Repeat([]byte("binary\x00\r\n"), 200_000)
	digest := sha256.Sum256(content)
	wantSHA := hex.EncodeToString(digest[:])
	source := bytes.NewReader(content)
	var events []transfer.Progress
	result, err := New(root).Upload(context.Background(), "", `C:\fakepath\payload.bin`, uploadReadFunc(func(buffer []byte) (int, error) {
		if _, err := os.Stat(filepath.Join(root, "payload.bin")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("upload exposed target before copy completed: %v", err)
		}
		return source.Read(buffer[:min(len(buffer), 1024)])
	}), UploadOptions{ExpectedSHA256: wantSHA, Total: int64(len(content)), Progress: func(event transfer.Progress) { events = append(events, event) }})
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != "payload.bin" || result.Size != int64(len(content)) || result.SHA256 != wantSHA {
		t.Fatalf("upload result: %+v", result)
	}
	actual, err := os.ReadFile(filepath.Join(root, result.Path))
	if err != nil || !bytes.Equal(actual, content) {
		t.Fatalf("binary upload changed bytes: size=%d err=%v", len(actual), err)
	}
	if len(events) < 2 || events[0].Transferred != 0 || events[len(events)-1].Transferred != int64(len(content)) || len(events) > len(content)/(256*1024)+2 {
		t.Fatalf("progress no longer coalesced: %+v", events)
	}
	for index, event := range events {
		if event.Total != int64(len(content)) || index > 0 && event.Transferred <= events[index-1].Transferred {
			t.Fatalf("invalid progress sequence: %+v", events)
		}
	}
	info, err := os.Stat(filepath.Join(root, result.Path))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o644 {
		t.Fatalf("upload mode = %v", info.Mode())
	}
	assertNoStagedEdits(t, root)
}

func TestUploadFailuresCleanStagingWithoutOverwriting(t *testing.T) {
	for _, action := range []string{"cancel before read", "cancel during read", "cancel at final progress", "read error", "digest mismatch", "intervening target"} {
		t.Run(action, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "upload.txt")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			readError := errors.New("fixture interrupted the stream")
			options := UploadOptions{Total: 3}
			if action == "cancel before read" {
				cancel()
			}
			if action == "digest mismatch" {
				options.ExpectedSHA256 = strings.Repeat("0", 64)
			}
			if action == "cancel at final progress" {
				options.Progress = func(event transfer.Progress) {
					if event.Transferred == 3 {
						cancel()
					}
				}
			}
			reads := 0
			source := uploadReadFunc(func(buffer []byte) (int, error) {
				reads++
				if reads > 1 {
					return 0, io.EOF
				}
				switch action {
				case "cancel before read":
					t.Fatal("cancelled upload consumed source")
				case "cancel during read":
					cancel()
				case "read error":
					return copy(buffer, "new"), readError
				case "intervening target":
					if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return copy(buffer, "new"), io.EOF
			})
			_, err := New(root).Upload(ctx, "upload.txt", "", source, options)
			if err == nil {
				t.Fatal("failed upload reported success")
			}
			switch action {
			case "cancel before read", "cancel during read", "cancel at final progress":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel error identity lost: %v", err)
				}
			case "read error":
				if !errors.Is(err, readError) {
					t.Fatalf("read error identity lost: %v", err)
				}
			case "digest mismatch":
				var mismatch *SHA256MismatchError
				digest := sha256.Sum256([]byte("new"))
				if !errors.As(err, &mismatch) || mismatch.Expected != options.ExpectedSHA256 || mismatch.Actual != hex.EncodeToString(digest[:]) {
					t.Fatalf("digest mismatch lost details: %v", err)
				}
			case "intervening target":
				if !strings.Contains(err.Error(), "already exists") {
					t.Fatalf("conflict error changed: %v", err)
				}
			}
			if action == "intervening target" {
				assertEditContent(t, target, "keep")
			} else if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed upload left a target: %v", err)
			}
			assertNoStagedEdits(t, root)
		})
	}
}

func TestUploadPreflightDoesNotReserveDestination(t *testing.T) {
	root := t.TempDir()
	fs := New(root)
	path, err := fs.ValidateUploadDestination(" incoming.txt ", "ignored")
	if err != nil || path != "incoming.txt" {
		t.Fatalf("preflight: path=%q err=%v", path, err)
	}
	if err := os.WriteFile(filepath.Join(root, path), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = fs.Upload(context.Background(), path, "", uploadReadFunc(func([]byte) (int, error) {
		t.Fatal("existing destination consumed upload stream")
		return 0, io.EOF
	}), UploadOptions{})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("stale preflight allowed overwrite: %v", err)
	}
	assertEditContent(t, filepath.Join(root, path), "keep")
	for _, path := range []string{"../escape", "/absolute", ".env", ".ssh/key", "missing/file", `nested\windows`, "."} {
		if _, err := fs.ValidateUploadDestination(path, ""); err == nil {
			t.Errorf("unsafe destination accepted: %q", path)
		}
	}
	assertNoStagedEdits(t, root)
}
