package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wxia529/joyrun/internal/model"
	"github.com/wxia529/joyrun/internal/project"
	"github.com/wxia529/joyrun/internal/store"
)

type fakeRunner struct {
	state              string
	metadata           []byte
	metadataWriteCount int
	failMetadataWrite  int
	schedulerID        string
}

func (f *fakeRunner) Exec(_ context.Context, _, command string, stdin io.Reader) (string, string, error) {
	switch {
	case strings.Contains(command, "sbatch --parsable"):
		f.schedulerID = "12345"
		return "12345\n", "", nil
	case strings.Contains(command, "squeue -h"):
		return f.state, "", nil
	case strings.Contains(command, "find . -type f"):
		return "eg.inp\x00eg.out\x00eg.gbw\x00scratch.tmp\x00joyrun-job.sh\x00", "", nil
	case strings.Contains(command, "tail -n"):
		return "calculation output\n", "", nil
	case strings.Contains(command, "cat >") && strings.Contains(command, "metadata.json"):
		f.metadataWriteCount++
		if f.metadataWriteCount == f.failMetadataWrite {
			return "", "simulated metadata failure", errors.New("metadata write failed")
		}
		f.metadata, _ = io.ReadAll(stdin)
		return "", "", nil
	case strings.HasPrefix(command, "cat ") && strings.Contains(command, "metadata.json"):
		return string(f.metadata), "", nil
	case strings.HasPrefix(command, "cat ") && strings.Contains(command, "scheduler_id"):
		if f.schedulerID == "" {
			return "", "not found", errors.New("not found")
		}
		return f.schedulerID + "\n", "", nil
	default:
		if stdin != nil {
			_, _ = io.ReadAll(stdin)
		}
		return "", "", nil
	}
}

type fakeTransfer struct {
	pushed     bool
	pushedFrom string
	pushedData []byte
	beforePush func()
	pulled     []string
}

func (f *fakeTransfer) Push(_ context.Context, _ model.Cluster, localDir, _ string, _ []string) error {
	if f.beforePush != nil {
		f.beforePush()
	}
	f.pushed = true
	f.pushedFrom = localDir
	f.pushedData, _ = os.ReadFile(filepath.Join(localDir, "job.inp"))
	return nil
}

func (f *fakeTransfer) Pull(_ context.Context, _ model.Cluster, _, _ string, files []string) error {
	f.pulled = append([]string{}, files...)
	return nil
}

func (f *fakeTransfer) Check(_ context.Context, _ model.Cluster) (string, error) {
	return "fake", nil
}

