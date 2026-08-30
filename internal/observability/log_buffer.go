package observability

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type LogEntry struct {
	Time      time.Time      `json:"time"`
	Level     string         `json:"level"`
	Message   string         `json:"message"`
	Component string         `json:"component,omitempty"`
	Fields    map[string]any `json:"fields,omitempty"`
}

type LogFilter struct {
	Level     string
	Component string
	Query     string
	Limit     int
}

type LogSubscription struct {
	Entries     []LogEntry
	Events      <-chan LogEntry
	Overflow    <-chan struct{}
	unsubscribe func()
}

func (s LogSubscription) Unsubscribe() {
	if s.unsubscribe != nil {
		s.unsubscribe()
	}
}

type Buffer struct {
	mu               sync.RWMutex
	entries          []LogEntry
	limit            int
	start            int
	subscribers      map[uint64]*logSubscriber
	nextSubscriberID uint64
}

type logSubscriber struct {
	matcher    logMatcher
	events     chan LogEntry
	overflow   chan struct{}
	overflowed bool
}

type logMatcher struct {
	minimum   slog.Level
	component string
	query     string
}

type bufferHandler struct {
	buffer *Buffer
	level  slog.Leveler
	attrs  []slog.Attr
	groups []string
}

func (h *bufferHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *bufferHandler) Handle(_ context.Context, record slog.Record) error {
	fields := make(map[string]any, len(h.attrs)+record.NumAttrs())
	for _, attr := range h.attrs {
		appendAttr(fields, h.groups, attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		appendAttr(fields, h.groups, attr)
		return true
	})
	component, _ := fields["component"].(string)
	delete(fields, "component")
	delete(fields, "service")
	h.buffer.add(LogEntry{Time: record.Time.UTC(), Level: strings.ToLower(record.Level.String()), Message: logRedactor.Redact(record.Message), Component: component, Fields: fields})
	return nil
}

func (h *bufferHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &clone
}

func (h *bufferHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.groups = append(append([]string{}, h.groups...), name)
	return &clone
}

func appendAttr(fields map[string]any, groups []string, attr slog.Attr) {
	if attr.Equal(slog.Attr{}) {
		return
	}
	value := attr.Value.Resolve()
	if value.Kind() == slog.KindGroup {
		next := groups
		if attr.Key != "" {
			next = append(append([]string{}, groups...), attr.Key)
		}
		for _, child := range value.Group() {
			appendAttr(fields, next, child)
		}
		return
	}
	key := strings.Join(append(append([]string{}, groups...), attr.Key), ".")
	if sensitiveKey(key) {
		fields[key] = "[REDACTED]"
		return
	}
	fields[key] = logValue(value)
}

func logValue(value slog.Value) any {
	switch value.Kind() {
	case slog.KindString:
		return logRedactor.Redact(value.String())
	case slog.KindBool:
		return value.Bool()
	case slog.KindDuration:
		return value.Duration().String()
	case slog.KindFloat64:
		return value.Float64()
	case slog.KindInt64:
		return value.Int64()
	case slog.KindTime:
		return value.Time().UTC()
	case slog.KindUint64:
		return value.Uint64()
	case slog.KindAny:
		return sanitizeAny(value.Any())
	default:
		return value.String()
	}
}

func (b *Buffer) add(entry LogEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 {
		b.limit = 2000
	}
	if len(b.entries) == b.limit {
		b.entries[b.start] = entry
		b.start = (b.start + 1) % len(b.entries)
	} else {
		b.entries = append(b.entries, entry)
	}
	for _, subscriber := range b.subscribers {
		if subscriber.overflowed || !subscriber.matcher.matches(entry) {
			continue
		}
		select {
		case subscriber.events <- entry:
		default:
			subscriber.overflowed = true
			subscriber.overflow <- struct{}{}
		}
	}
}

func (b *Buffer) recent(filter LogFilter) []LogEntry {
	matcher := newLogMatcher(filter)
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.recentLocked(matcher, normalizedLogLimit(filter.Limit))
}

func (b *Buffer) subscribe(filter LogFilter) LogSubscription {
	matcher := newLogMatcher(filter)
	events := make(chan LogEntry, 128)
	overflow := make(chan struct{}, 1)
	b.mu.Lock()
	entries := b.recentLocked(matcher, normalizedLogLimit(filter.Limit))
	if b.subscribers == nil {
		b.subscribers = make(map[uint64]*logSubscriber)
	}
	b.nextSubscriberID++
	id := b.nextSubscriberID
	b.subscribers[id] = &logSubscriber{matcher: matcher, events: events, overflow: overflow}
	b.mu.Unlock()

	var once sync.Once
	return LogSubscription{
		Entries:  entries,
		Events:   events,
		Overflow: overflow,
		unsubscribe: func() {
			once.Do(func() {
				b.mu.Lock()
				delete(b.subscribers, id)
				b.mu.Unlock()
			})
		},
	}
}

func normalizedLogLimit(limit int) int {
	if limit <= 0 || limit > 1000 {
		return 300
	}
	return limit
}

func newLogMatcher(filter LogFilter) logMatcher {
	minimum, err := parseLevel(filter.Level)
	if err != nil {
		minimum = slog.LevelDebug
	}
	return logMatcher{
		minimum:   minimum,
		component: strings.ToLower(strings.TrimSpace(filter.Component)),
		query:     strings.ToLower(strings.TrimSpace(filter.Query)),
	}
}

func (m logMatcher) matches(entry LogEntry) bool {
	entryLevel, _ := parseLevel(entry.Level)
	if entryLevel < m.minimum || (m.component != "" && strings.ToLower(entry.Component) != m.component) {
		return false
	}
	if m.query == "" {
		return true
	}
	encoded, _ := json.Marshal(entry.Fields)
	haystack := strings.ToLower(entry.Message + " " + entry.Component + " " + string(encoded))
	return strings.Contains(haystack, m.query)
}

func (b *Buffer) recentLocked(matcher logMatcher, limit int) []LogEntry {
	result := make([]LogEntry, 0, min(limit, len(b.entries)))
	for logical := len(b.entries) - 1; logical >= 0 && len(result) < limit; logical-- {
		index := (b.start + logical) % len(b.entries)
		entry := b.entries[index]
		if !matcher.matches(entry) {
			continue
		}
		result = append(result, entry)
	}
	return result
}
