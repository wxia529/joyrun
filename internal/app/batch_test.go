package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/model"
	"github.com/wxia529/joyrun/internal/project"
	"github.com/wxia529/joyrun/internal/store"
)

type batchRunner struct {
	calls    int
	listings map[string][]RemoteFile
}

func (r *batchRunner) Exec(_ context.Context, _, command string, _ io.Reader) (string, string, error) {
	r.calls++
	if strings.Contains(command, "sbatch --parsable") {
		ids := regexp.MustCompile(`joyrun:(jr_[a-zA-Z0-9_]+)`).FindAllStringSubmatch(command, -1)
		var output strings.Builder
		for index, match := range ids {
			fmt.Fprintf(&output, "OK\x00%s\x00%d\x00", match[1], 1000+index)
		}
		return output.String(), "", nil
	}
	if strings.Contains(command, "find . -type f") {
		var output strings.Builder
		for taskID, files := range r.listings {
			if !strings.Contains(command, taskID) {
				continue
			}
			for _, file := range files {
				fmt.Fprintf(&output, "%s\x00%s\x00%d\x00", taskID, file.Path, file.Size)
			}
		}
		return output.String(), "", nil
	}
	return "", "", nil
}

type batchTransfer struct {
	pushCalls int
	pullCalls int
	pulled    []string
}

func (t *batchTransfer) Push(_ context.Context, _ model.Cluster, localDir, _ string, _ []string) error {
	t.pushCalls++
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return err
	}
	if len(entries) < 2 {
		return fmt.Errorf("batch staging contains %d task directories", len(entries))
	}
	return nil
}

