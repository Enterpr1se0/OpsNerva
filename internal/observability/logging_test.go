package observability

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBufferFiltersAndRedactsSensitiveFields(t *testing.T) {
	buffer := &Buffer{limit: 10}
	level := slog.LevelDebug
	logger := slog.New(&bufferHandler{buffer: buffer, level: level}).With("component", "ssh")
	logger.InfoContext(context.Background(), "execution completed",
		"run_id", "run_test", "password", "do-not-expose", "stdout_bytes", 128, "token_count", 42)

	entries := buffer.recent(LogFilter{Level: "info", Component: "ssh", Query: "run_test", Limit: 5})
	if len(entries) != 1 {
		t.Fatalf("expected one matching entry, got %#v", entries)
	}
	if entries[0].Fields["password"] != "[REDACTED]" {
		t.Fatalf("sensitive field was not redacted: %#v", entries[0].Fields)
	}
	if entries[0].Fields["stdout_bytes"] != int64(128) {
		t.Fatalf("safe output metric was unexpectedly hidden: %#v", entries[0].Fields)
	}
	if entries[0].Fields["token_count"] != int64(42) {
		t.Fatalf("safe token metric was unexpectedly hidden: %#v", entries[0].Fields)
	}
	if got := buffer.recent(LogFilter{Level: "error", Limit: 5}); len(got) != 0 {
		t.Fatalf("minimum level filter returned info entry: %#v", got)
	}
}

func TestBufferUsesRingOrderAfterCapacity(t *testing.T) {
	buffer := &Buffer{limit: 3}
	for _, message := range []string{"one", "two", "three", "four", "five"} {
		buffer.add(LogEntry{Level: "info", Message: message})
	}
	entries := buffer.recent(LogFilter{Limit: 10})
	if len(entries) != 3 || entries[0].Message != "five" || entries[1].Message != "four" || entries[2].Message != "three" {
		t.Fatalf("ring entries = %#v", entries)
	}
}

func TestLogHandlersRedactMessagesErrorsAndNestedValues(t *testing.T) {
	buffer := &Buffer{limit: 10}
	level := slog.LevelDebug
	logger := slog.New(&bufferHandler{buffer: buffer, level: level})
	logger.ErrorContext(context.Background(), "request Bearer message-secret failed",
		"error", errors.New("password=error-secret"),
		"details", map[string]any{"api_key": "nested-secret", "url": "https://user:url-secret@proxy.example"},
		"password_bytes", 32)

	entries := buffer.recent(LogFilter{Level: "debug", Limit: 10})
	if len(entries) != 1 {
		t.Fatalf("entries = %#v", entries)
	}
	encoded := entries[0].Message
	for key, value := range entries[0].Fields {
		encoded += key + logValueString(value)
	}
	for _, secret := range []string{"message-secret", "error-secret", "nested-secret", "url-secret"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("buffer leaked %q: %#v", secret, entries[0])
		}
	}
	if entries[0].Fields["password_bytes"] != "[REDACTED]" {
		t.Fatalf("credential metric was not redacted: %#v", entries[0].Fields)
	}

	var output bytes.Buffer
	jsonLogger := slog.New(&redactingHandler{next: slog.NewJSONHandler(&output, &slog.HandlerOptions{ReplaceAttr: replaceSensitiveAttr})})
	jsonLogger.Error("Authorization: Basic output-secret", "error", errors.New("--token cli-secret"))
	if strings.Contains(output.String(), "output-secret") || strings.Contains(output.String(), "cli-secret") {
		t.Fatalf("structured output leaked a secret: %s", output.String())
	}
}

func logValueString(value any) string {
	return strings.TrimSpace(string(mustJSON(value)))
}

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

func TestRotatingWriterKeepsBoundedBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opsnerva.log")
	writer, err := newRotatingWriter(path, 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.file.Close()
	if _, err := writer.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "second\n" || string(backup) != "first\n" {
		t.Fatalf("unexpected rotation current=%q backup=%q", current, backup)
	}
}

func TestWriteArchiveIncludesCurrentLogAndRotatedBackups(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opsnerva.log")
	files := map[string]string{
		"opsnerva.log":   "current\n",
		"opsnerva.log.1": "backup-one\n",
		"opsnerva.log.2": "backup-two\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	previousFile := activeFile.Load()
	activeFile.Store(&path)
	t.Cleanup(func() {
		activeFile.Store(previousFile)
	})

	var output bytes.Buffer
	if err := WriteArchive(&output, Diagnostics{SchemaVersion: 1, Agent: AgentDiagnostics{ProviderName: "password=diagnostic-secret"}}); err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(reader.File) != len(files)+1 || reader.File[0].Name != "diagnostics.json" {
		t.Fatalf("archive entries = %#v", reader.File)
	}
	manifest, err := reader.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	manifestContent, err := io.ReadAll(manifest)
	manifest.Close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifestContent), "diagnostic-secret") {
		t.Fatalf("diagnostic manifest leaked a secret: %s", manifestContent)
	}
	for _, archived := range reader.File[1:] {
		entry, err := archived.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(entry)
		entry.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != files[archived.Name] {
			t.Fatalf("archive %q = %q, want %q", archived.Name, content, files[archived.Name])
		}
	}
}

func TestWriteArchiveFallsBackToInMemoryJSONL(t *testing.T) {
	disabled := "-"
	buffer := &Buffer{limit: 10, entries: []LogEntry{{Level: "debug", Message: "memory event", Component: "test"}}}
	previousFile := activeFile.Load()
	previousBuffer := activeBuffer.Load()
	activeFile.Store(&disabled)
	activeBuffer.Store(buffer)
	t.Cleanup(func() {
		activeFile.Store(previousFile)
		activeBuffer.Store(previousBuffer)
	})

	var output bytes.Buffer
	if err := WriteArchive(&output, Diagnostics{SchemaVersion: 1}); err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(reader.File) != 2 || reader.File[0].Name != "diagnostics.json" || reader.File[1].Name != "opsnerva-memory.jsonl" {
		t.Fatalf("unexpected archive entries: %#v", reader.File)
	}
	entry, err := reader.File[1].Open()
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(entry)
	entry.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `"message":"memory event"`) {
		t.Fatalf("in-memory log was not exported: %s", content)
	}
}

func TestWriteArchiveRedactsExistingLogFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opsnerva.log")
	content := `{"level":"ERROR","msg":"password=legacy-secret","password":"field-secret","details":{"api_key":"nested-secret"}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	previousFile := activeFile.Load()
	activeFile.Store(&path)
	t.Cleanup(func() { activeFile.Store(previousFile) })

	var output bytes.Buffer
	if err := WriteArchive(&output, Diagnostics{SchemaVersion: 1}); err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	entry, err := reader.File[1].Open()
	if err != nil {
		t.Fatal(err)
	}
	exported, err := io.ReadAll(entry)
	entry.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"legacy-secret", "field-secret", "nested-secret"} {
		if strings.Contains(string(exported), secret) {
			t.Fatalf("archive leaked %q: %s", secret, exported)
		}
	}
	var parsed map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(exported), &parsed); err != nil {
		t.Fatalf("exported JSONL is invalid: %v: %s", err, exported)
	}
}

func TestEmptyLogLevelDefaultsToDebug(t *testing.T) {
	level, err := parseLevel("")
	if err != nil {
		t.Fatal(err)
	}
	if level != slog.LevelDebug {
		t.Fatalf("empty level = %s, want debug", level)
	}
}
