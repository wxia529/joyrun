package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/model"
	_ "modernc.org/sqlite"
)

const (
	schemaVersion = 1
	schemaChannel = "stable"
	schemaLabel   = "stable-2"
	dryRunKey     = "dry_run"
)

// SchemaLabel is exposed to the daemon handshake and diagnostics.
const SchemaLabel = schemaChannel + "/" + schemaLabel

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fault.Wrap("DATABASE_FAILED", "cannot create database directory", false, err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fault.Wrap("DATABASE_FAILED", "cannot open task database", false, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	if err := s.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) initialize(ctx context.Context) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot acquire database connection", true, err)
	}
	defer conn.Close()
	for _, pragma := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := conn.ExecContext(ctx, pragma); err != nil {
			return fault.Wrap("DATABASE_FAILED", "cannot configure task database", true, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fault.Wrap("DATABASE_BUSY", "cannot acquire database initialization lock", true, err)
	}
	defer func() {
		if err != nil {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var version int
	if err = conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot read database schema version", false, err)
	}
	switch version {
	case 0:
		var existing int
		if err = conn.QueryRowContext(ctx, `
SELECT count(*) FROM sqlite_master
WHERE type='table' AND name IN ('joyrun_meta','projects','tasks','task_events')`).Scan(&existing); err != nil {
			return fault.Wrap("DATABASE_FAILED", "cannot inspect task database", false, err)
		}
		if existing != 0 {
			return fault.New("DATABASE_UNSUPPORTED",
				"unversioned JoyRun database is not supported; remove it or set JOYRUN_DB to a new path", false)
		}
		_, err = conn.ExecContext(ctx, stableSchema)
	case schemaVersion:
		var metadataTable int
		if err = conn.QueryRowContext(ctx, `
SELECT count(*) FROM sqlite_master
WHERE type='table' AND name='joyrun_meta'`).Scan(&metadataTable); err != nil {
			return fault.Wrap("DATABASE_FAILED", "cannot inspect database metadata", false, err)
		}
		if metadataTable == 0 {
			return fault.New("DATABASE_UNSUPPORTED",
				"database schema is not marked as a supported JoyRun database; remove it or set JOYRUN_DB to a new path", false)
		}
	default:
		return fault.New("DATABASE_UNSUPPORTED",
			fmt.Sprintf("database schema version %d is unsupported; remove it or set JOYRUN_DB to a new path", version), false)
	}
	if err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot initialize task database", false, err)
	}
	var channel, label string
	if err = conn.QueryRowContext(ctx,
		"SELECT value FROM joyrun_meta WHERE key='release_channel'").Scan(&channel); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fault.New("DATABASE_UNSUPPORTED",
				"database release channel metadata is missing; remove it or set JOYRUN_DB to a new path", false)
		}
		return fault.Wrap("DATABASE_FAILED", "cannot read database release channel", false, err)
	}
	if err = conn.QueryRowContext(ctx,
		"SELECT value FROM joyrun_meta WHERE key='schema_label'").Scan(&label); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fault.New("DATABASE_UNSUPPORTED",
				"database schema label metadata is missing; remove it or set JOYRUN_DB to a new path", false)
		}
		return fault.Wrap("DATABASE_FAILED", "cannot read database schema label", false, err)
	}
	if channel != schemaChannel || label != schemaLabel {
		if channel == schemaChannel && label == "stable-1" && schemaLabel == "stable-2" {
			return fault.New("DATABASE_UPGRADE_REQUIRED",
				"database is stable/stable-1; run `joyrun database upgrade --to stable-2` before using this build", false)
		}
		return fault.New("DATABASE_UNSUPPORTED",
			fmt.Sprintf("database is %s/%s; this build requires %s/%s",
				channel, label, schemaChannel, schemaLabel), false)
	}
	if _, err = conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", schemaVersion)); err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot record database schema version", false, err)
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot commit database initialization", true, err)
	}
	return nil
}