func TestCompleteSubmitStatusPullFlow(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, "old-location")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(root, "task01")
	if err := os.Mkdir(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "eg.inp"), []byte("input"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "old.out"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	runner := &fakeRunner{state: "COMPLETED"}
	xfer := &fakeTransfer{}
	application := &App{
		Config: model.Config{
			Version: 1,
			Clusters: map[string]model.Cluster{
				"gibbs": {Host: "gibbs", Scheduler: "slurm", RemoteRoot: "/scratch/joyrun"},
			},
			Targets: map[string]model.Target{
				"gibbs/orca": {
					Cluster: "gibbs",
					Params: map[string]model.ParamSpec{
						"cpus": {Type: "int", Default: 32},
					},
					Script: "orca {{ .Input }} > {{ .Stem }}.out\n",
					Push:   model.FilePolicy{Exclude: []string{"*.out"}},
					Pull:   model.FilePolicy{Default: []string{"*.out", "*.gbw"}},
					Logs:   []string{"{{ .Stem }}.out"},
				},
			},
		},
		Store: s, Runner: runner, Transfer: xfer,
	}
	result, err := application.Submit(ctx, root, "task01/eg.inp", "gibbs/orca", []string{"cpus=64"})
	if err != nil {
		t.Fatal(err)
	}
	if !xfer.pushed || result.Task.SchedulerID != "12345" || result.Task.State != model.StateQueued {
		t.Fatalf("unexpected submit result: %#v", result)
	}
	movedRoot := filepath.Join(base, "new-location")
	if err := os.Rename(root, movedRoot); err != nil {
		t.Fatal(err)
	}
	status, err := application.Status(ctx, movedRoot, "task01/eg.inp")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != model.StateCompleted || status.SchedulerState != "COMPLETED" {
		t.Fatalf("unexpected status: %#v", status)
	}
	pulled, err := application.Pull(ctx, movedRoot, result.Task.ID, PullOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pulled.Files, ",") != "eg.gbw,eg.out" {
		t.Fatalf("input protection or pull policy failed: %#v", pulled.Files)
	}
	if strings.Join(xfer.pulled, ",") != "eg.gbw,eg.out" {
		t.Fatalf("unexpected transfer files: %#v", xfer.pulled)
	}
	log, _, err := application.Logs(ctx, movedRoot, result.Task.ID, 10)
	if err != nil || log != "calculation output\n" {
		t.Fatalf("unexpected logs: %q, %v", log, err)
	}

	recoveryStore, err := store.Open(filepath.Join(t.TempDir(), "recovered.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer recoveryStore.Close()
	recoveryApp := &App{
		Config: application.Config, Store: recoveryStore, Runner: runner, Transfer: &fakeTransfer{},
	}
	recovered, err := recoveryApp.Recover(ctx, movedRoot, result.Task.ID, "gibbs/orca")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != result.Task.ID || recovered.State != model.StateCompleted {
		t.Fatalf("unexpected recovered task: %#v", recovered)
	}
}

func TestDryRunMakesNoTaskRecord(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	p, err := project.Init(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "job.inp"), []byte("input"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	application := &App{
		Config: model.Config{
			Clusters: map[string]model.Cluster{"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"}},
			Targets:  map[string]model.Target{"c/run": {Cluster: "c", Script: "run {{ .Input }}"}},
		},
		Store: s,
	}
	preview, _, _, err := application.Preview(ctx, root, "job.inp", "c/run", nil)
	if err != nil {
		t.Fatal(err)
	}
	if preview.RenderedScript != "run 'job.inp'" {
		t.Fatalf("unexpected preview: %#v", preview)
	}
	if _, err := s.LatestTask(ctx, p.ProjectID, "job.inp"); err == nil {
		t.Fatal("dry run unexpectedly persisted a task")
	}
}

func TestSubmitPersistsSchedulerIDBeforeMetadataRefresh(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "job.inp"), []byte("input"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	runner := &fakeRunner{failMetadataWrite: 2}
	application := &App{
		Config: model.Config{
			Clusters: map[string]model.Cluster{"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"}},
			Targets:  map[string]model.Target{"c/run": {Cluster: "c", Script: "run {{ .Input }}"}},
		},
		Store: s, Runner: runner, Transfer: &fakeTransfer{},
	}
	result, err := application.Submit(ctx, root, "job.inp", "c/run", nil)
	if err != nil {
		t.Fatalf("submission should remain successful after metadata refresh failure: %v", err)
	}
	stored, err := s.GetTask(ctx, result.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SchedulerID != "12345" || stored.State != model.StateQueued {
		t.Fatalf("scheduler identity was not persisted: %#v", stored)
	}
	if stored.Metadata["remote_metadata_error"] == "" {
		t.Fatalf("metadata refresh failure was not recorded: %#v", stored.Metadata)
	}
	recoveryStore, err := store.Open(filepath.Join(t.TempDir(), "recovered.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer recoveryStore.Close()
	recoveryApp := &App{
		Config: application.Config, Store: recoveryStore, Runner: runner, Transfer: &fakeTransfer{},
	}
	recovered, err := recoveryApp.Recover(ctx, root, result.Task.ID, "c/run")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.SchedulerID != "12345" || recovered.State != model.StateQueued {
		t.Fatalf("scheduler marker did not restore submitted task: %#v", recovered)
	}
}

func TestSubmitUploadsManifestSnapshot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(root, "job.inp")
	if err := os.WriteFile(inputPath, []byte("version one"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	xfer := &fakeTransfer{beforePush: func() {
		if err := os.WriteFile(inputPath, []byte("version two"), 0o644); err != nil {
			t.Fatal(err)
		}
	}}
	application := &App{
		Config: model.Config{
			Clusters: map[string]model.Cluster{"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"}},
			Targets:  map[string]model.Target{"c/run": {Cluster: "c", Script: "run {{ .Input }}"}},
		},
		Store: s, Runner: &fakeRunner{}, Transfer: xfer,
	}
	result, err := application.Submit(ctx, root, "job.inp", "c/run", nil)
	if err != nil {
		t.Fatal(err)
	}
	uploaded := xfer.pushedData
	if string(uploaded) != "version one" {
		t.Fatalf("upload did not use immutable snapshot: %q", uploaded)
	}
	sum := sha256.Sum256(uploaded)
	if got := result.Task.InputManifest[0].SHA256; got != hex.EncodeToString(sum[:]) {
		t.Fatalf("manifest %s does not describe uploaded bytes", got)
	}
}
