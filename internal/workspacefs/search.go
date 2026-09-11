package workspacefs

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

func (fs *FS) Search(relative, pattern string, matchMode domain.FileSearchMatchMode, contextLines int) ([]byte, error) {
	if contextLines < 0 {
		return nil, fmt.Errorf("search context_lines must be non-negative")
	}
	path, err := fs.Resolve(relative, false)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var matches func(string) bool
	switch matchMode {
	case domain.FileSearchLiteral:
		matches = func(line string) bool { return strings.Contains(line, pattern) }
	case domain.FileSearchRegex:
		expression, compileErr := regexp.CompilePOSIX(pattern)
		if compileErr != nil {
			return nil, fmt.Errorf("invalid POSIX search regex: %w", compileErr)
		}
		matches = expression.MatchString
	default:
		return nil, fmt.Errorf("invalid search match_mode: use literal or regex")
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), int(^uint(0)>>1))
	var output strings.Builder
	type bufferedLine struct {
		number  int
		content string
		match   bool
	}
	before := make([]bufferedLine, 0, contextLines)
	line, lastOutputLine, afterRemaining := 0, 0, 0
	writeLine := func(item bufferedLine) {
		if item.number <= lastOutputLine {
			return
		}
		separator := "-"
		if item.match {
			separator = ":"
		}
		fmt.Fprintf(&output, "%d%s%s\n", item.number, separator, item.content)
		lastOutputLine = item.number
	}
	for scanner.Scan() {
		line++
		item := bufferedLine{number: line, content: scanner.Text()}
		item.match = matches(item.content)
		if item.match {
			for _, previous := range before {
				writeLine(previous)
			}
			writeLine(item)
			afterRemaining = contextLines
		} else if afterRemaining > 0 {
			writeLine(item)
			afterRemaining--
		}
		if contextLines > 0 {
			before = append(before, item)
			if len(before) > contextLines {
				before = before[len(before)-contextLines:]
			}
		}
	}
	return []byte(output.String()), scanner.Err()
}
