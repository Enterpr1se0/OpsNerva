package agenttool

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

const maxOutputViewBytes = 4 << 20

func ValidateOutputView(maxBytes int, view string) (string, error) {
	view = strings.ToLower(strings.TrimSpace(view))
	if maxBytes < 0 || maxBytes > maxOutputViewBytes {
		return "", fmt.Errorf("max_output_bytes must be between 0 and %d", maxOutputViewBytes)
	}
	if maxBytes == 0 {
		if view != "" {
			return "", fmt.Errorf("output_view requires max_output_bytes")
		}
		return "", nil
	}
	if view == "" {
		return "head_tail", nil
	}
	switch view {
	case "head", "tail", "head_tail":
		return view, nil
	default:
		return "", fmt.Errorf("output_view must be head, tail, or head_tail")
	}
}

func UTF8Prefix(value string, limit int) string {
	if limit >= len(value) {
		return value
	}
	end := limit
	for end > 0 && end < len(value) && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func utf8Suffix(value string, limit int) string {
	if limit >= len(value) {
		return value
	}
	start := len(value) - limit
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return value[start:]
}

func LimitText(value string, maxBytes int, view string) string {
	if maxBytes == 0 || len(value) <= maxBytes {
		return value
	}
	switch view {
	case "head":
		return UTF8Prefix(value, maxBytes)
	case "tail":
		return utf8Suffix(value, maxBytes)
	default:
		headBytes := maxBytes / 2
		return UTF8Prefix(value, headBytes) + utf8Suffix(value, maxBytes-headBytes)
	}
}

func SelectExecOutput(result domain.ExecResult, stdoutOffset, stderrOffset, maxBytes int, view string, reportTotals bool) (domain.ExecResult, error) {
	view, err := ValidateOutputView(maxBytes, view)
	if err != nil {
		return result, err
	}
	stdoutTotal, stderrTotal := len(result.Stdout), len(result.Stderr)
	if stdoutOffset < 0 || stdoutOffset > stdoutTotal || stderrOffset < 0 || stderrOffset > stderrTotal {
		return result, fmt.Errorf("output byte offsets must be non-negative and no greater than the current stream totals")
	}
	stdout, stderr := result.Stdout[stdoutOffset:], result.Stderr[stderrOffset:]
	limitedStdout := LimitText(stdout, maxBytes, view)
	limitedStderr := LimitText(stderr, maxBytes, view)
	result.Stdout, result.Stderr = limitedStdout, limitedStderr
	result.StdoutOffsetBytes, result.StderrOffsetBytes = stdoutOffset, stderrOffset
	result.StdoutOmittedBytes = len(stdout) - len(limitedStdout)
	result.StderrOmittedBytes = len(stderr) - len(limitedStderr)
	result.OutputLimited = result.StdoutOmittedBytes > 0 || result.StderrOmittedBytes > 0
	if reportTotals || maxBytes > 0 || stdoutOffset > 0 || stderrOffset > 0 {
		result.StdoutTotalBytes, result.StderrTotalBytes = stdoutTotal, stderrTotal
	}
	if maxBytes > 0 {
		result.OutputView = view
	}
	return result, nil
}
