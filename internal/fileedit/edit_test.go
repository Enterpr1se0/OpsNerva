package fileedit

import (
	"strings"
	"testing"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func TestBuildTextEditNormalizesInputAndBuildsMinimalDiff(t *testing.T) {
	edit, change, err := Build("app.conf", "\ufeffa\r\nb\r\n", "a\r\nc\r\nd\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if edit.OldText != "a\nb" || edit.NewText != "a\nc\nd" || change.Additions != 2 || change.Deletions != 1 || !strings.Contains(change.Diff, "@@ unique block @@\n a\n-b\n+c\n+d\n") || strings.ContainsAny(change.Diff, "\ufeff\r") {
		t.Fatalf("unexpected normalized edit=%#v change=%#v", edit, change)
	}
	if err := ValidateChange("app.conf", edit, change); err != nil {
		t.Fatalf("generated edit failed consistency check: %v", err)
	}
	change.Diff = strings.Replace(change.Diff, "+c", "+other", 1)
	if err := ValidateChange("app.conf", edit, change); err == nil {
		t.Fatal("mismatched approval diff was accepted")
	}
}

func TestApplyTextEditRequiresOneExactBlock(t *testing.T) {
	if _, err := Apply([]byte("a\n"), domain.TextEdit{OldText: "wrong", NewText: "b"}); err == nil || !strings.Contains(err.Error(), "matched 0") || !strings.Contains(err.Error(), "preserving all leading whitespace") {
		t.Fatalf("missing old_text was accepted: %v", err)
	}
	if _, err := Apply([]byte("a\na\n"), domain.TextEdit{OldText: "a", NewText: "b"}); err == nil || !strings.Contains(err.Error(), "matched 2") {
		t.Fatalf("ambiguous old_text was accepted: %v", err)
	}
	updated, err := Apply([]byte("prefix\na\nsuffix\n"), domain.TextEdit{OldText: "a", NewText: "b"})
	if err != nil || string(updated) != "prefix\nb\nsuffix\n" {
		t.Fatalf("unique relocated edit failed: updated=%q err=%v", updated, err)
	}
	deleted, err := Apply([]byte("a\nb\n"), domain.TextEdit{OldText: "a", NewText: ""})
	if err != nil || string(deleted) != "b\n" {
		t.Fatalf("line deletion failed: updated=%q err=%v", deleted, err)
	}
	withoutFinalNewline, err := Apply([]byte("only-one-line-no-nl"), domain.TextEdit{OldText: "only-one-line-no-nl", NewText: "edited"})
	if err != nil || string(withoutFinalNewline) != "edited" {
		t.Fatalf("final-newline state changed: updated=%q err=%v", withoutFinalNewline, err)
	}
	crlf, err := Apply([]byte("a\r\nb\r\nc\r\n"), domain.TextEdit{OldText: "b", NewText: "b-edited"})
	if err != nil || string(crlf) != "a\r\nb-edited\r\nc\r\n" {
		t.Fatalf("CRLF bytes changed: updated=%q err=%v", crlf, err)
	}
	yaml := "tasks:\n    - name: install package\n      module: apt\n"
	if _, err := Apply([]byte(yaml), domain.TextEdit{OldText: "- name: install package\n      module: apt", NewText: "- name: update package\n      module: apt"}); err == nil || !strings.Contains(err.Error(), "matched 0") {
		t.Fatalf("unindented YAML block was accepted: %v", err)
	}
	updated, err = Apply([]byte(yaml), domain.TextEdit{OldText: "    - name: install package\n      module: apt", NewText: "    - name: update package\n      module: apt"})
	if err != nil || string(updated) != "tasks:\n    - name: update package\n      module: apt\n" {
		t.Fatalf("exactly indented YAML block failed: updated=%q err=%v", updated, err)
	}
}
