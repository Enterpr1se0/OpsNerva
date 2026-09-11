package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrInUse         = errors.New("in use")
)

type Store struct {
	db                *sql.DB
	shellEventEncoder *zstd.Encoder
	shellEventDecoder *zstd.Decoder
	changeMu          sync.RWMutex
	changeListeners   map[uint64]func(Change)
	changeListenerID  uint64
}

func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// DSN pragmas are applied to every pooled connection. WAL then permits
	// readers to proceed while the single SQLite writer is committing.
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	dsn := path + separator + "_pragma=foreign_keys%3Don&_pragma=busy_timeout%3D5000&_pragma=journal_mode%3DWAL&_pragma=synchronous%3DNORMAL"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	poolSize := 4
	if path == ":memory:" {
		// Each :memory: connection owns a different database.
		poolSize = 1
	}
	db.SetMaxOpenConns(poolSize)
	db.SetMaxIdleConns(poolSize)
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON; PRAGMA journal_mode = WAL; PRAGMA synchronous = NORMAL; PRAGMA busy_timeout = 5000;"); err != nil {
		db.Close()
		return nil, err
	}
	shellEventEncoder, err := zstd.NewWriter(nil,
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderLevel(zstd.SpeedFastest),
	)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize shell event compressor: %w", err)
	}
	shellEventDecoder, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(maxSSHShellEventDecodedBytes),
	)
	if err != nil {
		shellEventEncoder.Close()
		db.Close()
		return nil, fmt.Errorf("initialize shell event decompressor: %w", err)
	}
	st := &Store{db: db, shellEventEncoder: shellEventEncoder, shellEventDecoder: shellEventDecoder}
	if err := st.initializeSchema(ctx); err != nil {
		shellEventDecoder.Close()
		shellEventEncoder.Close()
		db.Close()
		return nil, err
	}
	return st, nil
}

func (s *Store) Close() error {
	s.shellEventDecoder.Close()
	return errors.Join(s.shellEventEncoder.Close(), s.db.Close())
}

type scanner interface{ Scan(...any) error }

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
