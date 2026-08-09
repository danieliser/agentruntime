// Package sqlite implements the durable Store contract with one SQLite file.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"github.com/danieliser/agentruntime/pkg/durable"
	_ "modernc.org/sqlite"
)

const schemaVersion = 2

//go:embed migrations/*.sql
var migrations embed.FS

// Store is AgentD's single-file SQLite durable store.
type Store struct {
	db     *sql.DB
	path   string
	mu     sync.RWMutex
	closed bool
}

// Open opens or creates the durable database at path and applies reviewed
// migrations. The parent directory and database are private to the user.
func Open(path string) (*Store, error) {
	const op = "open_store"
	if path == "" {
		return nil, durable.NewError(durable.CodeInvalidArgument, op, "database path is required", nil)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, durable.NewError(durable.CodeInvalidArgument, op, "resolve database path", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return nil, durable.NewError(durable.CodeIndeterminate, op, "create database directory", err)
	}
	if err := os.Chmod(filepath.Dir(absolute), 0o700); err != nil {
		return nil, durable.NewError(durable.CodeIndeterminate, op, "secure database directory", err)
	}

	db, err := sql.Open("sqlite", dataSourceName(absolute))
	if err != nil {
		return nil, durable.NewError(durable.CodeIndeterminate, op, "open database", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, path: absolute}
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, durable.NewError(durable.CodeIndeterminate, op, "connect to database", err)
	}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(absolute, 0o600); err != nil {
		_ = db.Close()
		return nil, durable.NewError(durable.CodeIndeterminate, op, "secure database file", err)
	}
	if err := store.verifyConfiguration(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func dataSourceName(path string) string {
	location := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := location.Query()
	for _, pragma := range []string{
		"foreign_keys(1)",
		"journal_mode(WAL)",
		"synchronous(FULL)",
		"busy_timeout(5000)",
		"temp_store(MEMORY)",
	} {
		query.Add("_pragma", pragma)
	}
	query.Set("_txlock", "immediate")
	location.RawQuery = query.Encode()
	return location.String()
}

func readOnlyDataSourceName(path string) string {
	location := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := location.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "foreign_keys(1)")
	location.RawQuery = query.Encode()
	return location.String()
}

func (store *Store) migrate(ctx context.Context) error {
	const op = "migrate_store"
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return storageError(op, "read schema version", err)
	}
	if version > schemaVersion {
		return durable.NewError(durable.CodeIndeterminate, op, "database schema is newer than this AgentD", nil)
	}
	for next := version + 1; next <= schemaVersion; next++ {
		name := fmt.Sprintf("migrations/%03d_durable_store_v%d.sql", next, next)
		migration, err := migrations.ReadFile(name)
		if err != nil {
			return storageError(op, "read embedded migration", err)
		}
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			return storageError(op, "begin migration", err)
		}
		if _, err := tx.ExecContext(ctx, string(migration)); err != nil {
			_ = tx.Rollback()
			return storageError(op, "apply migration", err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", next)); err != nil {
			_ = tx.Rollback()
			return storageError(op, "record schema version", err)
		}
		if err := tx.Commit(); err != nil {
			return storageError(op, "commit migration", err)
		}
	}
	return nil
}

func (store *Store) verifyConfiguration(ctx context.Context) error {
	const op = "verify_store_configuration"
	expected := map[string]string{
		"foreign_keys": "1",
		"journal_mode": "wal",
		"synchronous":  "2",
		"busy_timeout": "5000",
		"temp_store":   "2",
	}
	for pragma, want := range expected {
		var got string
		if err := store.db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
			return storageError(op, "read SQLite pragma", err)
		}
		if got != want {
			return durable.NewError(durable.CodeIndeterminate, op, fmt.Sprintf("SQLite %s=%s, want %s", pragma, got, want), nil)
		}
	}
	return nil
}

func (store *Store) begin(ctx context.Context, op string) (*sql.Tx, error) {
	store.mu.RLock()
	closed := store.closed
	store.mu.RUnlock()
	if closed {
		return nil, durable.NewError(durable.CodeStoreClosed, op, "store is closed", nil)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, storageError(op, "begin transaction", err)
	}
	return tx, nil
}

// Close releases the database. It is safe to call more than once.
func (store *Store) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil
	}
	store.closed = true
	return store.db.Close()
}

func storageError(op, message string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		return durable.NewError(durable.CodeNotFound, op, message, nil)
	}
	return durable.NewError(durable.CodeIndeterminate, op, message, err)
}

func rollback(tx *sql.Tx) {
	_ = tx.Rollback()
}
