// Package fileedit implements the text-edit contract shared by remote and Workspace files.
package fileedit

import (
	"fmt"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

const RetryAdvice = "copy one exact unique block from the latest read, preserving all leading whitespace"

// Build normalizes model input and generates the display diff bound to approval.
func Build(path, oldText, newText string) (domain.TextEdit, domain.FileChange, error) {
	oldText, err := normalizeBlock(oldText)
	if err != nil {
		return domain.TextEdit{}, domain.FileChange{}, fmt.Errorf("invalid old_text: %w", err)
	}
	newText, err = normalizeBlock(newText)
	if err != nil {
		return domain.TextEdit{}, domain.FileChange{}, fmt.Errorf("invalid new_text: %w", err)
	}
	if oldText == newText {
		return domain.TextEdit{}, domain.FileChange{}, fmt.Errorf("old_text and new_text must be different")
	}

	var oldLines []string
	if oldText != "" {
		oldLines = strings.Split(oldText, "\n")
	}
	var newLines []string
	if newText != "" {
		newLines = strings.Split(newText, "\n")
	}
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix && oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}
	oldChanged := oldLines[prefix : len(oldLines)-suffix]
	newChanged := newLines[prefix : len(newLines)-suffix]
	body := make([]string, 0, len(oldLines)+len(newChanged))
	for _, line := range oldLines[:prefix] {
		body = append(body, " "+line)
	}
	for _, line := range oldChanged {
		body = append(body, "-"+line)
	}
	for _, line := range newChanged {
		body = append(body, "+"+line)
	}
	if suffix > 0 {
		for _, line := range oldLines[len(oldLines)-suffix:] {
			body = append(body, " "+line)
		}
	}
	diff := "--- " + path + "\n+++ " + path + "\n@@ unique block @@\n" + strings.Join(body, "\n") + "\n"
	return domain.TextEdit{OldText: oldText, NewText: newText}, domain.FileChange{
		Diff: diff, Additions: len(newChanged), Deletions: len(oldChanged),
	}, nil
}

// ValidateChange verifies a persisted edit still matches its approved display diff.
func ValidateChange(path string, edit domain.TextEdit, change domain.FileChange) error {
	normalizedEdit, expectedChange, err := Build(path, edit.OldText, edit.NewText)
	if err != nil {
		return fmt.Errorf("invalid persisted text edit: %w", err)
	}
	if normalizedEdit != edit || expectedChange != change {
		return fmt.Errorf("file edit approval data does not match the generated change")
	}
	return nil
}

func normalizeBlock(value string) (string, error) {
	value = strings.TrimPrefix(value, "\ufeff")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	if strings.ContainsAny(value, "\x00\r") {
		return "", fmt.Errorf("contains unsupported control characters")
	}
	value = strings.TrimSuffix(value, "\n")
	return value, nil
}