const stableSchema = `
CREATE TABLE IF NOT EXISTS joyrun_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
INSERT OR IGNORE INTO joyrun_meta(key,value) VALUES
  ('release_channel','stable'),
  ('schema_label','stable-2');
CREATE TABLE IF NOT EXISTS projects (
  id TEXT PRIMARY KEY,
  last_path TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY,
  revision INTEGER NOT NULL,
  project_id TEXT NOT NULL,
  source_path TEXT NOT NULL,
  source_workdir TEXT NOT NULL,
  source_entry TEXT,
  target_name TEXT NOT NULL,
  cluster_name TEXT NOT NULL,
  remote_dir TEXT NOT NULL,
  scheduler_id TEXT NOT NULL DEFAULT '',
  compute_state TEXT NOT NULL,
  pull_state TEXT NOT NULL,
  scheduler_state TEXT NOT NULL DEFAULT '',
  scheduler_reason TEXT NOT NULL DEFAULT '',
  elapsed TEXT NOT NULL DEFAULT '',
  exit_code TEXT NOT NULL DEFAULT '',
  scheduler_start TEXT NOT NULL DEFAULT '',
  scheduler_end TEXT NOT NULL DEFAULT '',
  resolved_params TEXT NOT NULL,
  rendered_script TEXT NOT NULL,
  target_hash TEXT NOT NULL,
  input_manifest TEXT NOT NULL,
  pull_patterns TEXT NOT NULL,
  push_excludes TEXT NOT NULL,
  logs TEXT NOT NULL,
  metadata TEXT NOT NULL,
  created_at TEXT NOT NULL,
  submitted_at TEXT,
  updated_at TEXT NOT NULL,
  pulled_at TEXT,
  FOREIGN KEY(project_id) REFERENCES projects(id)
);
CREATE TABLE IF NOT EXISTS task_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id TEXT NOT NULL,
  type TEXT NOT NULL,
  stage TEXT NOT NULL,
  message TEXT NOT NULL DEFAULT '',
  data TEXT NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_tasks_source ON tasks(project_id, source_path, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_project ON tasks(project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_task ON task_events(task_id, id);
CREATE TABLE IF NOT EXISTS operations (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  project_id TEXT NOT NULL,
  cluster_key TEXT NOT NULL DEFAULT '',
  dedup_key TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  stage TEXT NOT NULL,
  payload TEXT NOT NULL,
  result TEXT NOT NULL DEFAULT '{}',
  attempt INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL DEFAULT 0,
  retry_deadline_at TEXT,
  next_attempt_at TEXT,
  lease_owner TEXT NOT NULL DEFAULT '',
  lease_expires_at TEXT,
  error_code TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  retryable INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  started_at TEXT,
  updated_at TEXT NOT NULL,
  finished_at TEXT,
  FOREIGN KEY(project_id) REFERENCES projects(id)
);
CREATE INDEX IF NOT EXISTS idx_operations_state ON operations(state, next_attempt_at);
CREATE INDEX IF NOT EXISTS idx_operations_project ON operations(project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_operations_cluster ON operations(cluster_key, state);
CREATE UNIQUE INDEX IF NOT EXISTS idx_operations_dedup
  ON operations(kind, dedup_key) WHERE dedup_key <> '';
CREATE TABLE IF NOT EXISTS operation_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  operation_id TEXT NOT NULL,
  state TEXT NOT NULL,
  stage TEXT NOT NULL,
  message TEXT NOT NULL DEFAULT '',
  data TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  FOREIGN KEY(operation_id) REFERENCES operations(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_operation_events ON operation_events(operation_id, id);
CREATE TABLE IF NOT EXISTS operation_tasks (
  operation_id TEXT NOT NULL,
  task_id TEXT NOT NULL,
  ordinal INTEGER NOT NULL,
  state TEXT NOT NULL DEFAULT 'pending',
  result TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY(operation_id, task_id),
  UNIQUE(operation_id, ordinal),
  FOREIGN KEY(operation_id) REFERENCES operations(id) ON DELETE CASCADE,
  FOREIGN KEY(task_id) REFERENCES tasks(id)
);
CREATE INDEX IF NOT EXISTS idx_operation_tasks_task ON operation_tasks(task_id, operation_id);
CREATE TABLE IF NOT EXISTS transfer_items (
  operation_id TEXT NOT NULL,
  task_id TEXT NOT NULL,
  ordinal INTEGER NOT NULL,
  remote_path TEXT NOT NULL,
  local_path TEXT NOT NULL,
  expected_size INTEGER NOT NULL DEFAULT 0,
  expected_sha256 TEXT NOT NULL DEFAULT '',
  transferred_size INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL,
  error_code TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY(operation_id, task_id, remote_path),
  FOREIGN KEY(operation_id) REFERENCES operations(id) ON DELETE CASCADE,
  FOREIGN KEY(task_id) REFERENCES tasks(id)
);
CREATE INDEX IF NOT EXISTS idx_transfer_items_operation ON transfer_items(operation_id, ordinal);
CREATE TABLE IF NOT EXISTS cluster_runtime (
  cluster_key TEXT PRIMARY KEY,
  config_hash TEXT NOT NULL,
  cluster_name TEXT NOT NULL,
  last_contact_at TEXT,
  last_success_at TEXT,
  last_error_code TEXT NOT NULL DEFAULT '',
  next_poll_at TEXT,
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL
);
`

