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
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;
PRAGMA user_version=1;
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
  state TEXT NOT NULL,
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
  results_pulled_at TEXT,
  FOREIGN KEY(project_id) REFERENCES projects(id)
);
CREATE INDEX IF NOT EXISTS idx_tasks_source ON tasks(project_id, source_path, created_at DESC);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot initialize task database", false, err)
	}
	return nil
}

func (s *Store) BindProject(ctx context.Context, project model.Project) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO projects(id,last_path,updated_at) VALUES(?,?,?)
ON CONFLICT(id) DO UPDATE SET last_path=excluded.last_path,updated_at=excluded.updated_at`,
		project.ProjectID, project.Root, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot bind project path", false, err)
	}
	return nil
}

func (s *Store) CreateTask(ctx context.Context, task model.Task) error {
	values, err := encodeTask(task)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO tasks(
 id,project_id,source_path,source_workdir,source_entry,target_name,cluster_name,remote_dir,
 scheduler_id,state,scheduler_state,resolved_params,rendered_script,target_hash,input_manifest,
 pull_patterns,push_excludes,logs,metadata,created_at,submitted_at,updated_at,results_pulled_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, values...)
	if err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot create task record", false, err)
	}
	return nil
}

func (s *Store) ImportTask(ctx context.Context, task model.Task) error {
	if _, err := s.GetTask(ctx, task.ID); err == nil {
		return fault.New("TASK_ALREADY_EXISTS", fmt.Sprintf("task %s already exists", task.ID), false)
	} else if fault.As(err).Code != "TASK_NOT_FOUND" {
		return err
	}
	return s.CreateTask(ctx, task)
}

func (s *Store) UpdateTask(ctx context.Context, task model.Task) error {
	values, err := encodeTask(task)
	if err != nil {
		return err
	}
	values = append(values[1:], task.ID)
	result, err := s.db.ExecContext(ctx, `
UPDATE tasks SET
 project_id=?,source_path=?,source_workdir=?,source_entry=?,target_name=?,cluster_name=?,remote_dir=?,
 scheduler_id=?,state=?,scheduler_state=?,resolved_params=?,rendered_script=?,target_hash=?,input_manifest=?,
 pull_patterns=?,push_excludes=?,logs=?,metadata=?,created_at=?,submitted_at=?,updated_at=?,results_pulled_at=?
WHERE id=?`, values...)
	if err != nil {
		return fault.Wrap("DATABASE_FAILED", "cannot update task record", false, err)
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return fault.New("TASK_NOT_FOUND", fmt.Sprintf("task %s not found", task.ID), false)
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
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM tasks
WHERE project_id=? AND source_path=? ORDER BY created_at DESC`, projectID, sourcePath)
	if err != nil {
		return nil, fault.Wrap("DATABASE_FAILED", "cannot read task history", false, err)
	}
	defer rows.Close()
	var result []model.Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, fault.Wrap("DATABASE_FAILED", "cannot decode task history", false, err)
		}
		result = append(result, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fault.Wrap("DATABASE_FAILED", "cannot finish reading task history", false, err)
	}
	return result, nil
}

const columns = `id,project_id,source_path,source_workdir,source_entry,target_name,cluster_name,
remote_dir,scheduler_id,state,scheduler_state,resolved_params,rendered_script,target_hash,input_manifest,
pull_patterns,push_excludes,logs,metadata,created_at,submitted_at,updated_at,results_pulled_at`

type scanner interface {
	Scan(...any) error
}

func scanTask(row scanner) (model.Task, error) {
	var task model.Task
	var entry, submitted, pulled sql.NullString
	var params, manifest, pull, push, logs, metadata string
	var created, updated string
	err := row.Scan(&task.ID, &task.ProjectID, &task.SourcePath, &task.SourceWorkDir, &entry,
		&task.TargetName, &task.ClusterName, &task.RemoteDir, &task.SchedulerID, &task.State,
		&task.SchedulerState, &params, &task.RenderedScript, &task.TargetHash, &manifest,
		&pull, &push, &logs, &metadata, &created, &submitted, &updated, &pulled)
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
		task.ResultsPulledAt = &value
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
	if task.ResultsPulledAt != nil {
		pulled = task.ResultsPulledAt.UTC().Format(time.RFC3339Nano)
	}
	return []any{
		task.ID, task.ProjectID, task.SourcePath, task.SourceWorkDir, entry, task.TargetName,
		task.ClusterName, task.RemoteDir, task.SchedulerID, task.State, task.SchedulerState,
		string(params), task.RenderedScript, task.TargetHash, string(manifest), string(pull),
		string(push), string(logs), string(metadata), task.CreatedAt.UTC().Format(time.RFC3339Nano),
		submitted, task.UpdatedAt.UTC().Format(time.RFC3339Nano), pulled,
	}, nil
}

func decodeJSON(raw string, target any) error {
	if raw == "" {
		raw = "null"
	}
	return json.Unmarshal([]byte(raw), target)
}
