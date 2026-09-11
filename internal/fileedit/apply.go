package fileedit

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

type editableTextLine struct {
	text              string
	start, contentEnd int
	afterEnd          int
	eol               []byte
}

func editableTextLines(original []byte) ([]editableTextLine, error) {
	if bytes.HasPrefix(original, []byte{0xff, 0xfe}) || bytes.HasPrefix(original, []byte{0xfe, 0xff}) {
		return nil, fmt.Errorf("UTF-16 file editing is unsupported; convert the file to UTF-8 first")
	}
	if bytes.IndexByte(original, 0) >= 0 {
		return nil, fmt.Errorf("binary file editing is unsupported")
	}
	start := 0
	if bytes.HasPrefix(original, []byte{0xef, 0xbb, 0xbf}) {
		start = 3
	}
	lines := make([]editableTextLine, 0, bytes.Count(original[start:], []byte{'\n'})+1)
	for start < len(original) {
		newlineOffset := bytes.IndexByte(original[start:], '\n')
		if newlineOffset < 0 {
			lines = append(lines, editableTextLine{text: string(original[start:]), start: start, contentEnd: len(original), afterEnd: len(original)})
			break
		}
		newline := start + newlineOffset
		contentEnd := newline
		if contentEnd > start && original[contentEnd-1] == '\r' {
			contentEnd--
		}
		lines = append(lines, editableTextLine{
			text: string(original[start:contentEnd]), start: start, contentEnd: contentEnd, afterEnd: newline + 1,
			eol: append([]byte(nil), original[contentEnd:newline+1]...),
		})
		start = newline + 1
	}
	return lines, nil
}

// Apply replaces exactly one complete block in normalized input. Untouched bytes,
// UTF-8 BOM, line endings, and the final-newline state are preserved.
func Apply(original []byte, edit domain.TextEdit) ([]byte, error) {
	originalLines, err := editableTextLines(original)
	if err != nil {
		return nil, err
	}
	oldLines := strings.Split(edit.OldText, "\n")
	var newLines []string
	if edit.NewText != "" {
		newLines = strings.Split(edit.NewText, "\n")
	}
	matches := make([]int, 0, 2)
	for start := 0; start+len(oldLines) <= len(originalLines); start++ {
		matched := true
		for offset := range oldLines {
			if originalLines[start+offset].text != oldLines[offset] {
				matched = false
				break
			}
		}
		if matched {
			matches = append(matches, start)
		}
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("file edit conflict: old_text matched %d blocks; %s", len(matches), RetryAdvice)
	}
	start := matches[0]
	first := originalLines[start]
	last := originalLines[start+len(oldLines)-1]
	spliceStart, spliceEnd := first.start, last.contentEnd
	if len(newLines) == 0 {
		if last.afterEnd > last.contentEnd {
			spliceEnd = last.afterEnd
		} else if start > 0 {
			spliceStart = originalLines[start-1].contentEnd
		}
		updated := make([]byte, 0, len(original)-(spliceEnd-spliceStart))
		updated = append(updated, original[:spliceStart]...)
		updated = append(updated, original[spliceEnd:]...)
		return updated, nil
	}
	eol := []byte{'\n'}
	for index := start; index <= start+len(oldLines)-1; index++ {
		if len(originalLines[index].eol) > 0 {
			eol = originalLines[index].eol
			break
		}
	}
	if len(last.eol) == 0 && start > 0 && len(originalLines[start-1].eol) > 0 {
		eol = originalLines[start-1].eol
	}
	var replacement bytes.Buffer
	for index, line := range newLines {
		if index > 0 {
			replacement.Write(eol)
		}
		replacement.WriteString(line)
	}
	updated := make([]byte, 0, len(original)-(spliceEnd-spliceStart)+replacement.Len())
	updated = append(updated, original[:spliceStart]...)
	updated = append(updated, replacement.Bytes()...)
	updated = append(updated, original[spliceEnd:]...)
	return updated, nil
}
