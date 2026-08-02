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
	bound, err := s.GetProject(ctx, project.ProjectID)
	if err != nil || bound.Root != "/new/location" {
		t.Fatalf("project binding lookup = %#v, err=%v", bound, err)
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

func TestFreshDatabaseIsMarkedStable(t *testing.T) {
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

func TestListWatchTasksIsBoundedFilteredAndOrderedByAttention(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	project := model.Project{ProjectID: "pj_watch", Root: t.TempDir()}
	if err := s.BindProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	makeTask := func(id, target, compute, pull string, updated time.Time) model.Task {
		entry := "input.inp"
		return model.Task{
			ID: id, ProjectID: project.ProjectID, SourcePath: id + "/input.inp", SourceWorkDir: id,
			SourceEntry: &entry, TargetName: target, ClusterName: "cluster", RemoteDir: "/remote/" + id,
			ComputeState: compute, PullState: pull, ResolvedParams: map[string]any{},
			RenderedScript: "echo test", InputManifest: []model.ManifestEntry{{Path: entry}},
			CreatedAt: updated.Add(-time.Minute), UpdatedAt: updated, Metadata: map[string]string{},
		}
	}
	for _, task := range []model.Task{
		makeTask("jr_attention", "target/a", model.ComputeFailed, model.PullNotPulled, now),
		makeTask("jr_running", "target/a", model.ComputeRunning, model.PullNotPulled, now.Add(-time.Second)),
		makeTask("jr_done", "target/b", model.ComputeCompleted, model.PullSucceeded, now.Add(-2*time.Second)),
	} {
		if err := s.CreateTask(ctx, &task); err != nil {
			t.Fatal(err)
		}
	}
	rows, total, err := s.ListWatchTasks(ctx, 2, WatchFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(rows) != 2 || rows[0].ID != "jr_attention" || rows[1].ID != "jr_running" {
		t.Fatalf("unexpected watch rows: total=%d rows=%#v", total, rows)
	}
	rows, total, err = s.ListWatchTasks(ctx, 100, WatchFilter{Target: "target/a", Attention: true})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(rows) != 1 || rows[0].ID != "jr_attention" {
		t.Fatalf("unexpected filtered watch rows: total=%d rows=%#v", total, rows)
	}
}

func TestListWatchTasksHidesOldAndSupersededFailures(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	project := model.Project{ProjectID: "pj_watch_retention", Root: t.TempDir()}
	if err := s.BindProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	makeTask := func(id, source, state string, created, updated time.Time) model.Task {
		entry := "input.inp"
		return model.Task{
			ID: id, ProjectID: project.ProjectID, SourcePath: source, SourceWorkDir: filepath.Dir(source),
			SourceEntry: &entry, TargetName: "target/a", ClusterName: "cluster", RemoteDir: "/remote/" + id,
			ComputeState: state, PullState: model.PullNotPulled, ResolvedParams: map[string]any{},
			RenderedScript: "echo test", InputManifest: []model.ManifestEntry{{Path: entry}},
			CreatedAt: created, UpdatedAt: updated, Metadata: map[string]string{},
		}
	}
	tasks := []model.Task{
		makeTask("jr_recent_failed", "recent/input.inp", model.ComputeFailed, now.Add(-time.Hour), now.Add(-time.Hour)),
		makeTask("jr_old_failed", "old/input.inp", model.ComputeFailed, now.Add(-13*time.Hour), now.Add(-13*time.Hour)),
		makeTask("jr_previous_failed", "retry/input.inp", model.ComputeFailed, now.Add(-2*time.Hour), now.Add(-2*time.Hour)),
		makeTask("jr_replacement", "retry/input.inp", model.ComputeRunning, now.Add(-time.Minute), now),
		makeTask("jr_completed", "done/input.inp", model.ComputeCompleted, now, now),
	}
	for i := range tasks {
		if err := s.CreateTask(ctx, &tasks[i]); err != nil {
			t.Fatal(err)
		}
	}
	rows, total, err := s.ListWatchTasks(ctx, 100, WatchFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(rows) != 2 || rows[0].ID != "jr_recent_failed" || rows[1].ID != "jr_replacement" {
		t.Fatalf("unexpected default watch rows: total=%d rows=%#v", total, rows)
	}
	rows, total, err = s.ListWatchTasks(ctx, 100, WatchFilter{Attention: true})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(rows) != 2 || rows[0].ID != "jr_recent_failed" || rows[1].ID != "jr_old_failed" {
		t.Fatalf("unexpected attention rows: total=%d rows=%#v", total, rows)
	}
	rows, total, err = s.ListWatchTasks(ctx, 100, WatchFilter{State: model.ComputeFailed})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(rows) != 3 {
		t.Fatalf("explicit state filter should expose history: total=%d rows=%#v", total, rows)
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

func TestRejectsUnsupportedDatabaseSchema(t *testing.T) {
	for _, version := range []int{2, 3, 4} {
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
PRAGMA user_version=1;
`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(database); fault.As(err).Code != "DATABASE_UNSUPPORTED" {
		t.Fatalf("expected unmarked version 1 database to be rejected, got %v", err)
	}
}

func TestRejectsNonStableDatabaseMarker(t *testing.T) {
	database := filepath.Join(t.TempDir(), "joyrun.db")
	s, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		"UPDATE joyrun_meta SET value='development' WHERE key='release_channel'"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(database); fault.As(err).Code != "DATABASE_UNSUPPORTED" {
		t.Fatalf("expected non-stable database to be rejected, got %v", err)
	}
}

func TestCreateTasksIsAtomic(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := model.Project{ProjectID: "pj_batch", Root: t.TempDir()}
	if err := s.BindProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first := &model.Task{
		ID: "jr_duplicate", ProjectID: p.ProjectID, SourcePath: "one",
		SourceWorkDir: ".", TargetName: "c/run", ClusterName: "c",
		RemoteDir: "/tmp/jr_duplicate", ComputeState: model.ComputeCreated,
		PullState: model.PullNotPulled, CreatedAt: now, UpdatedAt: now,
	}
	second := *first
	second.SourcePath = "two"
	if err := s.CreateTasks(ctx, []*model.Task{first, &second}); err == nil {
		t.Fatal("expected duplicate task ID to fail the batch")
	}
	if _, err := s.GetTask(ctx, first.ID); fault.As(err).Code != "TASK_NOT_FOUND" {
		t.Fatalf("partial batch task was committed: %v", err)
	}
}

func TestUpdateTasksWithEventsIsAtomic(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := model.Project{ProjectID: "pj_batch_update", Root: t.TempDir()}
	if err := s.BindProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	tasks := []*model.Task{
		{
			ID: "jr_batch_update_one", ProjectID: p.ProjectID, SourcePath: "one",
			SourceWorkDir: ".", TargetName: "c/run", ClusterName: "c",
			RemoteDir: "/tmp/jr_batch_update_one", ComputeState: model.ComputeCompleted,
			PullState: model.PullNotPulled, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "jr_batch_update_two", ProjectID: p.ProjectID, SourcePath: "two",
			SourceWorkDir: ".", TargetName: "c/run", ClusterName: "c",
			RemoteDir: "/tmp/jr_batch_update_two", ComputeState: model.ComputeCompleted,
			PullState: model.PullNotPulled, CreatedAt: now, UpdatedAt: now,
		},
	}
	if err := s.CreateTasks(ctx, tasks); err != nil {
		t.Fatal(err)
	}
	tasks[0].PullState = model.PullInProgress
	tasks[1].PullState = model.PullInProgress
	tasks[1].Revision--
	events := []model.TaskEvent{
		{Type: "PULL_STARTED", Stage: "pull"},
		{Type: "PULL_STARTED", Stage: "pull"},
	}
	if err := s.UpdateTasksWithEvents(ctx, tasks, events); fault.As(err).Code != "DATABASE_CONFLICT" {
		t.Fatalf("expected atomic batch conflict, got %v", err)
	}
	for _, task := range tasks {
		stored, err := s.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.PullState != model.PullNotPulled || stored.Revision != 1 {
			t.Fatalf("partial batch update was committed: %#v", stored)
		}
	}
}
