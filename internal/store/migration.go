package store

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/wxia529/joyrun/internal/fault"
	_ "modernc.org/sqlite"
)

// UpgradeStable2 explicitly upgrades a stable-1 database. It never runs from
// Open, so a daemon or normal command cannot silently change user data.
func UpgradeStable2(ctx context.Context, database string, dryRun bool) (string, error) {
	if database == "" {
		return "", fault.New("DATABASE_UPGRADE_REQUIRED", "database path is required", false)
	}
	if _, err := os.Stat(database); err != nil {
		return "", fault.Wrap("DATABASE_FAILED", "cannot inspect database", false, err)
	}
	backup := database + ".stable-1." + time.Now().UTC().Format("20060102T150405Z") + ".bak"
	if dryRun {
		return backup, validateMigration(ctx, database)
	}
	if err := checkpointDatabase(ctx, database); err != nil {
		return "", err
	}
	if err := copyFile(database, backup); err != nil {
		return "", fault.Wrap("DATABASE_BACKUP_FAILED", "cannot create database backup", false, err)
	}
	db, err := sql.Open("sqlite", database)
	if err != nil {
		return backup, fault.Wrap("DATABASE_FAILED", "cannot open database", false, err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return backup, fault.Wrap("DATABASE_FAILED", "cannot acquire database connection", false, err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return backup, err
	}
	if err = validateMigrationConn(ctx, conn); err != nil {
		return backup, err
	}
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return backup, fault.Wrap("DATABASE_BUSY", "cannot lock database for upgrade", true, err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if _, err = conn.ExecContext(ctx, operationSchema); err != nil {
		return backup, fault.Wrap("DATABASE_UPGRADE_FAILED", "cannot create operation tables", false, err)
	}
	if _, err = conn.ExecContext(ctx, "UPDATE joyrun_meta SET value='stable-2' WHERE key='schema_label'"); err != nil {
		return backup, fault.Wrap("DATABASE_UPGRADE_FAILED", "cannot update schema label", false, err)
	}
	if _, err = conn.ExecContext(ctx, "PRAGMA user_version=1"); err != nil {
		return backup, fault.Wrap("DATABASE_UPGRADE_FAILED", "cannot record schema version", false, err)
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return backup, fault.Wrap("DATABASE_UPGRADE_FAILED", "cannot commit database upgrade", true, err)
	}
	committed = true
	return backup, nil
}

func checkpointDatabase(ctx context.Context, database string) error {
	db, err := sql.Open("sqlite", database)
	if err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot open database for checkpoint", false, err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return fault.Wrap("DATABASE_BUSY", "cannot configure database checkpoint", true, err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(FULL)"); err != nil {
		return fault.Wrap("DATABASE_BUSY", "cannot checkpoint database before backup", true, err)
	}
	return nil
}

func validateMigration(ctx context.Context, database string) error {
	db, err := sql.Open("sqlite", database)
	if err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot open database", false, err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return validateMigrationConn(ctx, conn)
}

func validateMigrationConn(ctx context.Context, conn *sql.Conn) error {
	var channel, label string
	if err := conn.QueryRowContext(ctx, "SELECT value FROM joyrun_meta WHERE key='release_channel'").Scan(&channel); err != nil {
		return fault.Wrap("DATABASE_UPGRADE_REQUIRED", "database is not a marked stable-1 JoyRun database", false, err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT value FROM joyrun_meta WHERE key='schema_label'").Scan(&label); err != nil {
		return fault.Wrap("DATABASE_UPGRADE_REQUIRED", "database schema label is missing", false, err)
	}
	if channel != "stable" || label != "stable-1" {
		return fault.New("DATABASE_UPGRADE_REQUIRED", fmt.Sprintf("database is %s/%s; expected stable/stable-1", channel, label), false)
	}
	return nil
}

const operationSchema = `
CREATE TABLE IF NOT EXISTS operations (
  id TEXT PRIMARY KEY, kind TEXT NOT NULL, project_id TEXT NOT NULL,
  cluster_key TEXT NOT NULL DEFAULT '', dedup_key TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL, stage TEXT NOT NULL, payload TEXT NOT NULL, result TEXT NOT NULL DEFAULT '{}',
  attempt INTEGER NOT NULL DEFAULT 0, max_attempts INTEGER NOT NULL DEFAULT 0,
  retry_deadline_at TEXT, next_attempt_at TEXT, lease_owner TEXT NOT NULL DEFAULT '', lease_expires_at TEXT,
  error_code TEXT NOT NULL DEFAULT '', error_message TEXT NOT NULL DEFAULT '', retryable INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, started_at TEXT, updated_at TEXT NOT NULL, finished_at TEXT,
  FOREIGN KEY(project_id) REFERENCES projects(id)
);
CREATE INDEX IF NOT EXISTS idx_operations_state ON operations(state, next_attempt_at);
CREATE INDEX IF NOT EXISTS idx_operations_project ON operations(project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_operations_cluster ON operations(cluster_key, state);
CREATE UNIQUE INDEX IF NOT EXISTS idx_operations_dedup ON operations(kind, dedup_key) WHERE dedup_key <> '';
CREATE TABLE IF NOT EXISTS operation_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT, operation_id TEXT NOT NULL, state TEXT NOT NULL,
  stage TEXT NOT NULL, message TEXT NOT NULL DEFAULT '', data TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL,
  FOREIGN KEY(operation_id) REFERENCES operations(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_operation_events ON operation_events(operation_id, id);
CREATE TABLE IF NOT EXISTS operation_tasks (
  operation_id TEXT NOT NULL, task_id TEXT NOT NULL, ordinal INTEGER NOT NULL,
  state TEXT NOT NULL DEFAULT 'pending', result TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY(operation_id, task_id), UNIQUE(operation_id, ordinal),
  FOREIGN KEY(operation_id) REFERENCES operations(id) ON DELETE CASCADE,
  FOREIGN KEY(task_id) REFERENCES tasks(id)
);
CREATE INDEX IF NOT EXISTS idx_operation_tasks_task ON operation_tasks(task_id, operation_id);
CREATE TABLE IF NOT EXISTS transfer_items (
  operation_id TEXT NOT NULL, task_id TEXT NOT NULL, ordinal INTEGER NOT NULL,
  remote_path TEXT NOT NULL, local_path TEXT NOT NULL,
  expected_size INTEGER NOT NULL DEFAULT 0, expected_sha256 TEXT NOT NULL DEFAULT '',
  transferred_size INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL,
  error_code TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL,
  PRIMARY KEY(operation_id, task_id, remote_path),
  FOREIGN KEY(operation_id) REFERENCES operations(id) ON DELETE CASCADE,
  FOREIGN KEY(task_id) REFERENCES tasks(id)
);
CREATE INDEX IF NOT EXISTS idx_transfer_items_operation ON transfer_items(operation_id, ordinal);
CREATE TABLE IF NOT EXISTS cluster_runtime (
  cluster_key TEXT PRIMARY KEY, config_hash TEXT NOT NULL, cluster_name TEXT NOT NULL,
  last_contact_at TEXT, last_success_at TEXT, last_error_code TEXT NOT NULL DEFAULT '',
  next_poll_at TEXT, consecutive_failures INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL
);
`

func copyFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
