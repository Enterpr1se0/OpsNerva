package observability

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/security"
)

type Diagnostics struct {
	SchemaVersion    int                    `json:"schema_version"`
	GeneratedAt      time.Time              `json:"generated_at"`
	Application      ApplicationDiagnostics `json:"application"`
	Logging          LoggingDiagnostics     `json:"logging"`
	Agent            AgentDiagnostics       `json:"agent"`
	Resources        ResourceDiagnostics    `json:"resources"`
	CollectionErrors []string               `json:"collection_errors,omitempty"`
}

type ApplicationDiagnostics struct {
	Version       string    `json:"version"`
	GoVersion     string    `json:"go_version"`
	OS            string    `json:"os"`
	Architecture  string    `json:"architecture"`
	StartedAt     time.Time `json:"started_at"`
	UptimeSeconds int64     `json:"uptime_seconds"`
}

type LoggingDiagnostics struct {
	Level       string `json:"level"`
	Format      string `json:"format"`
	FileEnabled bool   `json:"file_enabled"`
	AddSource   bool   `json:"add_source"`
	MaxSizeMB   int    `json:"max_size_mb"`
	MaxBackups  int    `json:"max_backups"`
	RecentLimit int    `json:"recent_limit"`
}

type AgentDiagnostics struct {
	Available                       bool   `json:"available"`
	Source                          string `json:"source"`
	ProviderName                    string `json:"provider_name,omitempty"`
	Model                           string `json:"model,omitempty"`
	ToolCount                       int    `json:"tool_count"`
	ApprovalAgentAvailable          bool   `json:"approval_agent_available"`
	AutomaticApprovalAgentAvailable bool   `json:"automatic_approval_agent_available"`
	ApprovalMode                    string `json:"approval_mode"`
	ModelError                      string `json:"model_error,omitempty"`
	ApprovalError                   string `json:"approval_error,omitempty"`
	AutomaticApprovalError          string `json:"automatic_approval_error,omitempty"`
}

type ResourceDiagnostics struct {
	Hosts              int            `json:"hosts"`
	Proxies            int            `json:"proxies"`
	ModelProviders     int            `json:"model_providers"`
	ActiveProviders    int            `json:"active_providers"`
	MCPServers         int            `json:"mcp_servers"`
	MCPStatuses        map[string]int `json:"mcp_statuses"`
	Workspaces         int            `json:"workspaces"`
	WritableWorkspaces int            `json:"writable_workspaces"`
	Skills             int            `json:"skills"`
	EnabledSkills      int            `json:"enabled_skills"`
}

var activeBuffer atomic.Pointer[Buffer]
var activeLevel atomic.Int64
var activeFile atomic.Pointer[string]
var logRedactor = security.NewRedactor()

type loggerContextKey struct{}

func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerContextKey{}, logger)
}

func FromContext(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if logger, ok := ctx.Value(loggerContextKey{}).(*slog.Logger); ok && logger != nil {
			return logger
		}
	}
	return slog.Default()
}

func Configure(cfg config.Logging) error {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return err
	}
	format := strings.ToLower(strings.TrimSpace(cfg.Format))
	if format == "" {
		format = "text"
	}
	if format != "text" && format != "json" {
		return fmt.Errorf("invalid log format %q: use text or json", cfg.Format)
	}
	if cfg.RecentLimit <= 0 {
		cfg.RecentLimit = 2000
	}
	if cfg.MaxSizeMB <= 0 {
		cfg.MaxSizeMB = 20
	}
	if cfg.MaxBackups < 0 {
		cfg.MaxBackups = 0
	}

	levelVar := new(slog.LevelVar)
	levelVar.Set(level)
	activeLevel.Store(int64(level))
	logFile := cfg.File
	activeFile.Store(&logFile)
	options := &slog.HandlerOptions{Level: levelVar, AddSource: cfg.AddSource, ReplaceAttr: replaceSensitiveAttr}
	handlers := []slog.Handler{newHandler(os.Stderr, format, options)}
	if cfg.File != "" && cfg.File != "-" {
		file, err := newRotatingWriter(cfg.File, int64(cfg.MaxSizeMB)<<20, cfg.MaxBackups)
		if err != nil {
			return fmt.Errorf("open log file: %w", err)
		}
		handlers = append(handlers, slog.NewJSONHandler(file, options))
	}
	buffer := &Buffer{limit: cfg.RecentLimit}
	activeBuffer.Store(buffer)
	handlers = append(handlers, &bufferHandler{buffer: buffer, level: levelVar})
	logger := slog.New(&redactingHandler{next: slog.NewMultiHandler(handlers...)}).With("service", "opsnerva")
	slog.SetDefault(logger)
	slog.SetLogLoggerLevel(level)
	return nil
}

