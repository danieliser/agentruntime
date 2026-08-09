package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
)

// BackupMetadata is the tamper-evident manifest written beside every snapshot.
type BackupMetadata struct {
	SchemaVersion  int                 `json:"schema_version"`
	CreatedAt      time.Time           `json:"created_at"`
	DatabaseSHA256 string              `json:"database_sha256"`
	SessionTails   []BackupSessionTail `json:"session_tails"`
}

// BackupSessionTail records the committed replay boundary in the snapshot.
type BackupSessionTail struct {
	SessionID    string `json:"session_id"`
	LastSequence int64  `json:"last_sequence"`
}

// CheckIntegrity verifies SQLite structure plus AgentD's sequence, lifecycle,
// receipt, and generation invariants without attempting silent repair.
func (store *Store) CheckIntegrity(ctx context.Context) error {
	const op = "check_store_integrity"
	tx, err := store.begin(ctx, op)
	if err != nil {
		return err
	}
	defer rollback(tx)
	rows, err := tx.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return storageError(op, "run SQLite quick check", err)
	}
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			_ = rows.Close()
			return storageError(op, "read SQLite quick check", err)
		}
		if result != "ok" {
			_ = rows.Close()
			return durable.NewError(durable.CodeIndeterminate, op, "SQLite quick check failed", nil)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return storageError(op, "iterate quick-check rows", err)
	}
	if err := rows.Close(); err != nil {
		return storageError(op, "close quick-check rows", err)
	}

	rows, err = tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return storageError(op, "run foreign-key check", err)
	}
	if rows.Next() {
		_ = rows.Close()
		return durable.NewError(durable.CodeIndeterminate, op, "foreign-key check failed", nil)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return storageError(op, "iterate foreign-key rows", err)
	}
	if err := rows.Close(); err != nil {
		return storageError(op, "close foreign-key rows", err)
	}

	var sessionID string
	err = tx.QueryRowContext(ctx, `SELECT s.id
FROM sessions s LEFT JOIN events e ON e.session_id = s.id
GROUP BY s.id, s.last_sequence
HAVING s.last_sequence <> COUNT(e.sequence)
   OR (COUNT(e.sequence) > 0 AND (MIN(e.sequence) <> 1 OR MAX(e.sequence) <> s.last_sequence))
LIMIT 1`).Scan(&sessionID)
	if err == nil {
		return durable.NewError(durable.CodeEventGap, op, "session event sequence is not contiguous", nil)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return storageError(op, "validate event sequences", err)
	}
	rows, err = tx.QueryContext(ctx, "SELECT raw, raw_sha256 FROM events")
	if err != nil {
		return storageError(op, "query event hashes", err)
	}
	for rows.Next() {
		var raw []byte
		var storedHash string
		if err := rows.Scan(&raw, &storedHash); err != nil {
			_ = rows.Close()
			return storageError(op, "read event hash", err)
		}
		digest := sha256.Sum256(raw)
		if storedHash != "sha256:"+hex.EncodeToString(digest[:]) {
			_ = rows.Close()
			return durable.NewError(durable.CodeIndeterminate, op, "event raw hash does not match stored bytes", nil)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return storageError(op, "iterate event hashes", err)
	}
	if err := rows.Close(); err != nil {
		return storageError(op, "close event-hash rows", err)
	}

	err = tx.QueryRowContext(ctx, `SELECT s.id
FROM sessions s
LEFT JOIN terminal_receipts r ON r.session_id = s.id
WHERE (
    s.state IN ('completed', 'failed', 'cancelled', 'timed_out', 'crashed', 'indeterminate')
    AND (r.session_id IS NULL OR r.state <> s.state OR r.generation <> s.active_generation OR r.last_sequence <> s.last_sequence)
) OR (
    s.state NOT IN ('completed', 'failed', 'cancelled', 'timed_out', 'crashed', 'indeterminate')
    AND r.session_id IS NOT NULL
)
LIMIT 1`).Scan(&sessionID)
	if err == nil {
		return durable.NewError(durable.CodeIndeterminate, op, "terminal session and receipt disagree", nil)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return storageError(op, "validate terminal receipts", err)
	}

	err = tx.QueryRowContext(ctx, `SELECT s.id
FROM sessions s
LEFT JOIN (
    SELECT session_id, MAX(generation) AS highest_generation
    FROM runtime_generations GROUP BY session_id
) g ON g.session_id = s.id
WHERE s.active_generation <> COALESCE(g.highest_generation, 0)
LIMIT 1`).Scan(&sessionID)
	if err == nil {
		return durable.NewError(durable.CodeIndeterminate, op, "active generation does not match durable history", nil)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return storageError(op, "validate active generations", err)
	}
	if err := tx.Commit(); err != nil {
		return storageError(op, "commit integrity check", err)
	}
	return nil
}