func (s *Store) BindProject(ctx context.Context, project model.Project) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO projects(id,last_path,updated_at) VALUES(?,?,?)
ON CONFLICT(id) DO UPDATE SET last_path=excluded.last_path,updated_at=excluded.updated_at`,
		project.ProjectID, project.Root, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot bind project path", true, err)
	}
	return nil
}

func (s *Store) ListProjects(ctx context.Context) ([]model.Project, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,last_path FROM projects ORDER BY updated_at DESC")
	if err != nil {
		return nil, fault.Wrap("DATABASE_FAILED", "cannot list projects", true, err)
	}
	defer rows.Close()
	var result []model.Project
	for rows.Next() {
		var p model.Project
		if err := rows.Scan(&p.ProjectID, &p.Root); err != nil {
			return nil, fault.Wrap("DATABASE_FAILED", "cannot decode project", false, err)
		}
		result = append(result, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fault.Wrap("DATABASE_FAILED", "cannot finish listing projects", true, err)
	}
	return result, nil
}

// GetProject returns the most recently bound local path for a Project ID. The
// daemon uses this indirection so queued Operations continue to find a moved
// Project without trusting the absolute path captured at admission time.
func (s *Store) GetProject(ctx context.Context, projectID string) (model.Project, error) {
	var p model.Project
	err := s.db.QueryRowContext(ctx, "SELECT id,last_path FROM projects WHERE id=?", projectID).
		Scan(&p.ProjectID, &p.Root)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Project{}, fault.New("PROJECT_NOT_FOUND", fmt.Sprintf("project %s is not registered", projectID), false)
	}
	if err != nil {
		return model.Project{}, fault.Wrap("DATABASE_FAILED", "cannot read project binding", true, err)
	}
	return p, nil
}

func (s *Store) CreateTask(ctx context.Context, task *model.Task) error {
	return s.CreateTasks(ctx, []*model.Task{task})
}

// FindTaskBySubmissionKey returns the newest non-rejected task for one source
// and immutable submission fingerprint. It is used to make a retried batch
// operation reuse tasks already admitted before a worker crash.
func (s *Store) FindTaskBySubmissionKey(ctx context.Context, projectID, sourcePath, key string) (model.Task, bool, error) {
	if projectID == "" || sourcePath == "" || key == "" {
		return model.Task{}, false, fault.New("DATABASE_FAILED", "project, source, and submission key are required", false)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+columns+
		" FROM tasks WHERE project_id=? AND source_path=? ORDER BY created_at DESC",
		projectID, sourcePath)
	if err != nil {
		return model.Task{}, false, fault.Wrap("DATABASE_FAILED", "cannot inspect task submissions", true, err)
	}
	defer rows.Close()
	for rows.Next() {
		task, scanErr := scanTask(rows)
		if scanErr != nil {
			return model.Task{}, false, fault.Wrap("DATABASE_FAILED", "cannot decode task submission", false, scanErr)
		}
		if task.Metadata["submission_key"] == key && task.ComputeState != model.ComputeSubmissionFailed {
			return task, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return model.Task{}, false, fault.Wrap("DATABASE_FAILED", "cannot finish task submission inspection", true, err)
	}
	return model.Task{}, false, nil
}

// CreateTaskIdempotent reserves a task submission fingerprint while creating
// the task record. SQLite serializes the project UPDATE, so two JoyRun
// processes racing with the same fingerprint cannot both pass the duplicate
// check. A previously definitely rejected submission may be retried; an
// accepted, uncertain, or otherwise active submission is returned instead.
func (s *Store) CreateTaskIdempotent(ctx context.Context, task *model.Task, forceNew bool) (model.Task, bool, error) {
	if forceNew {
		return model.Task{}, false, s.CreateTask(ctx, task)
	}
	if task.Metadata == nil || task.Metadata["submission_key"] == "" {
		return model.Task{}, false, fault.New("DATABASE_FAILED",
			"cannot reserve task without a submission key", false)
	}
	if task.Revision == 0 {
		task.Revision = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Task{}, false, fault.Wrap("DATABASE_BUSY", "cannot begin task reservation", true, err)
	}
	defer tx.Rollback()
	// Acquire SQLite's write lock before reading candidates. This makes the
	// check-and-insert operation atomic across independent JoyRun processes.
	if _, err := tx.ExecContext(ctx,
		"UPDATE projects SET updated_at=updated_at WHERE id=?", task.ProjectID); err != nil {
		return model.Task{}, false, fault.Wrap("DATABASE_BUSY", "cannot reserve task submission", true, err)
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+columns+
		" FROM tasks WHERE project_id=? AND source_path=? ORDER BY created_at DESC",
		task.ProjectID, task.SourcePath)
	if err != nil {
		return model.Task{}, false, fault.Wrap("DATABASE_FAILED", "cannot inspect task submissions", true, err)
	}
	for rows.Next() {
		existing, scanErr := scanTask(rows)
		if scanErr != nil {
			rows.Close()
			return model.Task{}, false, fault.Wrap("DATABASE_FAILED", "cannot decode task submission", false, scanErr)
		}
		if existing.Metadata["submission_key"] == task.Metadata["submission_key"] &&
			existing.ComputeState != model.ComputeSubmissionFailed {
			rows.Close()
			return existing, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return model.Task{}, false, fault.Wrap("DATABASE_FAILED", "cannot finish task submission inspection", true, err)
	}
	rows.Close()
	if err := insertTask(ctx, tx, *task); err != nil {
		return model.Task{}, false, err
	}
	if err := insertEvent(ctx, tx, model.TaskEvent{
		TaskID: task.ID, Type: "TASK_CREATED", Stage: "task",
		Message: "Task record created", CreatedAt: task.CreatedAt,
	}); err != nil {
		return model.Task{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return model.Task{}, false, fault.Wrap("DATABASE_FAILED", "cannot commit task reservation", true, err)
	}
	return model.Task{}, false, nil
}

// CreateTasks atomically creates a batch of independent task records.
func (s *Store) CreateTasks(ctx context.Context, tasks []*model.Task) error {
	if len(tasks) == 0 {
		return nil
	}
	for _, task := range tasks {
		if task.Revision == 0 {
			task.Revision = 1
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fault.Wrap("DATABASE_BUSY", "cannot begin task transaction", true, err)
	}
	defer tx.Rollback()
	for _, task := range tasks {
		if err := insertTask(ctx, tx, *task); err != nil {
			return err
		}
		event := model.TaskEvent{
			TaskID: task.ID, Type: "TASK_CREATED", Stage: "task",
			Message: "Task record created", CreatedAt: task.CreatedAt,
		}
		if err := insertEvent(ctx, tx, event); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot commit task creation", true, err)
	}
	return nil
}

// CreateTasksIdempotent is the batch equivalent of CreateTaskIdempotent. A
// duplicate aborts the whole batch before any new scheduler submission can be
// attempted, preserving all-or-nothing admission semantics.
func (s *Store) CreateTasksIdempotent(ctx context.Context, tasks []*model.Task, forceNew bool) (model.Task, bool, error) {
	if forceNew {
		return model.Task{}, false, s.CreateTasks(ctx, tasks)
	}
	if len(tasks) == 0 {
		return model.Task{}, false, nil
	}
	for _, task := range tasks {
		if task.Metadata == nil || task.Metadata["submission_key"] == "" {
			return model.Task{}, false, fault.New("DATABASE_FAILED",
				"cannot reserve task without a submission key", false)
		}
		if task.Revision == 0 {
			task.Revision = 1
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Task{}, false, fault.Wrap("DATABASE_BUSY", "cannot begin task reservation", true, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		"UPDATE projects SET updated_at=updated_at WHERE id=?", tasks[0].ProjectID); err != nil {
		return model.Task{}, false, fault.Wrap("DATABASE_BUSY", "cannot reserve task submission", true, err)
	}
	for _, task := range tasks {
		rows, err := tx.QueryContext(ctx, "SELECT "+columns+
			" FROM tasks WHERE project_id=? AND source_path=? ORDER BY created_at DESC",
			task.ProjectID, task.SourcePath)
		if err != nil {
			return model.Task{}, false, fault.Wrap("DATABASE_FAILED", "cannot inspect task submissions", true, err)
		}
		for rows.Next() {
			existing, scanErr := scanTask(rows)
			if scanErr != nil {
				rows.Close()
				return model.Task{}, false, fault.Wrap("DATABASE_FAILED", "cannot decode task submission", false, scanErr)
			}
			if existing.Metadata["submission_key"] == task.Metadata["submission_key"] &&
				existing.ComputeState != model.ComputeSubmissionFailed {
				rows.Close()
				return existing, true, nil
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return model.Task{}, false, fault.Wrap("DATABASE_FAILED", "cannot finish task submission inspection", true, err)
		}
		rows.Close()
	}
	for _, task := range tasks {
		if err := insertTask(ctx, tx, *task); err != nil {
			return model.Task{}, false, err
		}
		if err := insertEvent(ctx, tx, model.TaskEvent{
			TaskID: task.ID, Type: "TASK_CREATED", Stage: "task",
			Message: "Task record created", CreatedAt: task.CreatedAt,
		}); err != nil {
			return model.Task{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return model.Task{}, false, fault.Wrap("DATABASE_FAILED", "cannot commit task reservation", true, err)
	}
	return model.Task{}, false, nil
}

func (s *Store) ImportTask(ctx context.Context, task *model.Task) error {
	if _, err := s.GetTask(ctx, task.ID); err == nil {
		return fault.New("TASK_ALREADY_EXISTS", fmt.Sprintf("task %s already exists", task.ID), false)
	} else if fault.As(err).Code != "TASK_NOT_FOUND" {
		return err
	}
	if task.Revision == 0 {
		task.Revision = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fault.Wrap("DATABASE_BUSY", "cannot begin task import", true, err)
	}
	defer tx.Rollback()
	if err := insertTask(ctx, tx, *task); err != nil {
		return err
	}
	if err := insertEvent(ctx, tx, model.TaskEvent{
		TaskID: task.ID, Type: "TASK_RECOVERED", Stage: "recovery",
		Message: "Task imported from remote metadata", CreatedAt: time.Now().UTC(),
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot commit task import", true, err)
	}
	return nil
}

func (s *Store) UpdateTask(ctx context.Context, task *model.Task) error {
	return s.updateTask(ctx, task, nil)
}

func (s *Store) UpdateTaskWithEvent(ctx context.Context, task *model.Task, event model.TaskEvent) error {
	if event.TaskID == "" {
		event.TaskID = task.ID
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	return s.updateTask(ctx, task, &event)
}

// UpdateTasksWithEvents atomically updates a batch of task records and appends
// one event for each task. Callers either observe the entire state transition
// or none of it.
func (s *Store) UpdateTasksWithEvents(
	ctx context.Context,
	tasks []*model.Task,
	events []model.TaskEvent,
) error {
	if len(tasks) == 0 {
		return nil
	}
	if len(events) != len(tasks) {
		return fault.New("DATABASE_FAILED",
			"batch task updates require one event per task", false)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fault.Wrap("DATABASE_BUSY", "cannot begin batch task update", true, err)
	}
	defer tx.Rollback()
	revisions := make([]int64, len(tasks))
	for index, task := range tasks {
		event := events[index]
		if event.TaskID == "" {
			event.TaskID = task.ID
		}
		if event.CreatedAt.IsZero() {
			event.CreatedAt = time.Now().UTC()
		}
		revision, err := updateTaskRecord(ctx, tx, task, &event)
		if err != nil {
			return err
		}
		revisions[index] = revision
	}
	if err := tx.Commit(); err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot commit batch task update", true, err)
	}
	for index, task := range tasks {
		task.Revision = revisions[index]
	}
	return nil
}

func (s *Store) updateTask(ctx context.Context, task *model.Task, event *model.TaskEvent) error {
	if task.Revision < 1 {
		return fault.New("DATABASE_CONFLICT", "task has no persistence revision", true)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fault.Wrap("DATABASE_BUSY", "cannot begin task update", true, err)
	}
	defer tx.Rollback()
	revision, err := updateTaskRecord(ctx, tx, task, event)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot commit task update", true, err)
	}
	task.Revision = revision
	return nil
}

func updateTaskRecord(
	ctx context.Context,
	tx *sql.Tx,
	task *model.Task,
	event *model.TaskEvent,
) (int64, error) {
	if task.Revision < 1 {
		return 0, fault.New("DATABASE_CONFLICT", "task has no persistence revision", true)
	}
	updated := *task
	updated.Revision++
	values, err := encodeTask(updated)
	if err != nil {
		return 0, err
	}
	values = append(values[1:], task.ID, task.Revision)
	result, err := tx.ExecContext(ctx, `
