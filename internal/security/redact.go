package security

import (
	"regexp"
	"strings"
	"sync"
)

type Redactor struct {
	rules []redactionRule
}

type redactionRule struct {
	pattern     *regexp.Regexp
	replacement string
}

type StreamRedactor struct {
	mu              sync.Mutex
	redactor        *Redactor
	pending         string
	privateKeyBlock bool
}

var (
	privateKeyBegin = regexp.MustCompile(`-----BEGIN (?:OPENSSH|RSA|EC|DSA)? ?PRIVATE KEY-----`)
	privateKeyEnd   = regexp.MustCompile(`-----END (?:OPENSSH|RSA|EC|DSA)? ?PRIVATE KEY-----`)
)

func NewRedactor() *Redactor {
	rules := []redactionRule{
		{regexp.MustCompile(`(?i)(\bauthorization\s*:\s*(?:bearer|basic)\s+)[^\s,;]+`), `${1}[REDACTED]`},
		{regexp.MustCompile(`(?i)(\b(?:bearer|basic)\s+)[A-Za-z0-9._~+/=-]{4,}`), `${1}[REDACTED]`},
		{regexp.MustCompile(`(?i)((?:["']?\b(?:password|passwd|sudo_password|proxy_password|api[_-]?key|access[_-]?token|secret|client_secret)["']?)\s*[=:]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;&]+)`), `${1}[REDACTED]`},
		{regexp.MustCompile(`(?i)(--(?:password|passwd|sudo-password|proxy-password|api[-_]?key|access[-_]?token|token|secret)(?:=|\s+))(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;&]+)`), `${1}[REDACTED]`},
		{regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://[^/\s:@]+:)[^@\s/]+@`), `${1}[REDACTED]@`},
		{regexp.MustCompile(`AKIA[0-9A-Z]{16}`), `[REDACTED]`},
		{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`), `[REDACTED]`},
		{regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`), `[REDACTED]`},
		{regexp.MustCompile(`-----BEGIN (?:OPENSSH|RSA|EC|DSA)? ?PRIVATE KEY-----[\s\S]*?-----END (?:OPENSSH|RSA|EC|DSA)? ?PRIVATE KEY-----`), `[REDACTED]`},
	}
	return &Redactor{rules: rules}
}

func (r *Redactor) Redact(input string) string {
	result := input
	for _, rule := range r.rules {
		result = rule.pattern.ReplaceAllString(result, rule.replacement)
	}
	return result
}

// NewStreamRedactor buffers incomplete lines so secrets split across transport
// chunks are never emitted before the complete value can be inspected.
func (r *Redactor) NewStreamRedactor() *StreamRedactor {
	return &StreamRedactor{redactor: r}
}

func (r *StreamRedactor) Write(data []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(data) == 0 {
		return ""
	}
	r.pending += string(data)
	var output strings.Builder
	for {
		index := strings.IndexAny(r.pending, "\r\n")
		if index < 0 {
			break
		}
		end := index + 1
		if r.pending[index] == '\r' && end < len(r.pending) && r.pending[end] == '\n' {
			end++
		}
		line := r.pending[:end]
		r.pending = r.pending[end:]
		output.WriteString(r.redactLine(line))
	}
	return output.String()
}

func (r *StreamRedactor) Flush() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == "" {
		return ""
	}
	line := r.pending
	r.pending = ""
	return r.redactLine(line)
}

func (r *StreamRedactor) redactLine(line string) string {
	if r.privateKeyBlock {
		if privateKeyEnd.MatchString(line) {
			r.privateKeyBlock = false
		}
		return ""
	}
	if privateKeyBegin.MatchString(line) {
		if !privateKeyEnd.MatchString(line) {
			r.privateKeyBlock = true
		}
		switch {
		case strings.HasSuffix(line, "\r\n"):
			return "[REDACTED]\r\n"
		case strings.HasSuffix(line, "\n"):
			return "[REDACTED]\n"
		case strings.HasSuffix(line, "\r"):
			return "[REDACTED]\r"
		}
		return "[REDACTED]"
	}
	return r.redactor.Redact(line)
}