// Backup writes a consistent SQLite snapshot to a new private file. Existing
// destinations are never overwritten.
func (store *Store) Backup(ctx context.Context, destination string) error {
	const op = "backup_store"
	if destination == "" {
		return durable.NewError(durable.CodeInvalidArgument, op, "backup destination is required", nil)
	}
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return durable.NewError(durable.CodeInvalidArgument, op, "resolve backup destination", err)
	}
	if absolute == store.path {
		return durable.NewError(durable.CodeInvalidArgument, op, "backup destination must differ from the live database", nil)
	}
	metadataPath := absolute + ".metadata.json"
	if _, err := os.Lstat(absolute); err == nil {
		return durable.NewError(durable.CodeImmutableConflict, op, "backup destination already exists", nil)
	} else if !errors.Is(err, os.ErrNotExist) {
		return storageError(op, "inspect backup destination", err)
	}
	if _, err := os.Lstat(metadataPath); err == nil {
		return durable.NewError(durable.CodeImmutableConflict, op, "backup metadata destination already exists", nil)
	} else if !errors.Is(err, os.ErrNotExist) {
		return storageError(op, "inspect backup metadata destination", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return storageError(op, "create backup directory", err)
	}
	if err := os.Chmod(filepath.Dir(absolute), 0o700); err != nil {
		return storageError(op, "secure backup directory", err)
	}
	store.mu.RLock()
	closed := store.closed
	store.mu.RUnlock()
	if closed {
		return durable.NewError(durable.CodeStoreClosed, op, "store is closed", nil)
	}
	if _, err := store.db.ExecContext(ctx, "PRAGMA wal_checkpoint(FULL)"); err != nil {
		return storageError(op, "checkpoint write-ahead log", err)
	}
	if _, err := store.db.ExecContext(ctx, "VACUUM INTO ?", absolute); err != nil {
		if _, statErr := os.Lstat(absolute); statErr == nil {
			return durable.NewError(durable.CodeImmutableConflict, op, "backup destination exists", err)
		}
		return storageError(op, "create backup snapshot", err)
	}
	if err := os.Chmod(absolute, 0o600); err != nil {
		return storageError(op, "secure backup file", err)
	}
	metadata, err := inspectBackup(ctx, absolute)
	if err != nil {
		return storageError(op, "inspect backup snapshot", err)
	}
	metadataBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return storageError(op, "encode backup metadata", err)
	}
	metadataBytes = append(metadataBytes, '\n')
	file, err := os.OpenFile(metadataPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return durable.NewError(durable.CodeImmutableConflict, op, "backup metadata destination already exists", err)
		}
		return storageError(op, "create backup metadata", err)
	}
	if _, err := file.Write(metadataBytes); err != nil {
		_ = file.Close()
		return storageError(op, "write backup metadata", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return storageError(op, "sync backup metadata", err)
	}
	if err := file.Close(); err != nil {
		return storageError(op, "close backup metadata", err)
	}
	return nil
}

func inspectBackup(ctx context.Context, path string) (BackupMetadata, error) {
	file, err := os.Open(path)
	if err != nil {
		return BackupMetadata{}, err
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		_ = file.Close()
		return BackupMetadata{}, err
	}
	if err := file.Close(); err != nil {
		return BackupMetadata{}, err
	}
	db, err := sql.Open("sqlite", readOnlyDataSourceName(path))
	if err != nil {
		return BackupMetadata{}, err
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT id, last_sequence FROM sessions
WHERE state NOT IN ('completed', 'failed', 'cancelled', 'timed_out', 'crashed', 'indeterminate')
ORDER BY id`)
	if err != nil {
		return BackupMetadata{}, err
	}
	defer rows.Close()
	metadata := BackupMetadata{
		SchemaVersion:  schemaVersion,
		CreatedAt:      time.Now().UTC().Round(0),
		DatabaseSHA256: "sha256:" + hex.EncodeToString(digest.Sum(nil)),
	}
	for rows.Next() {
		var tail BackupSessionTail
		if err := rows.Scan(&tail.SessionID, &tail.LastSequence); err != nil {
			return BackupMetadata{}, err
		}
		metadata.SessionTails = append(metadata.SessionTails, tail)
	}
	if err := rows.Err(); err != nil {
		return BackupMetadata{}, err
	}
	return metadata, nil
}