func newHandler(writer io.Writer, format string, options *slog.HandlerOptions) slog.Handler {
	if format == "json" {
		return slog.NewJSONHandler(writer, options)
	}
	return slog.NewTextHandler(writer, options)
}

func Recent(filter LogFilter) []LogEntry {
	buffer := activeBuffer.Load()
	if buffer == nil {
		return []LogEntry{}
	}
	return buffer.recent(filter)
}

func Subscribe(filter LogFilter) LogSubscription {
	buffer := activeBuffer.Load()
	if buffer == nil {
		return LogSubscription{Entries: []LogEntry{}}
	}
	return buffer.subscribe(filter)
}

func Components() []string {
	buffer := activeBuffer.Load()
	if buffer == nil {
		return []string{}
	}
	buffer.mu.RLock()
	defer buffer.mu.RUnlock()
	seen := make(map[string]struct{})
	for _, entry := range buffer.entries {
		if entry.Component != "" {
			seen[entry.Component] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for component := range seen {
		result = append(result, component)
	}
	sort.Strings(result)
	return result
}

func MinimumLevel() string {
	return strings.ToLower(slog.Level(activeLevel.Load()).String())
}

func File() string {
	value := activeFile.Load()
	if value == nil || *value == "-" {
		return ""
	}
	return *value
}

// WriteArchive streams a diagnostic manifest plus the complete active log file
// and its rotated backups. When file logging is disabled or no log file exists,
// it exports the current process's in-memory structured log entries instead.
func WriteArchive(writer io.Writer, diagnostics Diagnostics) error {
	archive := zip.NewWriter(writer)
	manifest, err := archive.Create("diagnostics.json")
	if err != nil {
		_ = archive.Close()
		return fmt.Errorf("create diagnostic manifest: %w", err)
	}
	encoder := json.NewEncoder(manifest)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(sanitizeAny(diagnostics)); err != nil {
		_ = archive.Close()
		return fmt.Errorf("write diagnostic manifest: %w", err)
	}
	written := 0
	if logFile := File(); logFile != "" {
		logFiles, err := existingLogFiles(logFile)
		if err != nil {
			_ = archive.Close()
			return err
		}
		for _, path := range logFiles {
			added, err := addLogFileToArchive(archive, path)
			if err != nil {
				_ = archive.Close()
				return err
			}
			if added {
				written++
			}
		}
	}
	if written == 0 {
		entry, err := archive.Create("opsnerva-memory.jsonl")
		if err != nil {
			_ = archive.Close()
			return fmt.Errorf("create in-memory log archive entry: %w", err)
		}
		encoder := json.NewEncoder(entry)
		for _, logEntry := range snapshotEntries() {
			if err := encoder.Encode(sanitizeAny(logEntry)); err != nil {
				_ = archive.Close()
				return fmt.Errorf("write in-memory log archive entry: %w", err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		return fmt.Errorf("finalize log archive: %w", err)
	}
	return nil
}

func existingLogFiles(logFile string) ([]string, error) {
	directory := filepath.Dir(logFile)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return []string{logFile}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list log directory %q: %w", directory, err)
	}
	type backup struct {
		path  string
		index int
	}
	prefix := filepath.Base(logFile) + "."
	backups := make([]backup, 0)
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		index, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), prefix))
		if err != nil || index < 1 {
			continue
		}
		backups = append(backups, backup{path: filepath.Join(directory, entry.Name()), index: index})
	}
	sort.Slice(backups, func(left, right int) bool { return backups[left].index > backups[right].index })
	paths := make([]string, 0, len(backups)+1)
	for _, item := range backups {
		paths = append(paths, item.path)
	}
	return append(paths, logFile), nil
}