UPDATE tasks SET
 revision=?,project_id=?,source_path=?,source_workdir=?,source_entry=?,target_name=?,cluster_name=?,remote_dir=?,
 scheduler_id=?,compute_state=?,pull_state=?,scheduler_state=?,scheduler_reason=?,elapsed=?,exit_code=?,
 scheduler_start=?,scheduler_end=?,
 resolved_params=?,rendered_script=?,
 target_hash=?,input_manifest=?,pull_patterns=?,push_excludes=?,logs=?,metadata=?,created_at=?,
 submitted_at=?,updated_at=?,pulled_at=?
WHERE id=? AND revision=?`, values...)
	if err != nil {
		return 0, fault.Wrap("DATABASE_FAILED", "cannot update task record", true, err)
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return 0, fault.New("DATABASE_CONFLICT",
			fmt.Sprintf("task %s changed in another JoyRun process; reload it and retry", task.ID), true)
	}
	if event != nil {
		if err := insertEvent(ctx, tx, *event); err != nil {
			return 0, err
		}
	}
	return updated.Revision, nil
}

func (s *Store) AppendEvent(ctx context.Context, event model.TaskEvent) error {
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	if _, err := s.GetTask(ctx, event.TaskID); err != nil {
		return err
	}
	if err := insertEvent(ctx, s.db, event); err != nil {
		return err
	}
	return nil
}

func (s *Store) GetTask(ctx context.Context, id string) (model.Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM tasks WHERE id=?`, id)
	task, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Task{}, fault.New("TASK_NOT_FOUND", fmt.Sprintf("task %s not found", id), false)
	}
	if err != nil {
		return model.Task{}, fault.Wrap("DATABASE_FAILED", "cannot read task", false, err)
	}
	return task, nil
}

