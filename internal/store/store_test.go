package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/model"
)

func TestProjectRebindAndTaskRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	project := model.Project{ProjectID: "pj_test", Root: "/old/location"}
	if err := s.BindProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	project.Root = "/new/location"
	if err := s.BindProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	entry := "eg.inp"
	task := model.Task{
		ID: "jr_test", ProjectID: project.ProjectID, SourcePath: "task01/eg.inp",
		SourceWorkDir: "task01", SourceEntry: &entry, TargetName: "gibbs/orca",
		ClusterName: "gibbs", RemoteDir: "/scratch/jr_test",
		ComputeState: model.ComputeCreated, PullState: model.PullNotPulled,
		SchedulerState: "PENDING", SchedulerReason: "Resources",
		Elapsed: "00:00:03", ExitCode: "0:0",
		SchedulerStart: "2026-07-28T10:00:00",
		ResolvedParams: map[string]any{"cpus": float64(32)}, RenderedScript: "echo test",
		InputManifest: []model.ManifestEntry{{Path: "eg.inp", SHA256: "abc"}},
		CreatedAt:     now, UpdatedAt: now,
	}
	if err := s.CreateTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestTask(ctx, project.ProjectID, task.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != task.ID || got.SourceEntry == nil || *got.SourceEntry != entry ||
		got.SchedulerReason != "Resources" || got.Elapsed != "00:00:03" ||
		got.ExitCode != "0:0" || got.SchedulerStart != "2026-07-28T10:00:00" {
		t.Fatalf("unexpected task: %#v", got)
	}
	events, err := s.Events(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "TASK_CREATED" {
		t.Fatalf("unexpected initial events: %#v", events)
	}
	task.ComputeState = model.ComputeRunning
	task.UpdatedAt = now.Add(time.Second)
	if err := s.UpdateTaskWithEvent(ctx, &task, model.TaskEvent{
		TaskID: task.ID, Type: "COMPUTE_STATE_CHANGED", Stage: "status",
		Data:      map[string]string{"compute_state": model.ComputeRunning},
		CreatedAt: task.UpdatedAt,
	}); err != nil {
		t.Fatal(err)
	}
	events, err = s.Events(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Data["compute_state"] != model.ComputeRunning {
		t.Fatalf("task update and event were not persisted together: %#v", events)
	}
	tasks, err := s.ListTasks(ctx, project.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ComputeState != model.ComputeRunning {
		t.Fatalf("unexpected task list: %#v", tasks)
	}
}

func TestFreshDatabaseIsMarkedDevelopment(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}

	values := map[string]string{}
	rows, err := s.db.Query("SELECT key,value FROM joyrun_meta")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			t.Fatal(err)
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if values["release_channel"] != schemaChannel || values["schema_label"] != schemaLabel {
		t.Fatalf("unexpected database metadata: %#v", values)
	}
}

func TestTwoStoreHandlesCanWriteSameDatabase(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "joyrun.db")
	first, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	var wait sync.WaitGroup
	errs := make(chan error, 40)
	for worker, handle := range []*Store{first, second} {
		wait.Add(1)
		go func(worker int, handle *Store) {
			defer wait.Done()
			for index := 0; index < 20; index++ {
				errs <- handle.BindProject(ctx, model.Project{
					ProjectID: "pj_concurrent",
					Root:      filepath.Join("/tmp", "joyrun", string(rune('a'+worker))),
				})
			}
		}(worker, handle)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent database write failed: %v", err)
		}
	}
}

func TestTaskRevisionRejectsStaleUpdate(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	project := model.Project{ProjectID: "pj_revision", Root: t.TempDir()}
	if err := s.BindProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := model.Task{
		ID: "jr_revision", ProjectID: project.ProjectID, SourcePath: ".",
		TargetName: "c/run", ClusterName: "c", RemoteDir: "/tmp/jr_revision",
		ComputeState: model.ComputeRunning, PullState: model.PullNotPulled,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	stale := task
	task.ComputeState = model.ComputeCompleted
	if err := s.UpdateTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	stale.PullState = model.PullPartial
	if err := s.UpdateTask(ctx, &stale); fault.As(err).Code != "DATABASE_CONFLICT" {
		t.Fatalf("expected stale update conflict, got %v", err)
	}
	stored, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ComputeState != model.ComputeCompleted || stored.PullState != model.PullNotPulled {
		t.Fatalf("stale update overwrote current state: %#v", stored)
	}
}

func TestRejectsOldDatabaseSchema(t *testing.T) {
	for _, version := range []int{1, 2, 4} {
		t.Run("version_"+strconv.Itoa(version), func(t *testing.T) {
			database := filepath.Join(t.TempDir(), "joyrun.db")
			legacy, err := sql.Open("sqlite", database)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := legacy.Exec("PRAGMA user_version=" + strconv.Itoa(version)); err != nil {
				t.Fatal(err)
			}
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(database); fault.As(err).Code != "DATABASE_UNSUPPORTED" {
				t.Fatalf("expected schema version %d to be rejected, got %v", version, err)
			}
		})
	}
}

func TestRejectsUnmarkedDatabaseUsingCurrentVersionNumber(t *testing.T) {
	database := filepath.Join(t.TempDir(), "joyrun.db")
	legacy, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
CREATE TABLE tasks (id TEXT PRIMARY KEY);
PRAGMA user_version=3;
`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(database); fault.As(err).Code != "DATABASE_UNSUPPORTED" {
		t.Fatalf("expected unmarked version 3 database to be rejected, got %v", err)
	}
}

func TestRejectsNonDevelopmentDatabaseMarker(t *testing.T) {
	database := filepath.Join(t.TempDir(), "joyrun.db")
	s, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		"UPDATE joyrun_meta SET value='stable' WHERE key='release_channel'"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(database); fault.As(err).Code != "DATABASE_UNSUPPORTED" {
		t.Fatalf("expected non-development database to be rejected, got %v", err)
	}
}