func addLogFileToArchive(archive *zip.Writer, logFile string) (bool, error) {
	file, err := os.Open(logFile)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open log file %q: %w", logFile, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("stat log file %q: %w", logFile, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("log file %q is not a regular file", logFile)
	}
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return false, fmt.Errorf("prepare log file %q: %w", logFile, err)
	}
	header.Name = filepath.Base(logFile)
	header.Method = zip.Deflate
	entry, err := archive.CreateHeader(header)
	if err != nil {
		return false, fmt.Errorf("create archive entry for %q: %w", logFile, err)
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 64<<20)
	encoder := json.NewEncoder(entry)
	for scanner.Scan() {
		line := scanner.Bytes()
		var value any
		if err := json.Unmarshal(line, &value); err == nil {
			if err := encoder.Encode(sanitizeAny(value)); err != nil {
				return false, fmt.Errorf("archive structured log file %q: %w", logFile, err)
			}
			continue
		}
		if _, err := fmt.Fprintln(entry, logRedactor.Redact(string(line))); err != nil {
			return false, fmt.Errorf("archive legacy log file %q: %w", logFile, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("read log file %q: %w", logFile, err)
	}
	return true, nil
}

func snapshotEntries() []LogEntry {
	buffer := activeBuffer.Load()
	if buffer == nil {
		return []LogEntry{}
	}
	buffer.mu.RLock()
	defer buffer.mu.RUnlock()
	return append([]LogEntry(nil), buffer.entries...)
}

func parseLevel(value string) (slog.Level, error) {
	var level slog.Level
	if strings.TrimSpace(value) == "" {
		return slog.LevelDebug, nil
	}
	if err := level.UnmarshalText([]byte(value)); err != nil {
		return 0, fmt.Errorf("invalid log level %q: use debug, info, warn, or error", value)
	}
	return level, nil
}

type redactingHandler struct {
	next slog.Handler
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	record = record.Clone()
	record.Message = logRedactor.Redact(record.Message)
	return h.next.Handle(ctx, record)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &redactingHandler{next: h.next.WithAttrs(attrs)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{next: h.next.WithGroup(name)}
}

func replaceSensitiveAttr(groups []string, attr slog.Attr) slog.Attr {
	key := strings.Join(append(append([]string{}, groups...), attr.Key), ".")
	if sensitiveKey(key) {
		return slog.String(attr.Key, "[REDACTED]")
	}
	attr.Value = sanitizeSlogValue(attr.Value)
	return attr
}

func sensitiveKey(key string) bool {
	key = strings.ToLower(key)
	for _, suffix := range []string{"_bytes", "_count", "_segments"} {
		if strings.HasSuffix(key, suffix) && !strings.Contains(key, "password") && !strings.Contains(key, "secret") && !strings.Contains(key, "authorization") && !strings.Contains(key, "api_key") && !strings.Contains(key, "apikey") && !strings.Contains(key, "private_key") {
			return false
		}
	}
	for _, fragment := range []string{"password", "secret", "token", "authorization", "api_key", "apikey", "private_key"} {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	for _, fragment := range []string{"request_body", "response_body", "stdout", "stderr", "reasoning", "content"} {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	return false
}

func sanitizeSlogValue(value slog.Value) slog.Value {
	value = value.Resolve()
	switch value.Kind() {
	case slog.KindString:
		return slog.StringValue(logRedactor.Redact(value.String()))
	case slog.KindAny:
		return slog.AnyValue(sanitizeAny(value.Any()))
	default:
		return value
	}
}

func sanitizeAny(value any) any {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		return logRedactor.Redact(typed)
	case error:
		return logRedactor.Redact(typed.Error())
	case bool, float32, float64, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return typed
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			if sensitiveKey(key) {
				result[key] = "[REDACTED]"
			} else {
				result[key] = sanitizeAny(item)
			}
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = sanitizeAny(item)
		}
		return result
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return logRedactor.Redact(fmt.Sprint(value))
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return logRedactor.Redact(fmt.Sprint(value))
	}
	switch typed := decoded.(type) {
	case map[string]any, []any:
		return sanitizeAny(typed)
	case string:
		return logRedactor.Redact(typed)
	default:
		return typed
	}
}

type rotatingWriter struct {
	mu         sync.Mutex
	path       string
	maxBytes   int64
	maxBackups int
	file       *os.File
	size       int64
}

func newRotatingWriter(path string, maxBytes int64, maxBackups int) (*rotatingWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	writer := &rotatingWriter{path: path, maxBytes: maxBytes, maxBackups: maxBackups}
	if err := writer.open(); err != nil {
		return nil, err
	}
	return writer, nil
}

func (w *rotatingWriter) open() error {
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	w.file = file
	w.size = info.Size()
	return nil
}

func (w *rotatingWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.maxBytes > 0 && w.size > 0 && w.size+int64(len(data)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	written, err := w.file.Write(data)
	w.size += int64(written)
	return written, err
}

func (w *rotatingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	if w.maxBackups == 0 {
		_ = os.Remove(w.path)
	} else {
		_ = os.Remove(fmt.Sprintf("%s.%d", w.path, w.maxBackups))
		for index := w.maxBackups - 1; index >= 1; index-- {
			_ = os.Rename(fmt.Sprintf("%s.%d", w.path, index), fmt.Sprintf("%s.%d", w.path, index+1))
		}
		if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return w.open()
}