func (s *Store) LatestTask(ctx context.Context, projectID, sourcePath string) (model.Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM tasks
WHERE project_id=? AND source_path=? ORDER BY created_at DESC LIMIT 1`, projectID, sourcePath)
	task, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Task{}, fault.New("TASK_NOT_FOUND", fmt.Sprintf("no task found for source %s", sourcePath), false)
	}
	if err != nil {
		return model.Task{}, fault.Wrap("DATABASE_FAILED", "cannot resolve latest task", false, err)
	}
	return task, nil
}

func (s *Store) History(ctx context.Context, projectID, sourcePath string) ([]model.Task, error) {
	return s.queryTasks(ctx, `SELECT `+columns+` FROM tasks
WHERE project_id=? AND source_path=? ORDER BY created_at DESC`, projectID, sourcePath)
}

func (s *Store) ListTasks(ctx context.Context, projectID string) ([]model.Task, error) {
	return s.queryTasks(ctx, `SELECT `+columns+` FROM tasks
WHERE project_id=? ORDER BY created_at DESC`, projectID)
}

// WatchFilter limits the cache-only watch view without contacting a cluster.
// Empty fields mean no filter. Attention selects submission/pull failures and
// other states that need user intervention.
type WatchFilter struct {
	ProjectID     string
	Target        string
	State         string
	Attention     bool
	IncludeDryRun bool
}

// ListWatchTasks returns a bounded, cache-only view for the global daemon
// dashboard. It deliberately selects summary columns instead of decoding full
// immutable Task snapshots, so a project with thousands of historical tasks
// does not make watch allocate or scan every input manifest and script.
func (s *Store) ListWatchTasks(ctx context.Context, limit int, filter WatchFilter) ([]model.TaskSummary, int, error) {
	if limit < 1 {
		limit = 100
	}
	where := make([]string, 0, 4)
	args := make([]any, 0, 4)
	if filter.ProjectID != "" {
		where = append(where, "project_id=?")
		args = append(args, filter.ProjectID)
	}
	if filter.Target != "" {
		where = append(where, "target_name=?")
		args = append(args, filter.Target)
	}
	if filter.State != "" {
		where = append(where, "compute_state=?")
		args = append(args, filter.State)
	}
	if !filter.IncludeDryRun {
		// Dry-run records are persisted for auditability, but are not compute
		// work and must not appear in the operational dashboard by default.
		where = append(where, `(metadata NOT LIKE '%"dry_run":"1"%' AND metadata NOT LIKE '%"dry_run":true%')`)
	}
	failure := "(compute_state IN ('submission_failed','submission_uncertain','failed') OR pull_state IN ('failed','partial'))"
	active := "(compute_state IN ('created','queued','running','unknown','submission_uncertain') OR pull_state='pulling')"
	superseded := `NOT EXISTS (
  SELECT 1 FROM tasks newer
  WHERE newer.project_id=tasks.project_id
    AND newer.source_path=tasks.source_path
    AND newer.created_at > tasks.created_at
)`
	if filter.Attention {
		// Attention is an explicit historical failure view, but a failure is
		// superseded as soon as a newer Task is created for the same Source.
		where = append(where, failure)
		where = append(where, superseded)
	} else if filter.State == "" {
		// The default view is squeue-like: active work plus failures that
		// happened recently enough to require attention. Historical terminal
		// records remain available through list/inspect, not this dashboard.
		cutoff := time.Now().UTC().Add(-12 * time.Hour).Format(time.RFC3339Nano)
		where = append(where, "("+active+" OR ("+failure+" AND updated_at>=? AND "+superseded+"))")
		args = append(args, cutoff)
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM tasks"+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, fault.Wrap("DATABASE_FAILED", "cannot count watch tasks", true, err)
	}
	query := `
SELECT id,project_id,source_path,target_name,cluster_name,scheduler_id,
       compute_state,pull_state,scheduler_state,scheduler_reason,elapsed,
       exit_code,scheduler_start,scheduler_end,metadata,created_at,updated_at
FROM tasks` + whereSQL + `
ORDER BY CASE
  WHEN compute_state IN ('submission_failed','submission_uncertain','failed')
    OR pull_state IN ('failed','partial') THEN 0
  WHEN compute_state IN ('created','queued','running','unknown')
    OR pull_state='pulling' THEN 1
  ELSE 2
END, updated_at DESC

LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fault.Wrap("DATABASE_FAILED", "cannot list watch tasks", true, err)
	}
	defer rows.Close()
	result := make([]model.TaskSummary, 0, limit)
	for rows.Next() {
		var task model.TaskSummary
		var metadata, created, updated string
		if err := rows.Scan(&task.ID, &task.ProjectID, &task.SourcePath, &task.TargetName,
			&task.ClusterName, &task.SchedulerID, &task.ComputeState, &task.PullState,
			&task.SchedulerState, &task.SchedulerReason, &task.Elapsed, &task.ExitCode,
			&task.SchedulerStart, &task.SchedulerEnd, &metadata, &created, &updated); err != nil {
			return nil, 0, fault.Wrap("DATABASE_FAILED", "cannot decode watch task", false, err)
		}
		parsedCreated, parseErr := time.Parse(time.RFC3339Nano, created)
		if parseErr != nil {
			return nil, 0, fault.Wrap("DATABASE_FAILED", "cannot decode watch task creation time", false, parseErr)
		} else {
			task.CreatedAt = parsedCreated
		}
		parsedUpdated, parseErr := time.Parse(time.RFC3339Nano, updated)
		if parseErr != nil {
			return nil, 0, fault.Wrap("DATABASE_FAILED", "cannot decode watch task update time", false, parseErr)
		} else {
			task.UpdatedAt = parsedUpdated
		}
		var values map[string]string
		if err := decodeJSON(metadata, &values); err != nil {
			return nil, 0, fault.Wrap("DATABASE_FAILED", "cannot decode watch task metadata", false, err)
		}
		task.DryRun = values[dryRunKey] == "1" || values[dryRunKey] == "true"
		task.SchedulerObservation = values["scheduler_observation"]
		task.SchedulerObservedAt = values["scheduler_observed_at"]
		task.SchedulerStaleSince = values["scheduler_stale_since"]
		result = append(result, task)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fault.Wrap("DATABASE_FAILED", "cannot finish listing watch tasks", true, err)
	}
	return result, total, nil
}

