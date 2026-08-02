package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/model"
)

func TestExplicitStable2Upgrade(t *testing.T) {
	database := filepath.Join(t.TempDir(), "joyrun.db")
	s, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("UPDATE joyrun_meta SET value='stable-1' WHERE key='schema_label'"); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(database); fault.As(err).Code != "DATABASE_UPGRADE_REQUIRED" {
		t.Fatalf("expected upgrade requirement, got %v", err)
	}
	backup, err := UpgradeStable2(context.Background(), database, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	upgraded, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	_ = upgraded.Close()
}

func TestOperationClaimAndRestartLease(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.BindProject(ctx, model.Project{ProjectID: "pj_ops", Root: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	op := &model.Operation{ID: "jo_test", Kind: "submit", ProjectID: "pj_ops", Payload: `{"args":[]}`, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := s.ClaimNextOperation(ctx, "worker-a", time.Second)
	if err != nil || !ok {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	if claimed.State != model.OperationRunning || claimed.Attempt != 1 {
		t.Fatalf("unexpected claim: %#v", claimed)
	}
	expired := time.Now().Add(-time.Second)
	claimed.LeaseExpiresAt = &expired
	if err := s.UpdateOperation(ctx, &claimed); err != nil {
		t.Fatal(err)
	}
	reclaimed, ok, err := s.ClaimNextOperation(ctx, "worker-b", time.Second)
	if err != nil || !ok {
		t.Fatalf("reclaim = %#v, %v", reclaimed, err)
	}
	if reclaimed.LeaseOwner != "worker-b" || reclaimed.Attempt != 2 {
		t.Fatalf("unexpected reclaim: %#v", reclaimed)
	}
}

func TestRenewOperationLeaseDoesNotOverwriteOperationFields(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.BindProject(ctx, model.Project{ProjectID: "pj_ops_lease", Root: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	op := &model.Operation{ID: "jo_lease_fields", Kind: "submit", ProjectID: "pj_ops_lease", Payload: `{"args":[]}`, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := s.ClaimNextOperation(ctx, "worker-a", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	claimed.Stage = "executing"
	claimed.Result = `{"progress":"uploaded"}`
	if err := s.UpdateOperation(ctx, &claimed); err != nil {
		t.Fatal(err)
	}
	if err := s.RenewOperationLease(ctx, claimed.ID, claimed.LeaseOwner, time.Now().UTC().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	updated, err := s.GetOperation(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Stage != "executing" || updated.Result != `{"progress":"uploaded"}` {
		t.Fatalf("lease renewal overwrote operation fields: %#v", updated)
	}
	if updated.LeaseOwner != "worker-a" || updated.LeaseExpiresAt == nil {
		t.Fatalf("lease was not renewed: %#v", updated)
	}
}

func TestRequeueOwnedOperationCannotClobberReplacementWorker(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.BindProject(ctx, model.Project{ProjectID: "pj_ops_requeue", Root: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	op := &model.Operation{ID: "jo_requeue", Kind: "submit", ProjectID: "pj_ops_requeue", Payload: "{}", CreatedAt: now}
	if err := s.CreateOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := s.ClaimNextOperation(ctx, "worker-a", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	claimed.LeaseExpiresAt = func() *time.Time { value := time.Now().UTC().Add(-time.Second); return &value }()
	if err := s.UpdateOperation(ctx, &claimed); err != nil {
		t.Fatal(err)
	}
	reclaimed, ok, err := s.ClaimNextOperation(ctx, "worker-b", time.Minute)
	if err != nil || !ok {
		t.Fatalf("reclaim = %#v, %v", reclaimed, err)
	}
	if err := s.RequeueOwnedOperation(ctx, claimed.ID, "worker-a"); fault.As(err).Code != "OPERATION_LEASE_LOST" {
		t.Fatalf("stale requeue error = %v", err)
	}
	current, err := s.GetOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != model.OperationRunning || current.LeaseOwner != "worker-b" {
		t.Fatalf("stale worker clobbered replacement claim: %#v", current)
	}
}

func TestUpdateOperationOwnedCannotClobberReplacementWorker(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.BindProject(ctx, model.Project{ProjectID: "pj_ops_owned", Root: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	op := &model.Operation{ID: "jo_owned", Kind: "submit", ProjectID: "pj_ops_owned", Payload: "{}"}
	if err := s.CreateOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := s.ClaimNextOperation(ctx, "worker-a", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	expired := time.Now().UTC().Add(-time.Second)
	claimed.LeaseExpiresAt = &expired
	if err := s.UpdateOperation(ctx, &claimed); err != nil {
		t.Fatal(err)
	}
	reclaimed, ok, err := s.ClaimNextOperation(ctx, "worker-b", time.Minute)
	if err != nil || !ok {
		t.Fatalf("reclaim = %#v, %v", reclaimed, err)
	}
	claimed.Stage = "stale-finished"
	if err := s.UpdateOperationOwned(ctx, &claimed, "worker-a"); fault.As(err).Code != "OPERATION_LEASE_LOST" {
		t.Fatalf("stale update error = %v", err)
	}
	current, err := s.GetOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Stage == "stale-finished" || current.LeaseOwner != "worker-b" {
		t.Fatalf("stale worker clobbered replacement state: %#v", current)
	}
}

func TestClaimNextOperationExceptSkipsBusyCluster(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.BindProject(ctx, model.Project{ProjectID: "pj_ops_queue", Root: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, op := range []*model.Operation{
		{ID: "jo_busy", Kind: "submit", ProjectID: "pj_ops_queue", ClusterKey: "cluster-a", Payload: "{}", CreatedAt: now},
		{ID: "jo_free", Kind: "submit", ProjectID: "pj_ops_queue", ClusterKey: "cluster-b", Payload: "{}", CreatedAt: now.Add(time.Nanosecond)},
	} {
		if err := s.CreateOperation(ctx, op); err != nil {
			t.Fatal(err)
		}
	}
	claimed, ok, err := s.ClaimNextOperationExcept(ctx, "worker", time.Minute, []string{"cluster-a"})
	if err != nil || !ok {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	if claimed.ID != "jo_free" {
		t.Fatalf("claimed %s, want jo_free", claimed.ID)
	}
}

func TestOperationDeduplication(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.BindProject(ctx, model.Project{ProjectID: "pj_ops_dedup", Root: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	first := &model.Operation{ID: "jo_first", Kind: "pull", ProjectID: "pj_ops_dedup", DedupKey: "task:one", Payload: "{}"}
	if err := s.CreateOperation(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := &model.Operation{ID: "jo_second", Kind: "pull", ProjectID: "pj_ops_dedup", DedupKey: "task:one", Payload: "{}"}
	if fault.As(s.CreateOperation(ctx, second)).Code != "OPERATION_DUPLICATE" {
		t.Fatal("expected duplicate operation rejection")
	}
}

func TestOperationTasksCanBeReplacedAndListed(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.BindProject(ctx, model.Project{ProjectID: "pj_ops_tasks", Root: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	op := &model.Operation{ID: "jo_tasks", Kind: "submit", ProjectID: "pj_ops_tasks", Payload: "{}", CreatedAt: now}
	if err := s.CreateOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	for _, taskID := range []string{"jr_one", "jr_two"} {
		task := &model.Task{ID: taskID, ProjectID: "pj_ops_tasks", SourcePath: taskID + ".inp",
			SourceWorkDir: ".", TargetName: "target", ClusterName: "cluster", RemoteDir: "/tmp/" + taskID,
			ComputeState: model.ComputeCreated, PullState: model.PullNotPulled, CreatedAt: now, UpdatedAt: now}
		if err := s.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ReplaceOperationTasks(ctx, op.ID, []model.OperationTask{{TaskID: "jr_one", Ordinal: 0, State: "queued"}, {TaskID: "jr_two", Ordinal: 1, State: "running"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceOperationTasks(ctx, op.ID, []model.OperationTask{{TaskID: "jr_two", Ordinal: 0, State: "completed"}}); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.OperationTasks(ctx, op.ID)
	if err != nil || len(tasks) != 1 || tasks[0].TaskID != "jr_two" || tasks[0].State != "completed" {
		t.Fatalf("unexpected operation tasks: %#v, %v", tasks, err)
	}
}
