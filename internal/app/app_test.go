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
	"strconv"
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
	commandWarning     string
	blockCommand       string
	execCalls          int
}

func (f *fakeRunner) Exec(ctx context.Context, _, command string, stdin io.Reader) (string, string, error) {
	f.execCalls++
	if f.blockCommand != "" && strings.Contains(command, f.blockCommand) {
		<-ctx.Done()
		return "", "", ctx.Err()
	}
	switch {
	case strings.Contains(command, "JOYRUN_DOCTOR"):
		root := f.rootResult
		if root == "" {
			root = "pass"
		}
		return "remote_root|" + root + "\n" +
			"sbatch|pass\nsqueue|pass\nsacct|pass\nscancel|pass\nrsync|pass\n", "", nil
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
	case strings.Contains(command, "account_output=$(sacct"):
		return f.state, "", nil
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
		count := strings.Count(command, "JOYRUN_LOG_FOUND|")
		if len(f.missingLogs) < count {
			return "JOYRUN_LOG_FOUND|" + strconv.Itoa(len(f.missingLogs)) + "\ncalculation output\n", "", nil
		}
		return "JOYRUN_LOG_MISSING\n", "", nil
	case strings.Contains(command, "root=") && strings.Contains(command, "creatable:"):
		return f.rootResult, "", nil
	case strings.HasPrefix(command, "command -v "):
		return "/usr/bin/tool\n", f.commandWarning, nil
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
	runner := &fakeRunner{rootResult: "creatable:/home/user"}
	application := &App{
		Config: model.Config{
			Clusters: map[string]model.Cluster{"mindu": {
				Host: "mindu", Scheduler: "slurm", RemoteRoot: "/home/user/joyrun",
			}},
			Targets: map[string]model.Target{"mindu/run": {Cluster: "mindu", Script: "run"}},
		},
		Runner:   runner,
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
	if runner.execCalls != 1 {
		t.Fatalf("doctor used %d remote calls, want 1", runner.execCalls)
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

func TestDoctorDoesNotTreatSuccessfulCommandWarningAsFailureDetail(t *testing.T) {
	application := &App{
		Config: model.Config{
			Clusters: map[string]model.Cluster{"c": {
				Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun",
			}},
			Targets: map[string]model.Target{"c/run": {Cluster: "c", Script: "run"}},
		},
		Runner: &fakeRunner{
			rootResult: "pass", commandWarning: "setlocale: warning",
		},
		Transfer: &fakeTransfer{},
	}
	result := application.Doctor(context.Background(), "c/run")
	if !result.Ready {
		t.Fatalf("successful scheduler checks should remain ready: %#v", result)
	}
	for _, check := range result.Checks {
		if check.Name == "sbatch" && check.Message != "available" {
			t.Fatalf("successful command warning leaked into result: %#v", check)
		}
	}
}

type fakeTransfer struct {
	pushed       bool
	pushedFrom   string
	pushedData   []byte
	pushedScript []byte
	beforePush   func()
	pushErr      error
	pulled       []string
	pullErr      error
}

func (f *fakeTransfer) Push(_ context.Context, _ model.Cluster, localDir, _ string, _ []string) error {
	if f.beforePush != nil {
		f.beforePush()
	}
	if f.pushErr != nil {
		return f.pushErr
	}
	f.pushed = true
	f.pushedFrom = localDir
	f.pushedData, _ = os.ReadFile(filepath.Join(localDir, "job.inp"))
	f.pushedScript, _ = os.ReadFile(filepath.Join(localDir, "joyrun-job.sh"))
	return nil
}

func (f *fakeTransfer) Pull(_ context.Context, _ model.Cluster, _, localDir string, files []string) error {
	f.pulled = append([]string{}, files...)
	if f.pullErr == nil {
		for _, file := range files {
			path := filepath.Join(localDir, filepath.FromSlash(file))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			size := int64(1)
			switch file {
			case "eg.out":
				size = 200
			case "eg.gbw":
				size = 300
			}
			if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
				return err
			}
		}
	}
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
		ctx, root, "task01/eg.inp", "gibbs/orca", []string{"cpus=64"}, nil, "", false,
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
		"TASK_CREATED", "UPLOAD_STARTED", "REMOTE_DIR_CREATED", "METADATA_WRITTEN",
		"SNAPSHOT_UPLOADED", "SCRIPT_UPLOADED", "UPLOAD_COMPLETED", "SUBMIT_STARTED",
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

func TestExecuteReservedSubmitUsesFrozenTaskTemplateAndID(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, err := project.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.BindProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := model.Task{ID: "jr_reserved", Revision: 1, ProjectID: p.ProjectID,
		SourcePath: "task", SourceWorkDir: "task", TargetName: "cluster/test",
		ClusterName: "cluster", RemoteDir: "/scratch/joyrun/jr_reserved",
		ComputeState: model.ComputeCreated, PullState: model.PullNotPulled,
		RenderedScript: "echo jr_reserved\n", InputManifest: []model.ManifestEntry{{Path: "job.inp", Size: 5}},
		Metadata: map[string]string{"partition": "compute"}, CreatedAt: now, UpdatedAt: now}
	if err := db.CreateTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot")
	if err := os.MkdirAll(snapshot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "job.inp"), []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	xfer := &fakeTransfer{}
	application := &App{Store: db, Runner: &fakeRunner{}, Transfer: xfer,
		Config: model.Config{Clusters: map[string]model.Cluster{"cluster": {
			Host: "cluster", Scheduler: "slurm", RemoteRoot: "/scratch/joyrun",
		}}, Targets: map[string]model.Target{"cluster/test": {Cluster: "cluster"}}}}
	result, err := application.ExecuteReservedSubmit(ctx, task.ID, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if result.Task.ID != task.ID || result.Task.SchedulerID != "12345" {
		t.Fatalf("reserved submit changed identity: %#v", result.Task)
	}
	if string(xfer.pushedScript) != task.RenderedScript {
		t.Fatalf("uploaded script changed: %q", xfer.pushedScript)
	}
}

func TestExecuteReservedSubmitReconcilesSubmitFenceWithoutResubmitting(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "job.inp"), []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "joyrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	runner := &fakeRunner{}
	application := &App{
		Config: model.Config{
			Clusters: map[string]model.Cluster{"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"}},
			Targets:  map[string]model.Target{"c/run": {Cluster: "c", Script: "run {{ .Input }}", Push: model.PushPolicy{Mode: "entry"}}},
		},
		Store: s, Runner: runner, Transfer: &fakeTransfer{},
	}
	first, err := application.Submit(ctx, root, "job.inp", "c/run", nil, nil, "", true)
	if err != nil {
		t.Fatal(err)
	}
	fenced := first.Task
	fenced.ComputeState = model.ComputeCreated
	fenced.SchedulerID = ""
	fenced.UpdatedAt = time.Now().UTC()
	if err := s.UpdateTask(ctx, &fenced); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, model.TaskEvent{TaskID: fenced.ID, Type: "SUBMIT_STARTED", Stage: "submit", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	runner.schedulerID = ""
	runner.reconcileID = "54321"
	runner.execCalls = 0
	result, err := application.ExecuteReservedSubmit(ctx, fenced.ID, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if result.Task.SchedulerID != "54321" || result.Task.ComputeState != model.ComputeQueued {
		t.Fatalf("submission fence was not reconciled: %#v", result.Task)
	}
	if runner.execCalls == 0 || runner.schedulerID != "" {
		t.Fatalf("reserved reconciliation unexpectedly submitted a new job: calls=%d scheduler=%q", runner.execCalls, runner.schedulerID)
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
	_, _, _, err = application.Preview(ctx, root, "benzene", "mindu/gaussian", nil, nil, "", false)
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
		ctx, root, "job.inp", "c/run", nil, nil, "", false,
	); fault.As(err).Code != "PROJECT_ROOT_UPLOAD_FORBIDDEN" {
		t.Fatalf("expected project-root upload rejection, got %v", err)
	}
	preview, _, _, err := application.Preview(ctx, root, "job.inp", "c/run", nil, nil, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.InputManifest) != 1 || preview.InputManifest[0].Path != "job.inp" {
		t.Fatalf("unexpected explicitly allowed snapshot: %#v", preview.InputManifest)
	}
}

func TestPreviewExposesAndRendersResolvedPartitionFacts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "job.inp"), []byte("input"), 0o644); err != nil {
		t.Fatal(err)
	}
	application := &App{Config: model.Config{
		Clusters: map[string]model.Cluster{"c": {
			Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun",
			Partitions: map[string]model.Partition{
				"normal": {CoresPerNode: 32},
				"highio": {CoresPerNode: 64, MemoryPerNode: "512GiB"},
			},
		}},
		Targets: map[string]model.Target{"c/run": {
			Cluster: "c", Software: model.Software{Name: "run"},
			Placement: model.Placement{
				DefaultPartition:  "normal",
				AllowedPartitions: []string{"normal", "highio"},
			},
			Source: model.SourcePolicy{Kind: "file"},
			Push:   model.PushPolicy{Mode: "entry"},
			Script: "#SBATCH -p {{ .Partition.Name }}\n# {{ .Partition.CoresPerNode }}",
		}},
	}}
	preview, task, _, err := application.Preview(
		ctx, root, "job.inp", "c/run", nil, nil, "highio", true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Partition.Name != "highio" || preview.PartitionSource != "cli" ||
		preview.Partition.MemoryPerNode != "512GiB" ||
		!strings.Contains(preview.RenderedScript, "#SBATCH -p 'highio'") {
		t.Fatalf("unexpected partition preview: %#v", preview)
	}
	if task.Metadata["partition"] != "highio" ||
		task.Metadata["software_name"] != "run" {
		t.Fatalf("task did not snapshot execution facts: %#v", task.Metadata)
	}
}

func TestPreviewAddsOnlyExplicitEntryDependencies(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(root, "task")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"job.inp":      "input",
		"coords.xyz":   "coordinates",
		"shared.basis": "basis",
		"restart.gbw":  "restart",
		"other.inp":    "other input",
	} {
		if err := os.WriteFile(filepath.Join(workDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	application := &App{Config: model.Config{
		Clusters: map[string]model.Cluster{
			"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"},
		},
		Targets: map[string]model.Target{
			"c/run": {
				Cluster: "c", Source: model.SourcePolicy{Kind: "file", Patterns: []string{"*.inp"}},
				Push:   model.PushPolicy{Mode: "entry", Include: []string{"shared.basis"}},
				Script: "run {{ .Input }}",
			},
			"c/workdir": {
				Cluster: "c", Source: model.SourcePolicy{Kind: "directory"},
				Push: model.PushPolicy{Mode: "workdir"}, Script: "run",
			},
			"c/excluding": {
				Cluster: "c", Source: model.SourcePolicy{Kind: "file"},
				Push:   model.PushPolicy{Mode: "entry", Exclude: []string{"*.gbw"}},
				Script: "run {{ .Input }}",
			},
		},
	}}

	preview, _, _, err := application.Preview(
		ctx, root, "task/job.inp", "c/run", nil, []string{"coords.xyz"}, "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool)
	for _, entry := range preview.InputManifest {
		got[entry.Path] = true
	}
	if !got["job.inp"] || !got["shared.basis"] || !got["coords.xyz"] ||
		got["restart.gbw"] || got["other.inp"] {
		t.Fatalf("unexpected explicit dependency selection: %#v", preview.InputManifest)
	}
	if len(preview.Push.Include) != 2 ||
		preview.Push.Include[0] != "shared.basis" ||
		preview.Push.Include[1] != "coords.xyz" {
		t.Fatalf("preview did not expose resolved includes: %#v", preview.Push.Include)
	}

	if _, _, _, err := application.Preview(
		ctx, root, "task/job.inp", "c/run", nil, []string{"missing.gbw"}, "", false,
	); fault.As(err).Code != "SOURCE_DEPENDENCY_NOT_FOUND" {
		t.Fatalf("expected missing dependency rejection, got %v", err)
	}
	if _, _, _, err := application.Preview(
		ctx, root, "task/job.inp", "c/excluding", nil, []string{"restart.gbw"}, "", false,
	); fault.As(err).Code != "SOURCE_DEPENDENCY_NOT_FOUND" {
		t.Fatalf("expected excluded dependency rejection, got %v", err)
	}
	if _, _, _, err := application.Preview(
		ctx, root, "task/job.inp", "c/run", nil, []string{"["}, "", false,
	); fault.As(err).Code != "INVALID_ARGUMENT" {
		t.Fatalf("expected invalid include rejection, got %v", err)
	}
	if _, _, _, err := application.Preview(
		ctx, root, "task", "c/workdir", nil, []string{"coords.xyz"}, "", false,
	); fault.As(err).Code != "INVALID_ARGUMENT" {
		t.Fatalf("expected workdir include rejection, got %v", err)
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
	if runner.execCalls != 1 {
		t.Fatalf("log fallback used %d remote calls, want 1", runner.execCalls)
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
	if runner.execCalls != 1 {
		t.Fatalf("legacy log fallback used %d remote calls, want 1", runner.execCalls)
	}
}

func TestDryRunPreviewIsMarkedWithoutRemoteSubmission(t *testing.T) {
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
	application := &App{
		Config: model.Config{
			Clusters: map[string]model.Cluster{"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"}},
			Targets: map[string]model.Target{"c/run": {
				Cluster: "c", Script: "run {{ .Input }}", Push: model.PushPolicy{Mode: "entry"},
			}},
		},
		Store: s,
	}
	preview, task, _, err := application.Preview(ctx, root, "job.inp", "c/run", nil, nil, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if preview.RenderedScript != "run 'job.inp'" {
		t.Fatalf("unexpected preview: %#v", preview)
	}
	if !task.DryRun {
		t.Fatal("preview task was not marked as dry-run")
	}
	if err := s.CreateTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestTask(ctx, p.ProjectID, "job.inp")
	if err != nil {
		t.Fatalf("dry run preview was not persisted: %v", err)
	}
	if !got.DryRun || got.SchedulerID != "" || got.ComputeState != model.ComputeCreated {
		t.Fatalf("unexpected dry-run task: %#v", got)
	}
}

func TestSubmitRecoveryUsesImmutableMetadataAndSchedulerMarker(t *testing.T) {
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
	runner := &fakeRunner{state: "PENDING"}
	application := &App{
		Config: model.Config{
			Clusters: map[string]model.Cluster{"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"}},
			Targets: map[string]model.Target{"c/run": {
				Cluster: "c", Script: "run {{ .Input }}", Push: model.PushPolicy{Mode: "entry"},
			}},
		},
		Store: s, Runner: runner, Transfer: &fakeTransfer{},
	}
	result, err := application.Submit(ctx, root, "job.inp", "c/run", nil, nil, "", true)
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
	if runner.metadataWriteCount != 1 {
		t.Fatalf("submit wrote remote metadata %d times, want 1", runner.metadataWriteCount)
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
	result, err := application.Submit(ctx, root, "job.inp", "c/run", nil, nil, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Task.SchedulerID != "12345" || result.Task.ComputeState != model.ComputeQueued {
		t.Fatalf("accepted Slurm job was not reconciled: %#v", result.Task)
	}
}

func TestSubmitRetriesAreIdempotentAndForceNewIsExplicit(t *testing.T) {
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
	runner := &fakeRunner{state: "PENDING"}
	application := &App{
		Config: model.Config{
			Clusters: map[string]model.Cluster{"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"}},
			Targets: map[string]model.Target{"c/run": {
				Cluster: "c", Script: "run {{ .Input }}", Push: model.PushPolicy{Mode: "entry"},
			}},
		},
		Store: s, Runner: runner, Transfer: &fakeTransfer{},
	}
	first, err := application.Submit(ctx, root, "job.inp", "c/run", nil, nil, "", true)
	if err != nil {
		t.Fatal(err)
	}
	callsAfterFirst := runner.execCalls
	target := application.Config.Targets["c/run"]
	target.Pull.Default = []string{"*.out"}
	application.Config.Targets["c/run"] = target
	second, err := application.Submit(ctx, root, "job.inp", "c/run", nil, nil, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Deduplicated || second.Task.ID != first.Task.ID {
		t.Fatalf("retry did not reuse first task: first=%#v second=%#v", first.Task, second)
	}
	if runner.execCalls != callsAfterFirst {
		t.Fatalf("idempotent retry contacted remote: calls before=%d after=%d", callsAfterFirst, runner.execCalls)
	}
	runner.state = "COMPLETED"
	third, err := application.Submit(ctx, root, "job.inp", "c/run", nil, nil, "", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if third.Deduplicated || third.Task.ID == first.Task.ID {
		t.Fatalf("force-new did not create a distinct task: %#v", third)
	}
	runner.state = "RUNNING"
	if _, err := application.Submit(ctx, root, "job.inp", "c/run", nil, nil, "", true, true); fault.As(err).Code != "SUBMISSION_SAFETY_UNCONFIRMED" {
		t.Fatalf("force-new should block while prior work is running, got %v", err)
	}
	tasks, err := s.ListTasks(ctx, p.ProjectID)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("unexpected task count after force-new: %d, %v", len(tasks), err)
	}
}

func TestSubmitRemoteTimeoutPersistsFailureWithDetachedContext(t *testing.T) {
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
			Clusters: map[string]model.Cluster{
				"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"},
			},
			Targets: map[string]model.Target{
				"c/run": {
					Cluster: "c", Script: "run {{ .Input }}", Push: model.PushPolicy{Mode: "entry"},
				},
			},
		},
		Store: s, Runner: &fakeRunner{blockCommand: "metadata.json.joyrun-tmp"},
		Transfer: &fakeTransfer{}, RemoteTimeout: 10 * time.Millisecond,
	}
	_, err = application.Submit(ctx, root, "job.inp", "c/run", nil, nil, "", true)
	if fault.As(err).Code != "SSH_TIMEOUT" {
		t.Fatalf("expected SSH_TIMEOUT, got %v", err)
	}
	tasks, err := s.ListTasks(ctx, p.ProjectID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("cannot read timed-out task: %#v, %v", tasks, err)
	}
	if tasks[0].ComputeState != model.ComputeSubmissionFailed ||
		tasks[0].Metadata["error_stage"] != "metadata" {
		t.Fatalf("timeout state was not persisted: %#v", tasks[0])
	}
}

func TestSubmitTimeoutAfterSbatchIsMarkedUncertain(t *testing.T) {
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
	runner := &fakeRunner{
		blockCommand: "sbatch --parsable", markerMissing: true,
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
		SubmitTimeout: 10 * time.Millisecond, RecoveryTimeout: time.Second,
	}
	_, err = application.Submit(ctx, root, "job.inp", "c/run", nil, nil, "", true)
	if fault.As(err).Code != "SUBMISSION_UNCERTAIN" {
		t.Fatalf("expected SUBMISSION_UNCERTAIN, got %v", err)
	}
	tasks, err := s.ListTasks(ctx, p.ProjectID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("cannot read uncertain task: %#v, %v", tasks, err)
	}
	if tasks[0].ComputeState != model.ComputeSubmissionUncertain {
		t.Fatalf("uncertain state was not persisted: %#v", tasks[0])
	}
}

func TestSubmitCancellationDuringTransferPersistsFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
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
	xfer := &fakeTransfer{
		beforePush: cancel,
		pushErr:    context.Canceled,
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
		Store: s, Runner: &fakeRunner{}, Transfer: xfer,
	}
	_, err = application.Submit(ctx, root, "job.inp", "c/run", nil, nil, "", true)
	if fault.As(err).Code != "SUBMISSION_CANCELLED" {
		t.Fatalf("expected SUBMISSION_CANCELLED, got %v", err)
	}
	tasks, err := s.ListTasks(context.Background(), p.ProjectID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("cannot read cancelled task: %#v, %v", tasks, err)
	}
	if tasks[0].ComputeState != model.ComputeSubmissionFailed ||
		tasks[0].Metadata["error_stage"] != "snapshot" {
		t.Fatalf("cancelled state was not persisted: %#v", tasks[0])
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
	if err := os.WriteFile(filepath.Join(root, "restart.gbw"), []byte("restart"), 0o644); err != nil {
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
	result, err := application.Submit(
		ctx, root, "job.inp", "c/run", nil, []string{"restart.gbw"}, "", true,
	)
	if err != nil {
		t.Fatal(err)
	}
	uploaded := xfer.pushedData
	if string(uploaded) != "version one" {
		t.Fatalf("upload did not use immutable snapshot: %q", uploaded)
	}
	if string(xfer.pushedScript) != "run 'job.inp'" {
		t.Fatalf("rendered script was not uploaded with the snapshot: %q", xfer.pushedScript)
	}
	sum := sha256.Sum256(uploaded)
	if got := result.Task.InputManifest[0].SHA256; got != hex.EncodeToString(sum[:]) {
		t.Fatalf("manifest %s does not describe uploaded bytes", got)
	}
	if len(result.Task.InputManifest) != 2 ||
		result.Task.InputManifest[1].Path != "restart.gbw" {
		t.Fatalf("explicit dependency was not frozen in the task: %#v", result.Task.InputManifest)
	}
	if result.Task.Metadata["submit_includes"] != `["restart.gbw"]` {
		t.Fatalf("explicit dependency intent was not recorded: %#v", result.Task.Metadata)
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

func TestStatusAllBatchesActiveJobsAndSkipsLegacyMissingSchedulerID(t *testing.T) {
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
	tasks := []model.Task{
		{
			ID: "jr_batch_1", ProjectID: p.ProjectID, SourcePath: "one.inp",
			TargetName: "c/run", ClusterName: "c", SchedulerID: "111",
			ComputeState: model.ComputeQueued, PullState: model.PullNotPulled,
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "jr_batch_2", ProjectID: p.ProjectID, SourcePath: "two.inp",
			TargetName: "c/run", ClusterName: "c", SchedulerID: "222",
			ComputeState: model.ComputeRunning, PullState: model.PullNotPulled,
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "jr_legacy_failed", ProjectID: p.ProjectID, SourcePath: "old.inp",
			TargetName: "c/run", ClusterName: "c",
			ComputeState: model.ComputeSubmissionFailed, PullState: model.PullNotPulled,
			CreatedAt: now, UpdatedAt: now,
		},
	}
	for i := range tasks {
		if err := s.CreateTask(ctx, &tasks[i]); err != nil {
			t.Fatal(err)
		}
	}
	runner := &fakeRunner{state: strings.Join([]string{
		"Q|111|RUNNING|00:01:00||node01|2026-07-29T10:00:00|N/A",
		"A|222|COMPLETED|00:02:00|0:0|None|2026-07-29T09:58:00|2026-07-29T10:00:00",
	}, "\n")}
	application := &App{
		Config: model.Config{Clusters: map[string]model.Cluster{
			"c": {Host: "c", Scheduler: "slurm", RemoteRoot: "/tmp/joyrun"},
		}},
		Store: s, Runner: runner,
	}
	result := application.StatusAll(ctx, root)
	if len(result.Failures) != 0 || len(result.Tasks) != 3 {
		t.Fatalf("unexpected bulk result: %#v", result)
	}
	if runner.execCalls != 1 {
		t.Fatalf("status --all used %d remote calls, want 1", runner.execCalls)
	}
	states := map[string]string{}
	for _, task := range result.Tasks {
		states[task.ID] = task.ComputeState
	}
	if states["jr_batch_1"] != model.ComputeRunning ||
		states["jr_batch_2"] != model.ComputeCompleted ||
		states["jr_legacy_failed"] != model.ComputeSubmissionFailed {
		t.Fatalf("unexpected bulk states: %#v", states)
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
		recoveryScanOutput: "/tmp/joyrun/jr_scan_test\x00" + string(metadata) + "\x00",
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
	if runner.execCalls != 1 {
		t.Fatalf("recovery scan used %d remote calls, want 1", runner.execCalls)
	}
}