func (s *Store) queryTasks(ctx context.Context, query string, args ...any) ([]model.Task, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fault.Wrap("DATABASE_FAILED", "cannot read tasks", true, err)
	}
	defer rows.Close()
	var result []model.Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, fault.Wrap("DATABASE_FAILED", "cannot decode tasks", false, err)
		}
		result = append(result, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fault.Wrap("DATABASE_FAILED", "cannot finish reading tasks", true, err)
	}
	return result, nil
}

func (s *Store) Events(ctx context.Context, taskID string) ([]model.TaskEvent, error) {
	if _, err := s.GetTask(ctx, taskID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id,task_id,type,stage,message,data,created_at
FROM task_events WHERE task_id=? ORDER BY id`, taskID)
	if err != nil {
		return nil, fault.Wrap("DATABASE_FAILED", "cannot read task events", true, err)
	}
	defer rows.Close()
	var events []model.TaskEvent
	for rows.Next() {
		var event model.TaskEvent
		var data, created string
		if err := rows.Scan(&event.ID, &event.TaskID, &event.Type, &event.Stage,
			&event.Message, &data, &created); err != nil {
			return nil, fault.Wrap("DATABASE_FAILED", "cannot decode task event", false, err)
		}
		if err := decodeJSON(data, &event.Data); err != nil {
			return nil, fault.Wrap("DATABASE_FAILED", "cannot decode task event data", false, err)
		}
		event.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, fault.Wrap("DATABASE_FAILED", "cannot decode task event time", false, err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fault.Wrap("DATABASE_FAILED", "cannot finish reading task events", true, err)
	}
	return events, nil
}

const columns = `id,revision,project_id,source_path,source_workdir,source_entry,target_name,cluster_name,
remote_dir,scheduler_id,compute_state,pull_state,scheduler_state,scheduler_reason,elapsed,exit_code,
scheduler_start,scheduler_end,resolved_params,rendered_script,
target_hash,input_manifest,pull_patterns,push_excludes,logs,metadata,created_at,submitted_at,
updated_at,pulled_at`

type scanner interface {
	Scan(...any) error
}

type sqlExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertTask(ctx context.Context, exec sqlExecutor, task model.Task) error {
	values, err := encodeTask(task)
	if err != nil {
		return err
	}
	_, err = exec.ExecContext(ctx, `
INSERT INTO tasks(
 id,revision,project_id,source_path,source_workdir,source_entry,target_name,cluster_name,remote_dir,
 scheduler_id,compute_state,pull_state,scheduler_state,scheduler_reason,elapsed,exit_code,
 scheduler_start,scheduler_end,resolved_params,rendered_script,target_hash,
 input_manifest,pull_patterns,push_excludes,logs,metadata,created_at,submitted_at,updated_at,
 pulled_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, values...)
	if err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot create task record", true, err)
	}
	return nil
}

