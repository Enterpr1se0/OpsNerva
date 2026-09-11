package workspacefs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/fileedit"
)

func TestEditStagesAndCommitsExactBytes(t *testing.T) {
	for _, test := range []struct{ name, original, oldText, newText, want string }{
		{"LF", "before\nold\nafter\n", "old", "new", "before\nnew\nafter\n"},
		{"CRLF with BOM", "\ufeffold\r\ntail\r\n", "old", "new", "\ufeffnew\r\ntail\r\n"},
		{"no final newline", "old", "old", "new", "new"},
		{"delete final line", "first\r\nlast", "last", "", "first"},
		{"insert lines", "old\r\n", "old", "new\nextra", "new\r\nextra\r\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "app.conf")
			if err := os.WriteFile(target, []byte(test.original), 0o640); err != nil {
				t.Fatal(err)
			}
			request, _, err := fileedit.Build("app.conf", test.oldText, test.newText)
			if err != nil {
				t.Fatal(err)
			}
			edit, err := New(root).PrepareEdit(context.Background(), "app.conf", request)
			if err != nil {
				t.Fatal(err)
			}
			defer edit.Close()
			assertEditContent(t, target, test.original)
			staged := edit.StagedPath()
			assertEditContent(t, staged, test.want)
			result, err := edit.Commit(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			assertEditContent(t, target, test.want)
			digest := sha256.Sum256([]byte(test.want))
			if result.Size != int64(len(test.want)) || result.SHA256 != hex.EncodeToString(digest[:]) {
				t.Fatalf("commit metadata = %+v", result)
			}
			info, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
				t.Fatalf("mode changed: %v", info.Mode())
			}
			if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("staging file remains: %v", err)
			}
			if _, err := edit.Commit(context.Background()); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("edit committed twice: %v", err)
			}
			if err := edit.Close(); err != nil {
				t.Fatal(err)
			}
			assertNoStagedEdits(t, root)
		})
	}
}

func TestEditCreateDoesNotOverwriteInterveningFile(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		name := "create"
		if conflict {
			name = "intervening create"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "created.conf")
			request, _, err := fileedit.Build("created.conf", "", "new")
			if err != nil {
				t.Fatal(err)
			}
			edit, err := New(root).PrepareEdit(context.Background(), "created.conf", request)
			if err != nil {
				t.Fatal(err)
			}
			defer edit.Close()
			if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("prepare created target: %v", err)
			}
			if conflict {
				if err := os.WriteFile(target, []byte("another writer"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err = edit.Commit(context.Background())
			if conflict {
				var conflictErr *EditConflictError
				if !errors.As(err, &conflictErr) || !strings.Contains(err.Error(), "appeared during validation") {
					t.Fatalf("create conflict = %v", err)
				}
				assertEditContent(t, target, "another writer")
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assertEditContent(t, target, "new")
			}
			if err := edit.Close(); err != nil {
				t.Fatal(err)
			}
			assertNoStagedEdits(t, root)
		})
	}
}

func TestEditCancelledDiscardedOrConflictedKeepsTarget(t *testing.T) {
	for _, action := range []string{"cancel before prepare", "cancel after prepare", "discard", "change target"} {
		t.Run(action, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "app.conf")
			if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			request, _, err := fileedit.Build("app.conf", "old", "new")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if action == "cancel before prepare" {
				cancel()
			}
			edit, err := New(root).PrepareEdit(ctx, "app.conf", request)
			if action == "cancel before prepare" {
				if !errors.Is(err, context.Canceled) || edit != nil {
					t.Fatalf("cancelled preparation = %v, %v", edit, err)
				}
				assertEditContent(t, target, "old\n")
				assertNoStagedEdits(t, root)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer edit.Close()
			want := "old\n"
			switch action {
			case "cancel after prepare":
				cancel()
				if _, err := edit.Commit(ctx); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled commit = %v", err)
				}
			case "discard":
				if err := edit.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := edit.Commit(ctx); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("discarded commit = %v", err)
				}
			case "change target":
				want = "out\n" // Same length: conflict detection must compare bytes, not size.
				if err := os.WriteFile(target, []byte(want), 0o600); err != nil {
					t.Fatal(err)
				}
				_, err := edit.Commit(ctx)
				var conflict *EditConflictError
				if !errors.As(err, &conflict) || !strings.Contains(err.Error(), "changed during validation") {
					t.Fatalf("changed target commit = %v", err)
				}
			}
			if err := edit.Close(); err != nil {
				t.Fatal(err)
			}
			assertEditContent(t, target, want)
			assertNoStagedEdits(t, root)
		})
	}
}

func TestEditRejectsInvalidTargetBeforeStaging(t *testing.T) {
	root := t.TempDir()
	for _, file := range []struct{ name, content string }{{"app.conf", "old\n"}, {"duplicate", "old\nold\n"}, {"binary", "old\x00"}} {
		if err := os.WriteFile(filepath.Join(root, file.name), []byte(file.content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path, oldText, message string
		conflict               bool
	}{
		{"app.conf", "", "already exists", true},
		{"app.conf", "wrong", "matched 0", true},
		{"duplicate", "old", "matched 2", true},
		{"missing.conf", "old", "does not exist", false},
		{"directory", "old", "not a regular file", false},
		{"binary", "old", "binary", true},
		{"../outside", "old", "relative", false},
		{".env", "", "sensitive", false},
	} {
		t.Run(test.path+"/"+test.oldText, func(t *testing.T) {
			request, _, err := fileedit.Build(test.path, test.oldText, "new")
			if err != nil {
				t.Fatal(err)
			}
			edit, err := New(root).PrepareEdit(context.Background(), test.path, request)
			if edit != nil {
				defer edit.Close()
				t.Fatal("invalid target was staged")
			}
			var conflict *EditConflictError
			if err == nil || !strings.Contains(err.Error(), test.message) || errors.As(err, &conflict) != test.conflict {
				t.Fatalf("target rejection = %v", err)
			}
			assertNoStagedEdits(t, root)
		})
	}
}

func assertEditContent(t *testing.T, path, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil || string(content) != want {
		t.Fatalf("%s = %q, want %q; err=%v", path, content, want, err)
	}
}

func assertNoStagedEdits(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".opsnerva-") {
			t.Fatalf("staged file was not removed: %s", entry.Name())
		}
	}
}
