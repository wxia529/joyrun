package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

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
		ClusterName: "gibbs", RemoteDir: "/scratch/jr_test", State: model.StateCreated,
		ResolvedParams: map[string]any{"cpus": float64(32)}, RenderedScript: "echo test",
		InputManifest: []model.ManifestEntry{{Path: "eg.inp", SHA256: "abc"}},
		CreatedAt:     now, UpdatedAt: now,
	}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestTask(ctx, project.ProjectID, task.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != task.ID || got.SourceEntry == nil || *got.SourceEntry != entry {
		t.Fatalf("unexpected task: %#v", got)
	}
}