func insertEvent(ctx context.Context, exec sqlExecutor, event model.TaskEvent) error {
	data, err := json.Marshal(event.Data)
	if err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot encode task event", false, err)
	}
	_, err = exec.ExecContext(ctx, `
INSERT INTO task_events(task_id,type,stage,message,data,created_at) VALUES(?,?,?,?,?,?)`,
		event.TaskID, event.Type, event.Stage, event.Message, string(data),
		event.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot append task event", true, err)
	}
	return nil
}

func scanTask(row scanner) (model.Task, error) {
	var task model.Task
	var entry, submitted, pulled sql.NullString
	var params, manifest, pull, push, logs, metadata string
	var created, updated string
	err := row.Scan(&task.ID, &task.Revision, &task.ProjectID, &task.SourcePath, &task.SourceWorkDir, &entry,
		&task.TargetName, &task.ClusterName, &task.RemoteDir, &task.SchedulerID,
		&task.ComputeState, &task.PullState, &task.SchedulerState, &task.SchedulerReason,
		&task.Elapsed, &task.ExitCode, &task.SchedulerStart, &task.SchedulerEnd, &params,
		&task.RenderedScript, &task.TargetHash, &manifest, &pull, &push, &logs,
		&metadata, &created, &submitted, &updated, &pulled)
	if err != nil {
		return task, err
	}
	if entry.Valid {
		task.SourceEntry = &entry.String
	}
	if err := decodeJSON(params, &task.ResolvedParams); err != nil {
		return task, err
	}
	if err := decodeJSON(manifest, &task.InputManifest); err != nil {
		return task, err
	}
	if err := decodeJSON(pull, &task.PullPatterns); err != nil {
		return task, err
	}
	if err := decodeJSON(push, &task.PushExcludes); err != nil {
		return task, err
	}
	if err := decodeJSON(logs, &task.Logs); err != nil {
		return task, err
	}
	if err := decodeJSON(metadata, &task.Metadata); err != nil {
		return task, err
	}
	task.DryRun = task.Metadata[dryRunKey] == "1" || task.Metadata[dryRunKey] == "true"
	task.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return task, err
	}
	task.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return task, err
	}
	if submitted.Valid {
		value, err := time.Parse(time.RFC3339Nano, submitted.String)
		if err != nil {
			return task, err
		}
		task.SubmittedAt = &value
	}
	if pulled.Valid {
		value, err := time.Parse(time.RFC3339Nano, pulled.String)
		if err != nil {
			return task, err
		}
		task.PulledAt = &value
	}
	return task, nil
}

