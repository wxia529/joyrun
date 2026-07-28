package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wxia529/joyrun/internal/fault"
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
	missingLogs        []string
	rootResult         string
	reconcileID        string
	reconcileTaskID    string
	submitDisconnect   bool
	markerMissing      bool
	findOutput         *string
	recoveryScanOutput string
}

func (f *fakeRunner) Exec(_ context.Context, _, command string, stdin io.Reader) (string, string, error) {
	switch {
	case strings.Contains(command, "sbatch --parsable"):
		f.schedulerID = "12345"
		if f.reconcileTaskID == "" {
			if start := strings.Index(command, "joyrun:"); start >= 0 {
				value := command[start+len("joyrun:"):]
				if end := strings.IndexAny(value, "'\" "); end >= 0 {
					value = value[:end]
				}
				f.reconcileTaskID = value
			}
		}
		if f.submitDisconnect {
			return "", "connection lost", errors.New("connection lost")
		}
		return "12345\n", "", nil
	case strings.Contains(command, "squeue -h -o '%A|%k'"):
		if f.reconcileID != "" {
			return f.reconcileID + "|joyrun:" + f.reconcileTaskID + "\n", "", nil
		}
		return "", "", nil
	case strings.Contains(command, "sacct -n -X --starttime"):
		return "", "", nil
	case strings.Contains(command, "squeue -h"):
		return f.state, "", nil
	case strings.Contains(command, "find . -type f"):
		if f.findOutput != nil {
			return *f.findOutput, "", nil
		}
		return "eg.inp\x00100\x00eg.out\x00200\x00eg.gbw\x00300\x00scratch.tmp\x00400\x00joyrun-job.sh\x00500\x00", "", nil
	case strings.Contains(command, "-name metadata.json"):
		return f.recoveryScanOutput, "", nil
	case strings.Contains(command, "tail -n"):
		for _, name := range f.missingLogs {
			if strings.Contains(command, name) {
				return "MISSING\n", "", nil
			}
		}
		return "FOUND\ncalculation output\n", "", nil
	case strings.Contains(command, "root=") && strings.Contains(command, "creatable:"):
		return f.rootResult, "", nil
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
		if f.markerMissing || f.schedulerID == "" {
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

func TestDoctorTreatsCreatableRemoteRootAsWarning(t *testing.T) {
	application := &App{
		Config: model.Config{
			Clusters: map[string]model.Cluster{"mindu": {
				Host: "mindu", Scheduler: "slurm", RemoteRoot: "/home/user/joyrun",
			}},
			Targets: map[string]model.Target{"mindu/run": {Cluster: "mindu", Script: "run"}},
		},
		Runner:   &fakeRunner{rootResult: "creatable:/home/user"},
		Transfer: &fakeTransfer{},
	}
	result := application.Doctor(context.Background(), "mindu/run")
	if !result.Ready {
		t.Fatalf("creatable remote root should not block readiness: %#v", result)
	}
	var root DoctorCheck
	for _, check := range result.Checks {
		if check.Name == "remote_root" {
			root = check
		}
	}
	if root.Status != "warn" || !root.OK || root.Blocking {
		t.Fatalf("unexpected remote root warning: %#v", root)
	}
}

func TestDoctorBlocksUnwritableRemoteRoot(t *testing.T) {
	application := &App{
		Config: model.Config{
			Clusters: map[string]model.Cluster{"mindu": {
				Host: "mindu", Scheduler: "slurm", RemoteRoot: "/shared/joyrun",
			}},
			Targets: map[string]model.Target{"mindu/run": {Cluster: "mindu", Script: "run"}},
		},
		Runner:   &fakeRunner{rootResult: "not_writable"},
		Transfer: &fakeTransfer{},
	}
	result := application.Doctor(context.Background(), "mindu/run")
	if result.Ready {
		t.Fatalf("unwritable remote root should block readiness: %#v", result)
	}
	for _, check := range result.Checks {
		if check.Name == "remote_root" && (check.Status != "fail" || check.SuggestedAction == "") {
			t.Fatalf("unexpected remote root failure: %#v", check)
		}
	}
}

type fakeTransfer struct {
	pushed     bool
	pushedFrom string
	pushedData []byte
	beforePush func()
	pulled     []string
	pullErr    error
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
	return f.pullErr
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
	runner := &fakeRunner{state: "COMPLETED|00:12:34|0:0|None|2026-07-28T10:00:00|2026-07-28T10:12:34"}
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
					Push:   model.PushPolicy{Mode: "entry", Exclude: []string{"*.out"}},
					Pull:   model.FilePolicy{Default: []string{"*.out", "*.gbw"}},
					Logs:   []string{"{{ .Stem }}.out"},
				},
			},
		},
		Store: s, Runner: runner, Transfer: xfer,
	}
	result, err := application.Submit(
		ctx, root, "task01/eg.inp", "gibbs/orca", []string{"cpus=64"}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !xfer.pushed || result.Task.SchedulerID != "12345" || result.Task.ComputeState != model.ComputeQueued {
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
	if status.ComputeState != model.ComputeCompleted ||
		status.PullState != model.PullNotPulled ||
		status.SchedulerState != "COMPLETED" ||
		status.Elapsed != "00:12:34" ||
		status.ExitCode != "0:0" ||
		status.SchedulerStart == "" ||
		status.SchedulerEnd == "" {
		t.Fatalf("unexpected status: %#v", status)
	}
	remoteFiles, err := application.RemoteFiles(ctx, movedRoot, result.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	inputMarked := false
	for _, file := range remoteFiles {
		inputMarked = inputMarked || (file.Path == "eg.inp" && file.Input)
	}
	if len(remoteFiles) != 4 || !inputMarked {
		t.Fatalf("unexpected remote files: %#v", remoteFiles)
	}
	planned, err := application.Pull(ctx, movedRoot, result.Task.ID, PullOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !planned.DryRun || planned.TotalBytes != 500 ||
		strings.Join(planned.Files, ",") != "eg.gbw,eg.out" ||
		len(xfer.pulled) != 0 {
		t.Fatalf("dry-run changed state or selected unexpected files: %#v transfer=%#v", planned, xfer)
	}
	storedAfterPlan, err := s.GetTask(ctx, result.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedAfterPlan.PullState != model.PullNotPulled {
		t.Fatalf("dry-run changed pull state: %#v", storedAfterPlan)
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
	if pulled.Task.ComputeState != model.ComputeCompleted || pulled.Task.PullState != model.PullSucceeded {
		t.Fatalf("pull collapsed compute and result state: %#v", pulled.Task)
	}
	trace, err := application.Trace(ctx, movedRoot, result.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	eventTypes := make([]string, 0, len(trace.Events))
	for _, event := range trace.Events {
		eventTypes = append(eventTypes, event.Type)
	}
	for _, expected := range []string{
		"TASK_CREATED", "UPLOAD_STARTED", "UPLOAD_COMPLETED", "SUBMIT_STARTED",
		"SCHEDULER_ACCEPTED", "COMPUTE_STATE_CHANGED", "PULL_STARTED", "PULL_COMPLETED",
	} {
		if !contains(eventTypes, expected) {
			t.Fatalf("missing event %s in %#v", expected, eventTypes)
		}
	}
	log, err := application.Logs(ctx, movedRoot, result.Task.ID, 10, "")
	if err != nil || log.Content != "calculation output\n" || log.Kind != "application" {
		t.Fatalf("unexpected logs: %#v, %v", log, err)
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
	if recovered.ID != result.Task.ID || recovered.ComputeState != model.ComputeCompleted {
		t.Fatalf("unexpected recovered task: %#v", recovered)
	}
}

func TestPullFailurePreservesCompletedComputeState(t *testing.T) {
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
	if err := s.BindProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	entry := "job.inp"
	now := time.Now().UTC()
	task := model.Task{
		ID: "jr_pull_failure", ProjectID: p.ProjectID, SourcePath: "job.inp",
		SourceEntry: &entry, TargetName: "c/run", ClusterName: "c",
		RemoteDir: "/tmp/joyrun/jr_pull_failure", SchedulerID: "12345",
		ComputeState: model.ComputeCompleted, PullState: model.PullNotPulled,
		ResolvedParams: map[string]any{}, CreatedAt: now, UpdatedAt: now,
		PullPatterns: []string{"*.out"},
	}
	if err := s.CreateTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	xfer := &fakeTransfer{pullErr: errors.New("connection reset")}
	application := &App{
		Config: model.Config{Clusters: map[string]model.Cluster{
			"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"},
		}},
		Store: s, Runner: &fakeRunner{state: "COMPLETED"}, Transfer: xfer,
	}
	_, pullErr := application.Pull(ctx, root, task.ID, PullOptions{})
	if fault.As(pullErr).Code != "PULL_FAILED" {
		t.Fatalf("unexpected pull error: %v", pullErr)
	}
	stored, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ComputeState != model.ComputeCompleted || stored.PullState != model.PullFailed {
		t.Fatalf("pull failure corrupted task states: %#v", stored)
	}
	if fault.As(pullErr).SuggestedAction != "joyrun pull "+task.ID {
		t.Fatalf("missing retry action: %#v", fault.As(pullErr))
	}
	application.Runner = &fakeRunner{}
	refreshed, err := application.Status(ctx, root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.ComputeState != model.ComputeCompleted ||
		refreshed.PullState != model.PullFailed {
		t.Fatalf("status regressed durable terminal/result state: %#v", refreshed)
	}
}

func TestCancelRequiresExactTaskID(t *testing.T) {
	application := &App{}
	_, err := application.Cancel(context.Background(), t.TempDir(), "task01/eg.inp")
	if fault.As(err).Code != "CANCEL_REQUIRES_TASK_ID" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func TestPreviewRejectsDirectoryForFileTarget(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(root, "benzene")
	if err := os.Mkdir(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "benzene_opt.gjf"), []byte("input"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	application := &App{
		Config: model.Config{
			Clusters: map[string]model.Cluster{"mindu": {Host: "mindu", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"}},
			Targets: map[string]model.Target{"mindu/gaussian": {
				Cluster: "mindu",
				Source:  model.SourcePolicy{Kind: "file", Patterns: []string{"*.gjf"}},
				Script:  "g16 < {{ .Input }} > {{ .Stem }}.log",
				Push:    model.PushPolicy{Mode: "entry"},
			}},
		},
		Store: s,
	}
	_, _, _, err = application.Preview(ctx, root, "benzene", "mindu/gaussian", nil, false)
	if fault.As(err).Code != "SOURCE_KIND_MISMATCH" {
		t.Fatalf("expected source kind mismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), "benzene/benzene_opt.gjf") {
		t.Fatalf("expected actionable candidate in error, got %v", err)
	}
}

func TestSourceContractRejectsWrongKindAndPattern(t *testing.T) {
	entry := "job.txt"
	tests := []struct {
		name   string
		source model.Source
		target model.Target
		code   string
	}{
		{
			name:   "file passed to directory target",
			source: model.Source{RelativePath: "job.txt", Entry: &entry, Kind: "file"},
			target: model.Target{Source: model.SourcePolicy{Kind: "directory"}},
			code:   "SOURCE_KIND_MISMATCH",
		},
		{
			name:   "entry does not match target patterns",
			source: model.Source{RelativePath: "job.txt", Entry: &entry, Kind: "file"},
			target: model.Target{Source: model.SourcePolicy{Kind: "file", Patterns: []string{"*.inp"}}},
			code:   "SOURCE_PATTERN_MISMATCH",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSourceContract(test.source, t.TempDir(), "c/run", test.target)
			if fault.As(err).Code != test.code {
				t.Fatalf("expected %s, got %v", test.code, err)
			}
		})
	}
}

func TestPreviewRejectsProjectRootWithoutExplicitOverride(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "job.inp"), []byte("input"), 0o644); err != nil {
		t.Fatal(err)
	}
	application := &App{Config: model.Config{
		Clusters: map[string]model.Cluster{
			"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"},
		},
		Targets: map[string]model.Target{
			"c/run": {
				Cluster: "c", Source: model.SourcePolicy{Kind: "file"},
				Push: model.PushPolicy{Mode: "entry"}, Script: "run {{ .Input }}",
			},
		},
	}}
	if _, _, _, err := application.Preview(
		ctx, root, "job.inp", "c/run", nil, false,
	); fault.As(err).Code != "PROJECT_ROOT_UPLOAD_FORBIDDEN" {
		t.Fatalf("expected project-root upload rejection, got %v", err)
	}
	preview, _, _, err := application.Preview(ctx, root, "job.inp", "c/run", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.InputManifest) != 1 || preview.InputManifest[0].Path != "job.inp" {
		t.Fatalf("unexpected explicitly allowed snapshot: %#v", preview.InputManifest)
	}
}

func TestLogsFallBackToJoyRunSchedulerLog(t *testing.T) {
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
	if err := s.BindProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	entry := "job.inp"
	now := time.Now().UTC()
	task := model.Task{
		ID: "jr_logs", ProjectID: p.ProjectID, SourcePath: "job.inp", SourceEntry: &entry,
		TargetName: "c/run", ClusterName: "c", RemoteDir: "/tmp/joyrun/jr_logs",
		SchedulerID: "12345", ComputeState: model.ComputeFailed, PullState: model.PullNotPulled,
		Logs:           []string{"job.log"},
		ResolvedParams: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{missingLogs: []string{"job.log"}}
	application := &App{
		Config: model.Config{Clusters: map[string]model.Cluster{
			"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"},
		}},
		Store: s, Runner: runner,
	}
	result, err := application.Logs(ctx, root, task.ID, 20, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind != "scheduler" || result.Path != "joyrun-slurm-12345.log" {
		t.Fatalf("unexpected fallback: %#v", result)
	}
}

func TestLogsSupportLegacySchedulerOutput(t *testing.T) {
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
	if err := s.BindProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	entry := "job.inp"
	now := time.Now().UTC()
	task := model.Task{
		ID: "jr_legacy_logs", ProjectID: p.ProjectID, SourcePath: "job.inp", SourceEntry: &entry,
		TargetName: "c/run", ClusterName: "c", RemoteDir: "/tmp/joyrun/jr_legacy_logs",
		SchedulerID: "9876", ComputeState: model.ComputeFailed, PullState: model.PullNotPulled,
		Logs:           []string{".log"},
		ResolvedParams: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{missingLogs: []string{".log", "joyrun-slurm-9876.log"}}
	application := &App{
		Config: model.Config{Clusters: map[string]model.Cluster{
			"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"},
		}},
		Store: s, Runner: runner,
	}
	result, err := application.Logs(ctx, root, task.ID, 20, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind != "scheduler_legacy" || result.Path != "slurm-9876.out" {
		t.Fatalf("unexpected legacy fallback: %#v", result)
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
			Targets: map[string]model.Target{"c/run": {
				Cluster: "c", Script: "run {{ .Input }}", Push: model.PushPolicy{Mode: "entry"},
			}},
		},
		Store: s,
	}
	preview, _, _, err := application.Preview(ctx, root, "job.inp", "c/run", nil, true)
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
	runner := &fakeRunner{failMetadataWrite: 2, state: "PENDING"}
	application := &App{
		Config: model.Config{
			Clusters: map[string]model.Cluster{"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"}},
			Targets: map[string]model.Target{"c/run": {
				Cluster: "c", Script: "run {{ .Input }}", Push: model.PushPolicy{Mode: "entry"},
			}},
		},
		Store: s, Runner: runner, Transfer: &fakeTransfer{},
	}
	result, err := application.Submit(ctx, root, "job.inp", "c/run", nil, true)
	if err != nil {
		t.Fatalf("submission should remain successful after metadata refresh failure: %v", err)
	}
	stored, err := s.GetTask(ctx, result.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SchedulerID != "12345" || stored.ComputeState != model.ComputeQueued {
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
	if recovered.SchedulerID != "12345" || recovered.ComputeState != model.ComputeQueued {
		t.Fatalf("scheduler marker did not restore submitted task: %#v", recovered)
	}
}

func TestSubmitReconcilesAcceptedJobAfterSSHDisconnect(t *testing.T) {
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
	runner := &fakeRunner{
		submitDisconnect: true, markerMissing: true, reconcileID: "12345",
	}
	application := &App{
		Config: model.Config{
			Clusters: map[string]model.Cluster{
				"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"},
			},
			Targets: map[string]model.Target{
				"c/run": {
					Cluster: "c", Script: "run {{ .Input }}", Push: model.PushPolicy{Mode: "entry"},
				},
			},
		},
		Store: s, Runner: runner, Transfer: &fakeTransfer{},
	}
	result, err := application.Submit(ctx, root, "job.inp", "c/run", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Task.SchedulerID != "12345" || result.Task.ComputeState != model.ComputeQueued {
		t.Fatalf("accepted Slurm job was not reconciled: %#v", result.Task)
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
			Targets: map[string]model.Target{"c/run": {
				Cluster: "c", Script: "run {{ .Input }}", Push: model.PushPolicy{Mode: "entry"},
			}},
		},
		Store: s, Runner: &fakeRunner{}, Transfer: xfer,
	}
	result, err := application.Submit(ctx, root, "job.inp", "c/run", nil, true)
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

func TestStatusReconcilesSchedulerIDWhenMarkerIsMissing(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	p, err := project.Init(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.BindProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := model.Task{
		ID: "jr_reconcile", ProjectID: p.ProjectID, SourcePath: ".",
		TargetName: "c/run", ClusterName: "c", RemoteDir: "/tmp/joyrun/jr_reconcile",
		ComputeState: model.ComputeSubmissionFailed, PullState: model.PullNotPulled,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{
		markerMissing: true, reconcileID: "24680", reconcileTaskID: task.ID, state: "PENDING",
	}
	application := &App{
		Config: model.Config{Clusters: map[string]model.Cluster{
			"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"},
		}},
		Store: s, Runner: runner,
	}
	got, err := application.Status(ctx, root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchedulerID != "24680" || got.ComputeState != model.ComputeQueued {
		t.Fatalf("failed to reconcile scheduler task: %#v", got)
	}
}

func TestRecoverRejectsMetadataOutsideExpectedTaskDirectory(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	p, err := project.Init(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	unsafe := model.Task{
		ID: "jr_unsafe", ProjectID: p.ProjectID, SourcePath: ".", TargetName: "c/run",
		ClusterName: "c", RemoteDir: "/home/user", ComputeState: model.ComputeCompleted,
		PullState: model.PullNotPulled, CreatedAt: now, UpdatedAt: now,
		Metadata: map[string]string{"recovery_format": "1"},
	}
	data, err := json.Marshal(unsafe)
	if err != nil {
		t.Fatal(err)
	}
	application := &App{
		Config: model.Config{
			Clusters: map[string]model.Cluster{
				"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"},
			},
			Targets: map[string]model.Target{"c/run": {Cluster: "c"}},
		},
		Store: s, Runner: &fakeRunner{metadata: data},
	}
	if _, err := application.Recover(ctx, root, unsafe.ID, "c/run"); fault.As(err).Code != "RECOVERY_FAILED" {
		t.Fatalf("expected unsafe metadata to be rejected, got %v", err)
	}
}

func TestRecoveryRelativePathsRejectWindowsAndPOSIXEscapes(t *testing.T) {
	for _, value := range []string{"../escape", `..\escape`, "/absolute", `C:\absolute`} {
		if safeTaskRelative(value, false) {
			t.Fatalf("unsafe recovery path accepted: %q", value)
		}
	}
	for _, value := range []string{"", "task01", "nested/results"} {
		if !safeTaskRelative(value, false) {
			t.Fatalf("safe recovery path rejected: %q", value)
		}
	}
}

func TestPullRejectsEmptySelection(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	p, err := project.Init(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.BindProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := model.Task{
		ID: "jr_empty_pull", ProjectID: p.ProjectID, SourcePath: ".",
		TargetName: "c/run", ClusterName: "c", RemoteDir: "/tmp/joyrun/jr_empty_pull",
		SchedulerID: "123", ComputeState: model.ComputeCompleted, PullState: model.PullNotPulled,
		PullPatterns: []string{"*.out"}, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	output := "joyrun-job.sh\x00"
	application := &App{
		Config: model.Config{Clusters: map[string]model.Cluster{
			"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"},
		}},
		Store: s, Runner: &fakeRunner{state: "COMPLETED", findOutput: &output},
		Transfer: &fakeTransfer{},
	}
	if _, err := application.Pull(ctx, root, task.ID, PullOptions{}); fault.As(err).Code != "NO_FILES_MATCHED" {
		t.Fatalf("expected empty pull to fail clearly, got %v", err)
	}
}

func TestLogsReportsRemovedCluster(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	p, err := project.Init(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.BindProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := model.Task{
		ID: "jr_missing_cluster", ProjectID: p.ProjectID, SourcePath: ".",
		TargetName: "c/run", ClusterName: "removed", RemoteDir: "/tmp/joyrun/task",
		ComputeState: model.ComputeFailed, PullState: model.PullNotPulled,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	application := &App{Config: model.Config{Clusters: map[string]model.Cluster{}}, Store: s}
	if _, err := application.Logs(ctx, root, task.ID, 10, ""); fault.As(err).Code != "CLUSTER_NOT_FOUND" {
		t.Fatalf("expected missing cluster error, got %v", err)
	}
}

func TestRecoveryScanFindsCurrentProjectTasksWithoutDatabase(t *testing.T) {
	root := t.TempDir()
	p, err := project.Init(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := model.Task{
		ID: "jr_scan_test", ProjectID: p.ProjectID,
		SourcePath: "task/job.inp", SourceWorkDir: "task",
		TargetName: "c/run", ClusterName: "c",
		RemoteDir:    "/tmp/joyrun/jr_scan_test",
		ComputeState: model.ComputeCompleted, UpdatedAt: now,
		Metadata: map[string]string{"recovery_format": "1"},
	}
	metadata, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{
		metadata:           metadata,
		recoveryScanOutput: "/tmp/joyrun/jr_scan_test\x00",
	}
	application := &App{
		Config: model.Config{
			Clusters: map[string]model.Cluster{
				"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"},
			},
			Targets: map[string]model.Target{"c/run": {Cluster: "c"}},
		},
		Runner: runner,
	}
	candidates, err := application.RecoveryCandidates(
		context.Background(), root, "c/run")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].TaskID != task.ID {
		t.Fatalf("unexpected recovery candidates: %#v", candidates)
	}
}
