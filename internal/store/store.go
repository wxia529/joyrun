package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/model"
	_ "modernc.org/sqlite"
)

const (
	schemaVersion = 1
	schemaChannel = "development"
	schemaLabel   = "dev-1"
)

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
		_, err = conn.ExecContext(ctx, developmentSchema)
	case schemaVersion:
		var metadataTable int
		if err = conn.QueryRowContext(ctx, `
SELECT count(*) FROM sqlite_master
WHERE type='table' AND name='joyrun_meta'`).Scan(&metadataTable); err != nil {
			return fault.Wrap("DATABASE_FAILED", "cannot inspect database metadata", false, err)
		}
		if metadataTable == 0 {
			return fault.New("DATABASE_UNSUPPORTED",
				"database schema is not marked as a JoyRun development database; remove it or set JOYRUN_DB to a new path", false)
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

const developmentSchema = `
CREATE TABLE IF NOT EXISTS joyrun_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
INSERT OR IGNORE INTO joyrun_meta(key,value) VALUES
  ('release_channel','development'),
  ('schema_label','dev-1');
CREATE TABLE IF NOT EXISTS projects (
  id TEXT PRIMARY KEY,
  last_path TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY,
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

func (s *Store) CreateTask(ctx context.Context, task model.Task) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fault.Wrap("DATABASE_BUSY", "cannot begin task transaction", true, err)
	}
	defer tx.Rollback()
	if err := insertTask(ctx, tx, task); err != nil {
		return err
	}
	event := model.TaskEvent{
		TaskID: task.ID, Type: "TASK_CREATED", Stage: "task",
		Message: "Task record created", CreatedAt: task.CreatedAt,
	}
	if err := insertEvent(ctx, tx, event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot commit task creation", true, err)
	}
	return nil
}

func (s *Store) ImportTask(ctx context.Context, task model.Task) error {
	if _, err := s.GetTask(ctx, task.ID); err == nil {
		return fault.New("TASK_ALREADY_EXISTS", fmt.Sprintf("task %s already exists", task.ID), false)
	} else if fault.As(err).Code != "TASK_NOT_FOUND" {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fault.Wrap("DATABASE_BUSY", "cannot begin task import", true, err)
	}
	defer tx.Rollback()
	if err := insertTask(ctx, tx, task); err != nil {
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

func (s *Store) UpdateTask(ctx context.Context, task model.Task) error {
	return s.updateTask(ctx, task, nil)
}

func (s *Store) UpdateTaskWithEvent(ctx context.Context, task model.Task, event model.TaskEvent) error {
	if event.TaskID == "" {
		event.TaskID = task.ID
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	return s.updateTask(ctx, task, &event)
}

func (s *Store) updateTask(ctx context.Context, task model.Task, event *model.TaskEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fault.Wrap("DATABASE_BUSY", "cannot begin task update", true, err)
	}
	defer tx.Rollback()
	values, err := encodeTask(task)
	if err != nil {
		return err
	}
	values = append(values[1:], task.ID)
	result, err := tx.ExecContext(ctx, `
UPDATE tasks SET
 project_id=?,source_path=?,source_workdir=?,source_entry=?,target_name=?,cluster_name=?,remote_dir=?,
 scheduler_id=?,compute_state=?,pull_state=?,scheduler_state=?,resolved_params=?,rendered_script=?,
 target_hash=?,input_manifest=?,pull_patterns=?,push_excludes=?,logs=?,metadata=?,created_at=?,
 submitted_at=?,updated_at=?,pulled_at=?
WHERE id=?`, values...)
	if err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot update task record", true, err)
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return fault.New("TASK_NOT_FOUND", fmt.Sprintf("task %s not found", task.ID), false)
	}
	if event != nil {
		if err := insertEvent(ctx, tx, *event); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot commit task update", true, err)
	}
	return nil
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

const columns = `id,project_id,source_path,source_workdir,source_entry,target_name,cluster_name,
remote_dir,scheduler_id,compute_state,pull_state,scheduler_state,resolved_params,rendered_script,
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
 id,project_id,source_path,source_workdir,source_entry,target_name,cluster_name,remote_dir,
 scheduler_id,compute_state,pull_state,scheduler_state,resolved_params,rendered_script,target_hash,
 input_manifest,pull_patterns,push_excludes,logs,metadata,created_at,submitted_at,updated_at,
 pulled_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, values...)
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
	err := row.Scan(&task.ID, &task.ProjectID, &task.SourcePath, &task.SourceWorkDir, &entry,
		&task.TargetName, &task.ClusterName, &task.RemoteDir, &task.SchedulerID,
		&task.ComputeState, &task.PullState, &task.SchedulerState, &params,
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
	metadata, _ := json.Marshal(task.Metadata)
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
		task.ID, task.ProjectID, task.SourcePath, task.SourceWorkDir, entry, task.TargetName,
		task.ClusterName, task.RemoteDir, task.SchedulerID, task.ComputeState, task.PullState,
		task.SchedulerState, string(params), task.RenderedScript, task.TargetHash, string(manifest),
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