func encodeTask(task model.Task) ([]any, error) {
	params, err := json.Marshal(task.ResolvedParams)
	if err != nil {
		return nil, err
	}
	manifest, _ := json.Marshal(task.InputManifest)
	pull, _ := json.Marshal(task.PullPatterns)
	push, _ := json.Marshal(task.PushExcludes)
	logs, _ := json.Marshal(task.Logs)
	metadataValues := make(map[string]string, len(task.Metadata)+1)
	for key, value := range task.Metadata {
		metadataValues[key] = value
	}
	if task.DryRun {
		metadataValues[dryRunKey] = "1"
	} else {
		delete(metadataValues, dryRunKey)
	}
	metadata, _ := json.Marshal(metadataValues)
	var entry, submitted, pulled any
	if task.SourceEntry != nil {
		entry = *task.SourceEntry
	}
	if task.SubmittedAt != nil {
		submitted = task.SubmittedAt.UTC().Format(time.RFC3339Nano)
	}
	if task.PulledAt != nil {
		pulled = task.PulledAt.UTC().Format(time.RFC3339Nano)
	}
	return []any{
		task.ID, task.Revision, task.ProjectID, task.SourcePath, task.SourceWorkDir, entry, task.TargetName,
		task.ClusterName, task.RemoteDir, task.SchedulerID, task.ComputeState, task.PullState,
		task.SchedulerState, task.SchedulerReason, task.Elapsed, task.ExitCode,
		task.SchedulerStart, task.SchedulerEnd,
		string(params), task.RenderedScript, task.TargetHash, string(manifest),
		string(pull), string(push), string(logs), string(metadata),
		task.CreatedAt.UTC().Format(time.RFC3339Nano), submitted,
		task.UpdatedAt.UTC().Format(time.RFC3339Nano), pulled,
	}, nil
}

func decodeJSON(raw string, target any) error {
	if raw == "" {
		raw = "null"
	}
	return json.Unmarshal([]byte(raw), target)
}