func (t *batchTransfer) Pull(
	_ context.Context,
	_ model.Cluster,
	_, localDir string,
	files []string,
) error {
	t.pullCalls++
	t.pulled = append([]string{}, files...)
	for _, file := range files {
		destination := filepath.Join(localDir, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(destination, []byte("result:"+file), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (t *batchTransfer) Check(context.Context, model.Cluster) (string, error) {
	return "fake", nil
}

func TestSubmitManyUploadsAndSubmitsBatchOnce(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	for index, name := range []string{"task01/a.inp", "task02/different.inp"} {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("input "+strconv.Itoa(index)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runner := &batchRunner{}
	transfer := &batchTransfer{}
	application := &App{
		Config: model.Config{
			Clusters: map[string]model.Cluster{
				"c": {
					Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun",
					Partitions: map[string]model.Partition{"cpu": {CoresPerNode: 1}},
				},
			},
			Targets: map[string]model.Target{
				"c/run": {
					Cluster: "c", Software: model.Software{Name: "run"},
					Placement: model.Placement{DefaultPartition: "cpu", AllowedPartitions: []string{"cpu"}},
					Source:    model.SourcePolicy{Kind: "file", Patterns: []string{"*.inp"}},
					Push:      model.PushPolicy{Mode: "entry"},
					Pull:      model.FilePolicy{Default: []string{"*.out"}},
					Script:    "run {{ .Input }}",
				},
			},
		},
		Store: database, Runner: runner, Transfer: transfer,
	}
	result, err := application.SubmitMany(ctx, root,
		[]string{"task01/a.inp", "task02/different.inp"},
		"c/run", nil, nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tasks) != 2 || len(result.Failures) != 0 {
		t.Fatalf("unexpected batch result: %#v", result)
	}
	if transfer.pushCalls != 1 || runner.calls != 1 {
		t.Fatalf("batch used push=%d remote=%d, want 1 each", transfer.pushCalls, runner.calls)
	}
	if result.Tasks[0].Metadata["batch_id"] == "" ||
		result.Tasks[0].Metadata["batch_id"] != result.Tasks[1].Metadata["batch_id"] {
		t.Fatalf("batch identity was not frozen in tasks: %#v", result.Tasks)
	}
	second, err := application.SubmitMany(ctx, root,
		[]string{"task01/a.inp", "task02/different.inp"},
		"c/run", nil, nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Tasks) != 2 || second.BatchID != result.BatchID {
		t.Fatalf("retry did not reuse admitted batch: %#v", second)
	}
	if transfer.pushCalls != 1 || runner.calls != 1 {
		t.Fatalf("retry resubmitted batch: push=%d remote=%d", transfer.pushCalls, runner.calls)
	}
	runner.calls = 0
	transfer.pushCalls = 0
	if _, err := application.SubmitMany(ctx, root,
		[]string{"task01/a.inp", "missing/input.inp"},
		"c/run", nil, nil, "", false); err == nil {
		t.Fatal("expected invalid source to reject the entire batch preflight")
	}
	if transfer.pushCalls != 0 || runner.calls != 0 {
		t.Fatalf("failed preflight changed remote state: push=%d remote=%d",
			transfer.pushCalls, runner.calls)
	}
}

func TestPullManyListsAndTransfersOnce(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	p, err := project.Init(root)
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.BindProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	tasks := []model.Task{
		{
			ID: "jr_pull_one", ProjectID: p.ProjectID,
			SourcePath: "task01/a.inp", SourceWorkDir: "task01",
			TargetName: "c/run", ClusterName: "c", RemoteDir: "/tmp/joyrun/jr_pull_one",
			SchedulerID: "101", ComputeState: model.ComputeCompleted, PullState: model.PullNotPulled,
			PullPatterns: []string{"*.out"}, CreatedAt: now, UpdatedAt: now,
			Metadata: map[string]string{"batch_id": "jb_pull", "batch_index": "0"},
		},
		{
			ID: "jr_pull_two", ProjectID: p.ProjectID,
			SourcePath: "task02/b.inp", SourceWorkDir: "task02",
			TargetName: "c/run", ClusterName: "c", RemoteDir: "/tmp/joyrun/jr_pull_two",
			SchedulerID: "102", ComputeState: model.ComputeCompleted, PullState: model.PullNotPulled,
			PullPatterns: []string{"*.out"}, CreatedAt: now, UpdatedAt: now,
			Metadata: map[string]string{"batch_id": "jb_pull", "batch_index": "1"},
		},
	}
	for _, directory := range []string{"task01", "task02"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for index := range tasks {
		if err := database.CreateTask(ctx, &tasks[index]); err != nil {
			t.Fatal(err)
		}
	}
	runner := &batchRunner{listings: map[string][]RemoteFile{
		"jr_pull_one": {{Path: "a.out", Size: 10}},
		"jr_pull_two": {{Path: "different.out", Size: 20}},
	}}
	transfer := &batchTransfer{}
	application := &App{
		Config: model.Config{Clusters: map[string]model.Cluster{
			"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"},
		}},
		Store: database, Runner: runner, Transfer: transfer,
	}
	result, err := application.PullMany(ctx, root, nil,
		PullManyOptions{BatchID: "jb_pull"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tasks) != 2 || len(result.Failures) != 0 {
		t.Fatalf("unexpected pull batch result: %#v", result)
	}
	if runner.calls != 1 || transfer.pullCalls != 1 {
		t.Fatalf("batch pull used list=%d transfer=%d, want 1 each",
			runner.calls, transfer.pullCalls)
	}
	for _, file := range []string{"task01/a.out", "task02/different.out"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(file))); err != nil {
			t.Fatalf("missing installed result %s: %v", file, err)
		}
	}
}

func TestPullManyRejectsCrossTaskDestinationConflict(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	p, err := project.Init(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.BindProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for index, id := range []string{"jr_conflict_one", "jr_conflict_two"} {
		task := model.Task{
			ID: id, ProjectID: p.ProjectID,
			SourcePath: fmt.Sprintf("shared/input%d.inp", index), SourceWorkDir: "shared",
			TargetName: "c/run", ClusterName: "c", RemoteDir: "/tmp/joyrun/" + id,
			SchedulerID: strconv.Itoa(300 + index), ComputeState: model.ComputeCompleted,
			PullState: model.PullNotPulled, PullPatterns: []string{"*.out"},
			CreatedAt: now, UpdatedAt: now,
		}
		if err := database.CreateTask(ctx, &task); err != nil {
			t.Fatal(err)
		}
	}
	runner := &batchRunner{listings: map[string][]RemoteFile{
		"jr_conflict_one": {{Path: "result.out", Size: 10}},
		"jr_conflict_two": {{Path: "result.out", Size: 20}},
	}}
	transfer := &batchTransfer{}
	application := &App{
		Config: model.Config{Clusters: map[string]model.Cluster{
			"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"},
		}},
		Store: database, Runner: runner, Transfer: transfer,
	}
	_, err = application.PullMany(ctx, root,
		[]string{"jr_conflict_one", "jr_conflict_two"},
		PullManyOptions{PullOptions: PullOptions{DryRun: true}})
	if fault.As(err).Code != "BATCH_LOCAL_CONFLICT" {
		t.Fatalf("expected BATCH_LOCAL_CONFLICT, got %v", err)
	}
	if transfer.pullCalls != 0 {
		t.Fatal("conflicting batch started a transfer")
	}
}
