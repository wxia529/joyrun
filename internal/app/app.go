package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wxia529/joyrun/internal/config"
	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/identity"
	"github.com/wxia529/joyrun/internal/localfs"
	"github.com/wxia529/joyrun/internal/manifest"
	"github.com/wxia529/joyrun/internal/model"
	"github.com/wxia529/joyrun/internal/project"
	"github.com/wxia529/joyrun/internal/remote"
	"github.com/wxia529/joyrun/internal/scheduler"
	"github.com/wxia529/joyrun/internal/source"
	"github.com/wxia529/joyrun/internal/store"
	jtemplate "github.com/wxia529/joyrun/internal/template"
)

type App struct {
	Config             model.Config
	Store              *store.Store
	Runner             remote.Runner
	Scheduler          Scheduler
	Transfer           Transfer
	TransferInspector  TransferInspector
	Progress           io.Writer
	RemoteTimeout      time.Duration
	SubmitTimeout      time.Duration
	RecoveryTimeout    time.Duration
	PersistenceTimeout time.Duration
}

type Transfer interface {
	Push(ctx context.Context, cluster model.Cluster, localDir, remoteDir string, excludes []string) error
	Pull(ctx context.Context, cluster model.Cluster, remoteDir, localDir string, files []string) error
}

// TransferInspector is the optional diagnostics seam used by doctor. It is
// intentionally separate from the submit/pull data path so a transfer
// implementation only needs to provide the operations it actually supports.
type TransferInspector interface {
	Check(ctx context.Context, cluster model.Cluster) (string, error)
}

// Scheduler is the execution scheduler seam used by the application layer.
// Keeping it here lets tests and future scheduler adapters reuse the same
// submit/status/cancel pipeline without constructing Slurm directly.
type Scheduler interface {
	Submit(context.Context, string, string, string, string) (string, error)
	SubmitMany(context.Context, string, []scheduler.BatchJob) (scheduler.BatchSubmitResult, error)
	FindByTaskID(context.Context, string, string, time.Time) (string, error)
	Status(context.Context, string, string) (scheduler.JobStatus, error)
	Statuses(context.Context, string, []string) (map[string]scheduler.JobStatus, error)
	Cancel(context.Context, string, string) error
	Nodes(context.Context, string, string) (scheduler.NodesResult, error)
}

func (a *App) scheduler() Scheduler {
	if a.Scheduler != nil {
		return a.Scheduler
	}
	return scheduler.Slurm{Runner: a.Runner}
}

type Preview struct {
	TaskID          string                  `json:"task_id"`
	Source          model.Source            `json:"source"`
	Target          string                  `json:"target"`
	Cluster         string                  `json:"cluster"`
	Software        model.Software          `json:"software"`
	Partition       model.ResolvedPartition `json:"partition"`
	PartitionSource string                  `json:"partition_source"`
	Push            model.PushPolicy        `json:"push"`
	RemoteDir       string                  `json:"remote_dir"`
	Params          map[string]any          `json:"params"`
	ParamSources    map[string]string       `json:"param_sources"`
	Files           []string                `json:"upload_files"`
	Ignored         []string                `json:"ignored"`
	RenderedScript  string                  `json:"rendered_script"`
	InputManifest   []model.ManifestEntry   `json:"input_manifest"`
	TemplateValues  TemplateValues          `json:"template_values"`
	SchedulerLog    string                  `json:"scheduler_log"`
}

type TemplateValues struct {
	Input   string `json:"input"`
	Stem    string `json:"stem"`
	Name    string `json:"name"`
	WorkDir string `json:"workdir"`
}

type SubmitResult struct {
	Task         model.Task `json:"task"`
	Deduplicated bool       `json:"deduplicated,omitempty"`
}

type PullOptions struct {
	All             bool
	Include         []string
	OverwriteInputs bool
	Live            bool
	DryRun          bool
}

type PullResult struct {
	Task        model.Task `json:"task"`
	Files       []string   `json:"files"`
	Destination string     `json:"destination"`
	TotalBytes  int64      `json:"total_bytes"`
	DryRun      bool       `json:"dry_run"`
}

type RemoteFile struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Input bool   `json:"input"`
}

type LogResult struct {
	TaskID  string `json:"task_id"`
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

type TraceResult struct {
	Task   model.Task        `json:"task"`
	Events []model.TaskEvent `json:"events"`
}

type StatusFailure struct {
	TaskID string       `json:"task_id"`
	Error  *fault.Error `json:"error"`
}

type StatusAllResult struct {
	Tasks    []model.Task    `json:"tasks"`
	Failures []StatusFailure `json:"failures,omitempty"`
}

type DoctorCheck struct {
	Name            string `json:"name"`
	Status          string `json:"status"`
	OK              bool   `json:"ok"`
	Blocking        bool   `json:"blocking"`
	Message         string `json:"message,omitempty"`
	SuggestedAction string `json:"suggested_action,omitempty"`
}

type DoctorResult struct {
	Ready  bool          `json:"ready"`
	Checks []DoctorCheck `json:"checks"`
}

type RecoveryCandidate struct {
	TaskID       string    `json:"task_id"`
	SourcePath   string    `json:"source_path"`
	ComputeState string    `json:"compute_state"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (a *App) Close() error {
	if a.Store != nil {
		return a.Store.Close()
	}
	return nil
}

func (a *App) InitProject(ctx context.Context, root string) (model.Project, error) {
	p, err := project.Init(root)
	if err != nil {
		return p, err
	}
	if err := a.Store.BindProject(ctx, p); err != nil {
		return p, err
	}
	return p, nil
}

func (a *App) Preview(
	ctx context.Context,
	cwd, sourcePath, targetName string,
	sets []string,
	includes []string,
	partitionOverride string,
	allowProjectRoot bool,
) (Preview, model.Task, string, error) {
	return a.prepare(ctx, cwd, sourcePath, targetName, sets, includes, partitionOverride, true, allowProjectRoot)
}

func (a *App) prepare(
	ctx context.Context,
	cwd, sourcePath, targetName string,
	sets []string,
	includes []string,
	partitionOverride string,
	scanManifest bool,
	allowProjectRoot bool,
) (Preview, model.Task, string, error) {
	p, err := project.Discover(cwd)
	if err != nil {
		return Preview{}, model.Task{}, "", err
	}
	src, localWorkDir, err := source.Resolve(p, resolveFrom(cwd, sourcePath))
	if err != nil {
		return Preview{}, model.Task{}, "", err
	}
	if !allowProjectRoot && sameDirectory(localWorkDir, p.Root) {
		return Preview{}, model.Task{}, "", fault.New("PROJECT_ROOT_UPLOAD_FORBIDDEN",
			"the selected source would upload from the JoyRun project root", false).
			WithAction("move the task into its own directory or explicitly add --allow-project-root")
	}
	target, ok := a.Config.Targets[targetName]
	if !ok {
		return Preview{}, model.Task{}, "", fault.New("TARGET_NOT_FOUND", fmt.Sprintf("target %q not found", targetName), false)
	}
	if err := validateSourceContract(src, localWorkDir, targetName, target); err != nil {
		return Preview{}, model.Task{}, "", err
	}
	cluster := a.Config.Clusters[target.Cluster]
	partition, partitionSource, err := config.ResolvePartition(cluster, target, partitionOverride)
	if err != nil {
		return Preview{}, model.Task{}, "", err
	}
	params, paramSources, err := config.ResolveParams(target, sets)
	if err != nil {
		return Preview{}, model.Task{}, "", err
	}
	taskID, err := identity.New("jr_")
	if err != nil {
		return Preview{}, model.Task{}, "", fault.Wrap("TASK_CREATE_FAILED", "cannot allocate task ID", false, err)
	}
	remoteDir := path.Join(cluster.RemoteRoot, taskID)
	remoteWorkDir := path.Join(remoteDir, "work")
	values := jtemplate.Values(src, taskID, remoteWorkDir, filepath.Base(localWorkDir), params)
	values.Partition = partition
	script, err := jtemplate.Render(target, values)
	if err != nil {
		return Preview{}, model.Task{}, "", err
	}
	var inputManifest []model.ManifestEntry
	var ignored []string
	selection, err := uploadSelection(p.Root, localWorkDir, src, target, includes)
	if err != nil {
		return Preview{}, model.Task{}, "", err
	}
	if scanManifest {
		inputManifest, ignored, err = manifest.Build(localWorkDir, selection)
		if err != nil {
			return Preview{}, model.Task{}, "", err
		}
		if err := validateRequestedIncludes(includes, inputManifest); err != nil {
			return Preview{}, model.Task{}, "", err
		}
	}
	files := make([]string, 0, len(inputManifest))
	for _, entry := range inputManifest {
		files = append(files, entry.Path)
	}
	logs := make([]string, 0, len(target.Logs))
	for _, value := range target.Logs {
		rendered, err := jtemplate.RenderString(value, values)
		if err != nil {
			return Preview{}, model.Task{}, "", fault.Wrap("TARGET_INVALID", "cannot render target log path", false, err)
		}
		logs = append(logs, rendered)
	}
	now := time.Now().UTC()
	metadata := map[string]string{"recovery_format": "1"}
	if target.Software.Name != "" {
		metadata["software_name"] = target.Software.Name
	}
	if target.Software.Version != "" {
		metadata["software_version"] = target.Software.Version
	}
	if partition.Name != "" {
		metadata["partition"] = partition.Name
		metadata["partition_source"] = partitionSource
		if partition.CoresPerNode > 0 {
			metadata["cores_per_node"] = strconv.Itoa(partition.CoresPerNode)
		}
		if partition.MemoryPerNode != "" {
			metadata["memory_per_node"] = partition.MemoryPerNode
		}
	}
	if len(includes) > 0 {
		encoded, _ := json.Marshal(includes)
		metadata["submit_includes"] = string(encoded)
	}
	task := model.Task{
		ID: taskID, DryRun: scanManifest, ProjectID: p.ProjectID, SourcePath: src.RelativePath, SourceWorkDir: src.WorkDir,
		SourceEntry: src.Entry, TargetName: targetName, ClusterName: target.Cluster,
		RemoteDir: remoteDir, ComputeState: model.ComputeCreated, PullState: model.PullNotPulled,
		ResolvedParams: params,
		RenderedScript: script, TargetHash: jtemplate.TargetHash(target), InputManifest: inputManifest,
		PullPatterns: append([]string{}, target.Pull.Default...),
		PushExcludes: selection.Exclude,
		Logs:         logs, Metadata: metadata,
		CreatedAt: now, UpdatedAt: now,
	}
	resolvedPush := target.Push
	resolvedPush.Include = append([]string{}, selection.Include...)
	preview := Preview{
		TaskID: taskID, Source: src, Target: targetName, Cluster: target.Cluster,
		Software: target.Software, Partition: partition, PartitionSource: partitionSource,
		Push:      resolvedPush,
		RemoteDir: remoteDir, Params: params, ParamSources: paramSources, Files: files,
		Ignored: ignored, RenderedScript: script, InputManifest: inputManifest,
		TemplateValues: TemplateValues{
			Input: values.Input, Stem: values.Stem, Name: values.Name, WorkDir: values.WorkDir,
		},
		SchedulerLog: "joyrun-slurm-<jobid>.log",
	}
	return preview, task, localWorkDir, nil
}

func sameDirectory(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr == nil && rightErr == nil {
		return os.SameFile(leftInfo, rightInfo)
	}
	leftAbsolute, _ := filepath.Abs(left)
	rightAbsolute, _ := filepath.Abs(right)
	return filepath.Clean(leftAbsolute) == filepath.Clean(rightAbsolute)
}

func uploadSelection(
	projectRoot, localWorkDir string,
	src model.Source,
	target model.Target,
	requestedIncludes []string,
) (manifest.Selection, error) {
	var maxTotalBytes int64
	var err error
	if target.Push.Limits.MaxTotalSize != "" {
		maxTotalBytes, err = config.ParseByteSize(target.Push.Limits.MaxTotalSize)
		if err != nil {
			return manifest.Selection{}, fault.Wrap("TARGET_INVALID",
				"cannot resolve push.limits.max_total_size", false, err)
		}
	}
	entry := ""
	if src.Entry != nil {
		entry = *src.Entry
	}
	includes, err := resolveSubmitIncludes(target, requestedIncludes)
	if err != nil {
		return manifest.Selection{}, err
	}
	return manifest.Selection{
		Mode: target.Push.Mode, Entry: entry,
		Include:  includes,
		Exclude:  manifest.ExcludePatterns(projectRoot, localWorkDir, target.Push.Exclude),
		MaxFiles: target.Push.Limits.MaxFiles, MaxTotalBytes: maxTotalBytes,
	}, nil
}

func resolveSubmitIncludes(target model.Target, requested []string) ([]string, error) {
	if len(requested) > 0 && target.Push.Mode != "entry" {
		return nil, fault.New("INVALID_ARGUMENT",
			"--include is only valid for targets with push.mode: entry", false).
			WithAction("remove --include or submit through an entry-mode target")
	}
	result := make([]string, 0, len(target.Push.Include)+len(requested))
	seen := make(map[string]struct{}, cap(result))
	for _, pattern := range append(append([]string{}, target.Push.Include...), requested...) {
		pattern = filepath.ToSlash(strings.TrimSpace(pattern))
		if pattern == "" {
			return nil, fault.New("INVALID_ARGUMENT", "--include pattern cannot be empty", false)
		}
		if strings.HasPrefix(pattern, "/") || filepath.IsAbs(pattern) {
			return nil, fault.New("INVALID_ARGUMENT",
				fmt.Sprintf("--include pattern %q must be relative to the source work directory", pattern), false)
		}
		for _, segment := range strings.Split(pattern, "/") {
			if segment == ".." {
				return nil, fault.New("INVALID_ARGUMENT",
					fmt.Sprintf("--include pattern %q cannot traverse outside the source work directory", pattern), false)
			}
		}
		matchPattern := strings.TrimSuffix(pattern, "/")
		if matchPattern == "" {
			return nil, fault.New("INVALID_ARGUMENT", "--include pattern cannot select the work directory root", false)
		}
		if _, err := path.Match(matchPattern, "candidate"); err != nil {
			return nil, fault.Wrap("INVALID_ARGUMENT",
				fmt.Sprintf("invalid --include pattern %q", pattern), false, err)
		}
		if _, ok := seen[pattern]; ok {
			continue
		}
		seen[pattern] = struct{}{}
		result = append(result, pattern)
	}
	return result, nil
}

func validateRequestedIncludes(requested []string, entries []model.ManifestEntry) error {
	for _, pattern := range requested {
		matched := false
		for _, entry := range entries {
			if manifest.Match(entry.Path, []string{pattern}) {
				matched = true
				break
			}
		}
		if !matched {
			return fault.New("SOURCE_DEPENDENCY_NOT_FOUND",
				fmt.Sprintf("--include pattern %q matched no uploaded file", pattern), false).
				WithAction("check the filename and the target push.exclude and .joyrunignore rules")
		}
	}
	return nil
}

func validateSourceContract(src model.Source, localWorkDir, targetName string, target model.Target) error {
	if target.Source.Kind == "file" && src.Entry == nil {
		suggestion := filepath.ToSlash(filepath.Join(src.RelativePath, "<input-file>"))
		if candidate := singleSourceCandidate(localWorkDir, target.Source.Patterns); candidate != "" {
			suggestion = filepath.ToSlash(filepath.Join(src.RelativePath, candidate))
		}
		return fault.New("SOURCE_KIND_MISMATCH", fmt.Sprintf(
			"target %q requires a file source, but %q is a directory; try: joyrun submit %s -t %s",
			targetName, src.RelativePath, suggestion, targetName), false)
	}
	if target.Source.Kind == "directory" && src.Entry != nil {
		directory := src.WorkDir
		if directory == "" {
			directory = "."
		}
		return fault.New("SOURCE_KIND_MISMATCH", fmt.Sprintf(
			"target %q requires a directory source, but %q is a file; try: joyrun submit %s -t %s",
			targetName, src.RelativePath, directory, targetName), false)
	}
	if src.Entry != nil && len(target.Source.Patterns) > 0 {
		for _, pattern := range target.Source.Patterns {
			if matched, _ := path.Match(pattern, *src.Entry); matched {
				return nil
			}
		}
		return fault.New("SOURCE_PATTERN_MISMATCH", fmt.Sprintf(
			"source %q does not match target %q patterns: %s",
			*src.Entry, targetName, strings.Join(target.Source.Patterns, ", ")), false)
	}
	return nil
}

func singleSourceCandidate(workDir string, patterns []string) string {
	if len(patterns) == 0 {
		return ""
	}
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return ""
	}
	var candidate string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		for _, pattern := range patterns {
			if matched, _ := path.Match(pattern, name); matched {
				if candidate != "" {
					return ""
				}
				candidate = name
				break
			}
		}
	}
	return candidate
}

func (a *App) Submit(
	ctx context.Context,
	cwd, sourcePath, targetName string,
	sets []string,
	includes []string,
	partitionOverride string,
	allowProjectRoot bool,
	forceNewOption ...bool,
) (SubmitResult, error) {
	forceNew := len(forceNewOption) > 0 && forceNewOption[0]
	_, task, localWorkDir, err := a.prepare(
		ctx, cwd, sourcePath, targetName, sets, includes, partitionOverride, false, allowProjectRoot,
	)
	if err != nil {
		return SubmitResult{}, err
	}
	p, err := project.Discover(cwd)
	if err != nil {
		return SubmitResult{}, err
	}
	if err := a.Store.BindProject(ctx, p); err != nil {
		return SubmitResult{}, err
	}
	target := a.Config.Targets[task.TargetName]
	selection, err := uploadSelection(p.Root, localWorkDir, model.Source{Entry: task.SourceEntry}, target, includes)
	if err != nil {
		return SubmitResult{}, err
	}
	snapshotDir, inputManifest, _, cleanup, err := manifest.Snapshot(localWorkDir, selection)
	if err != nil {
		return SubmitResult{}, err
	}
	defer cleanup()
	if err := validateRequestedIncludes(includes, inputManifest); err != nil {
		return SubmitResult{}, err
	}
	task.InputManifest = inputManifest
	key, err := submissionKey(task)
	if err != nil {
		return SubmitResult{}, fault.Wrap("TASK_CREATE_FAILED", "cannot calculate submission key", false, err)
	}
	if task.Metadata == nil {
		task.Metadata = map[string]string{}
	}
	task.Metadata["submission_key"] = key
	if forceNew {
		if err := a.ensureForceNewSafe(ctx, task); err != nil {
			return SubmitResult{}, err
		}
	}
	existing, deduplicated, err := a.Store.CreateTaskIdempotent(ctx, &task, forceNew)
	if err != nil {
		return SubmitResult{}, err
	}
	if deduplicated {
		// A daemon crash can leave a reserved Task in `created` before its
		// remote metadata or scheduler submission is attempted. Reuse that
		// exact immutable reservation and continue the safe stages instead of
		// returning a permanently stuck Task or creating a second job.
		if existing.ComputeState != model.ComputeCreated {
			a.progress("Submission already exists; reusing task " + existing.ID)
			return SubmitResult{Task: existing, Deduplicated: true}, nil
		}
		task = existing
	}
	return a.submitPrepared(ctx, task, snapshotDir, inputManifest)
}

// ExecuteReservedSubmit executes a Task that was admitted by the daemon. It
// deliberately uses the frozen Task ID, rendered script, and input manifest;
// re-running preparation here would change templates that reference .TaskID
// or .RemoteDir and could defeat idempotency.
func (a *App) ExecuteReservedSubmit(ctx context.Context, taskID, snapshotDir string) (SubmitResult, error) {
	if a.Store == nil {
		return SubmitResult{}, fault.New("DATABASE_FAILED", "task store is required", false)
	}
	task, err := a.Store.GetTask(ctx, taskID)
	if err != nil {
		return SubmitResult{}, err
	}
	if task.ComputeState == model.ComputeSubmissionUncertain {
		return a.reconcileReservedSubmit(ctx, task)
	}
	if task.ComputeState != model.ComputeCreated {
		return SubmitResult{Task: task, Deduplicated: true}, nil
	}
	// A process can die after the remote sbatch command is accepted but before
	// the scheduler ID is persisted locally. The SUBMIT_STARTED event is the
	// durable fence: reconcile it before ever issuing another sbatch command.
	events, eventsErr := a.Store.Events(ctx, task.ID)
	if eventsErr != nil {
		return SubmitResult{}, eventsErr
	}
	for _, event := range events {
		if event.Type == "SUBMIT_STARTED" {
			return a.reconcileReservedSubmit(ctx, task)
		}
	}
	return a.submitPrepared(ctx, task, snapshotDir, task.InputManifest)
}

func (a *App) reconcileReservedSubmit(ctx context.Context, task model.Task) (SubmitResult, error) {
	cluster, ok := a.Config.Clusters[task.ClusterName]
	if !ok {
		return SubmitResult{}, fault.New("CLUSTER_NOT_FOUND",
			fmt.Sprintf("cluster %q is no longer configured", task.ClusterName), false)
	}
	recoveryCtx, cancelRecovery := a.recoveryContext(ctx)
	schedulerID, recoveryErr := a.recoverSchedulerID(recoveryCtx, cluster, task)
	cancelRecovery()
	if recoveryErr != nil {
		if task.ComputeState == model.ComputeSubmissionUncertain {
			return SubmitResult{}, fault.Wrap("SUBMISSION_UNCERTAIN",
				"cannot reconcile a possibly accepted Slurm submission", true, recoveryErr).
				WithTask("submit", "joyrun status "+task.ID, task.ComputeState, task.PullState)
		}
		return SubmitResult{}, a.failSubmissionUncertain(ctx, &task, "submit",
			"Slurm submission may have succeeded; reconciliation failed", recoveryErr)
	}
	if schedulerID == "" {
		cause := errors.New("no scheduler job matched the immutable JoyRun task marker")
		if task.ComputeState == model.ComputeSubmissionUncertain {
			return SubmitResult{}, fault.Wrap("SUBMISSION_UNCERTAIN",
				"Slurm submission remains unconfirmed; refusing to submit again", true, cause).
				WithTask("submit", "joyrun recover "+task.ID, task.ComputeState, task.PullState)
		}
		return SubmitResult{}, a.failSubmissionUncertain(ctx, &task, "submit",
			"Slurm submission may have succeeded; refusing to submit again without confirmation", cause)
	}
	now := time.Now().UTC()
	task.SchedulerID = schedulerID
	task.ComputeState = model.ComputeQueued
	task.SubmittedAt = &now
	task.UpdatedAt = now
	if err := a.Store.UpdateTaskWithEvent(ctx, &task, taskEvent(task, "SCHEDULER_ACCEPTED", "submit",
		"Recovered scheduler acceptance without resubmitting", map[string]string{"scheduler_id": schedulerID})); err != nil {
		return SubmitResult{}, err
	}
	return SubmitResult{Task: task}, nil
}

func (a *App) submitPrepared(ctx context.Context, task model.Task, snapshotDir string, inputManifest []model.ManifestEntry) (SubmitResult, error) {
	cluster := a.Config.Clusters[task.ClusterName]
	workDir := path.Join(task.RemoteDir, "work")
	if err := a.recordStage(ctx, &task, "UPLOAD_STARTED", "upload",
		"Uploading immutable input snapshot"); err != nil {
		return SubmitResult{}, err
	}
	a.progress("Creating remote task and uploading recovery metadata...")
	remoteCtx, cancelRemote := a.remoteContext(ctx)
	err := a.writeMetadata(remoteCtx, cluster, task)
	remoteFailure := operationFailure(remoteCtx, "REMOTE_METADATA_FAILED", "SSH_TIMEOUT")
	cancelRemote()
	if err != nil {
		return SubmitResult{}, a.failSubmission(ctx, &task, remoteFailure, "metadata",
			"cannot write recovery metadata", true, err)
	}
	if err := a.recordStage(ctx, &task, "REMOTE_DIR_CREATED", "remote_directory",
		"Remote task directory created"); err != nil {
		return SubmitResult{}, err
	}
	if err := a.recordStage(ctx, &task, "METADATA_WRITTEN", "metadata",
		"Recovery metadata uploaded"); err != nil {
		return SubmitResult{}, err
	}
	if err := os.WriteFile(filepath.Join(snapshotDir, "joyrun-job.sh"),
		[]byte(task.RenderedScript), 0o700); err != nil {
		return SubmitResult{}, a.failSubmission(ctx, &task, "SNAPSHOT_PREPARE_FAILED", "script",
			"cannot add rendered job script to immutable snapshot", false, err)
	}
	a.progress("Uploading %d input file(s) and the rendered job script...", len(inputManifest))
	if err := a.Transfer.Push(ctx, cluster, snapshotDir, workDir, nil); err != nil {
		return SubmitResult{}, a.failSubmission(ctx, &task, transferFailure(ctx, err), "snapshot",
			"cannot upload task files", true, err)
	}
	if err := a.recordStage(ctx, &task, "SNAPSHOT_UPLOADED", "snapshot",
		"Immutable input snapshot uploaded"); err != nil {
		return SubmitResult{}, err
	}
	if err := a.recordStage(ctx, &task, "SCRIPT_UPLOADED", "script",
		"Rendered job script uploaded with immutable snapshot"); err != nil {
		return SubmitResult{}, err
	}
	if err := a.recordStage(ctx, &task, "UPLOAD_COMPLETED", "upload",
		"Input snapshot and rendered script uploaded"); err != nil {
		return SubmitResult{}, err
	}
	if err := a.recordStage(ctx, &task, "SUBMIT_STARTED", "submit",
		"Submitting task to scheduler"); err != nil {
		return SubmitResult{}, err
	}
	a.progress("Submitting task to Slurm...")
	submitCtx, cancelSubmit := a.submitContext(ctx)
	schedulerID, err := a.scheduler().Submit(
		submitCtx, cluster.Host, workDir, task.ID, task.Metadata["partition"],
	)
	submitUncertain := submitCtx.Err() != nil
	cancelSubmit()
	if err != nil {
		if scheduler.SubmissionDefinitelyRejected(err) {
			return SubmitResult{}, a.failSubmission(ctx, &task, "SUBMIT_FAILED", "submit",
				"Slurm rejected the task before accepting a job", false, err)
		}
		// Submission may have succeeded even if the SSH connection dropped before
		// stdout arrived. Recover from the marker or the immutable Slurm comment.
		recoveryCtx, cancelRecovery := a.recoveryContext(ctx)
		recoveredID, recoveryErr := a.recoverSchedulerID(recoveryCtx, cluster, task)
		cancelRecovery()
		if recoveryErr != nil || recoveredID == "" {
			if submitUncertain || recoveryErr != nil {
				return SubmitResult{}, a.failSubmissionUncertain(ctx, &task, "submit",
					"Slurm submission may have succeeded, but its scheduler ID could not be recovered", err)
			}
			return SubmitResult{}, a.failSubmission(ctx, &task, "SUBMIT_FAILED", "submit",
				"cannot submit task to Slurm", true, err)
		}
		schedulerID = recoveredID
	}
	now := time.Now().UTC()
	task.SchedulerID = schedulerID
	task.ComputeState = model.ComputeQueued
	task.SubmittedAt = &now
	task.UpdatedAt = now
	persistCtx, cancelPersist := a.persistenceContext(ctx)
	err = a.Store.UpdateTaskWithEvent(persistCtx, &task, taskEvent(task, "SCHEDULER_ACCEPTED", "submit",
		"Scheduler accepted task", map[string]string{"scheduler_id": schedulerID}))
	cancelPersist()
	if err != nil {
		// The immutable metadata written before upload plus scheduler_id written
		// atomically by the submit command are sufficient for recovery.
		return SubmitResult{}, err
	}
	return SubmitResult{Task: task}, nil
}

// ensureForceNewSafe performs the remote check required before intentionally
// creating a second execution with the same immutable submission intent. A
// local terminal row is not sufficient: it may be stale or may have lost the
// scheduler response. Ambiguous state blocks the new run rather than risking
// duplicate Slurm work.
func (a *App) ensureForceNewSafe(ctx context.Context, candidate model.Task) error {
	history, err := a.Store.History(ctx, candidate.ProjectID, candidate.SourcePath)
	if err != nil {
		return fault.Wrap("SUBMISSION_SAFETY_UNCONFIRMED", "cannot inspect prior submissions before --force-new", true, err)
	}
	for _, prior := range history {
		if prior.Metadata["submission_key"] != candidate.Metadata["submission_key"] {
			continue
		}
		if prior.ComputeState == model.ComputeSubmissionFailed {
			continue
		}
		refreshed, refreshErr := a.refreshTask(ctx, prior)
		if refreshErr != nil {
			return fault.Wrap("SUBMISSION_SAFETY_UNCONFIRMED",
				"cannot remotely confirm the previous identical submission before --force-new", true, refreshErr).
				WithTask("submit", "joyrun status "+prior.ID, prior.ComputeState, prior.PullState)
		}
		prior = refreshed
		if prior.ComputeState == model.ComputeCreated ||
			prior.ComputeState == model.ComputeSubmissionUncertain ||
			prior.ComputeState == model.ComputeQueued ||
			prior.ComputeState == model.ComputeRunning ||
			prior.ComputeState == model.ComputeUnknown {
			return fault.New("SUBMISSION_SAFETY_UNCONFIRMED",
				"an identical submission is still active or uncertain; wait for remote terminal confirmation before --force-new", true).
				WithTask("submit", "joyrun status "+prior.ID, prior.ComputeState, prior.PullState)
		}
	}
	return nil
}

func (a *App) ResolveTask(ctx context.Context, cwd, identifier string) (model.Task, model.Project, error) {
	p, err := project.Discover(cwd)
	if err != nil {
		return model.Task{}, model.Project{}, err
	}
	if err := a.Store.BindProject(ctx, p); err != nil {
		return model.Task{}, p, err
	}
	if strings.HasPrefix(identifier, "jr_") {
		task, err := a.Store.GetTask(ctx, identifier)
		if err != nil {
			return task, p, err
		}
		if task.ProjectID != p.ProjectID {
			return model.Task{}, p, fault.New("TASK_PROJECT_MISMATCH", "task belongs to a different project", false)
		}
		return task, p, nil
	}
	src, _, err := source.Resolve(p, resolveFrom(cwd, identifier))
	if err != nil {
		return model.Task{}, p, err
	}
	task, err := a.Store.LatestTask(ctx, p.ProjectID, src.RelativePath)
	return task, p, err
}

func (a *App) List(ctx context.Context, cwd string) ([]model.Task, error) {
	p, err := project.Discover(cwd)
	if err != nil {
		return nil, err
	}
	if err := a.Store.BindProject(ctx, p); err != nil {
		return nil, err
	}
	return a.Store.ListTasks(ctx, p.ProjectID)
}

func (a *App) Inspect(ctx context.Context, cwd, identifier string) (model.Task, error) {
	task, _, err := a.ResolveTask(ctx, cwd, identifier)
	return task, err
}

func (a *App) Trace(ctx context.Context, cwd, identifier string) (TraceResult, error) {
	task, _, err := a.ResolveTask(ctx, cwd, identifier)
	if err != nil {
		return TraceResult{}, err
	}
	events, err := a.Store.Events(ctx, task.ID)
	if err != nil {
		return TraceResult{}, err
	}
	return TraceResult{Task: task, Events: events}, nil
}

func (a *App) StatusAll(ctx context.Context, cwd string) StatusAllResult {
	tasks, err := a.List(ctx, cwd)
	if err != nil {
		return StatusAllResult{Failures: []StatusFailure{{Error: fault.As(err)}}}
	}
	result := StatusAllResult{Tasks: make([]model.Task, 0, len(tasks))}
	resolved := make(map[string]model.Task, len(tasks))
	type clusterBatch struct {
		cluster model.Cluster
		tasks   []model.Task
	}
	batches := map[string]*clusterBatch{}
	for _, task := range tasks {
		// A missing scheduler ID requires reconciliation, which can be
		// expensive and ambiguous for legacy failed records. Keep bulk status
		// bounded; exact `status TASK` remains the recovery path.
		if !bulkRefreshableComputeState(task.ComputeState) {
			resolved[task.ID] = task
			continue
		}
		if task.SchedulerID == "" {
			if task.ComputeState == model.ComputeSubmissionUncertain {
				updated, refreshErr := a.refreshTask(ctx, task)
				if refreshErr != nil {
					result.Failures = append(result.Failures, StatusFailure{TaskID: task.ID, Error: fault.As(refreshErr)})
				} else {
					resolved[task.ID] = updated
				}
			} else {
				resolved[task.ID] = task
			}
			continue
		}
		cluster, ok := a.Config.Clusters[task.ClusterName]
		if !ok {
			result.Failures = append(result.Failures, StatusFailure{
				TaskID: task.ID,
				Error: fault.New("CLUSTER_NOT_FOUND",
					fmt.Sprintf("cluster %q is no longer configured", task.ClusterName), false),
			})
			continue
		}
		batch := batches[task.ClusterName]
		if batch == nil {
			batch = &clusterBatch{cluster: cluster}
			batches[task.ClusterName] = batch
		}
		batch.tasks = append(batch.tasks, task)
	}
	for _, batch := range batches {
		ids := make([]string, 0, len(batch.tasks))
		for _, task := range batch.tasks {
			ids = append(ids, task.SchedulerID)
		}
		statuses, err := a.scheduler().Statuses(
			ctx, batch.cluster.Host, ids)
		if err != nil {
			for _, task := range batch.tasks {
				a.markSchedulerStale(ctx, &task)
				result.Failures = append(result.Failures, StatusFailure{
					TaskID: task.ID, Error: fault.As(err),
				})
			}
			continue
		}
		for _, task := range batch.tasks {
			updated, err := a.applySchedulerStatus(ctx, task, statuses[task.SchedulerID])
			if err != nil {
				a.markSchedulerStale(ctx, &task)
				result.Failures = append(result.Failures, StatusFailure{
					TaskID: task.ID, Error: fault.As(err),
				})
				continue
			}
			resolved[task.ID] = updated
		}
	}
	for _, task := range tasks {
		if updated, ok := resolved[task.ID]; ok {
			result.Tasks = append(result.Tasks, updated)
		}
	}
	return result
}

func (a *App) Status(ctx context.Context, cwd, identifier string) (model.Task, error) {
	task, _, err := a.ResolveTask(ctx, cwd, identifier)
	if err != nil {
		return task, err
	}
	return a.refreshTask(ctx, task)
}

func (a *App) refreshTask(ctx context.Context, task model.Task) (model.Task, error) {
	cluster, ok := a.Config.Clusters[task.ClusterName]
	if !ok {
		return task, fault.New("CLUSTER_NOT_FOUND",
			fmt.Sprintf("cluster %q is no longer configured", task.ClusterName), false).
			WithTask("status", "restore cluster configuration, then run joyrun status "+task.ID,
				task.ComputeState, task.PullState)
	}
	if task.SchedulerID == "" {
		schedulerID, err := a.recoverSchedulerID(ctx, cluster, task)
		if err != nil {
			a.markSchedulerStale(ctx, &task)
			return task, fault.As(err).
				WithTask("status", "joyrun status "+task.ID, task.ComputeState, task.PullState)
		}
		if schedulerID == "" {
			return task, nil
		}
		task.SchedulerID = schedulerID
		task.ComputeState = model.ComputeQueued
		task.UpdatedAt = time.Now().UTC()
		if err := a.Store.UpdateTaskWithEvent(ctx, &task, taskEvent(task, "SCHEDULER_ID_RECOVERED",
			"status", "Recovered scheduler ID from remote marker or Slurm task comment",
			map[string]string{"scheduler_id": task.SchedulerID})); err != nil {
			return task, err
		}
	}
	status, err := a.scheduler().Status(ctx, cluster.Host, task.SchedulerID)
	if err != nil {
		a.markSchedulerStale(ctx, &task)
		return task, fault.As(err).
			WithTask("status", "joyrun status "+task.ID, task.ComputeState, task.PullState)
	}
	return a.applySchedulerStatus(ctx, task, status)
}

func (a *App) markSchedulerStale(ctx context.Context, task *model.Task) {
	if task == nil || a.Store == nil {
		return
	}
	if task.Metadata == nil {
		task.Metadata = map[string]string{}
	}
	if task.Metadata["scheduler_stale_since"] == "" {
		task.Metadata["scheduler_stale_since"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if task.Metadata["scheduler_observation"] == "" {
		task.Metadata["scheduler_observation"] = "cache"
	}
	task.UpdatedAt = time.Now().UTC()
	_ = a.Store.UpdateTask(ctx, task)
}

func (a *App) applySchedulerStatus(
	ctx context.Context,
	task model.Task,
	status scheduler.JobStatus,
) (model.Task, error) {
	now := time.Now().UTC()
	if task.Metadata == nil {
		task.Metadata = map[string]string{}
	}
	previousObservedAt := task.Metadata["scheduler_observed_at"]
	task.Metadata["scheduler_observation"] = "remote"
	task.Metadata["scheduler_observed_at"] = now.Format(time.RFC3339Nano)
	delete(task.Metadata, "scheduler_stale_since")
	previousCompute := task.ComputeState
	previousRaw := task.SchedulerState
	previousReason, previousElapsed, previousExitCode :=
		task.SchedulerReason, task.Elapsed, task.ExitCode
	previousStart, previousEnd := task.SchedulerStart, task.SchedulerEnd
	if terminalComputeState(previousCompute) && !terminalComputeState(status.State) {
		// A scheduler may purge old accounting records. Never erase a terminal
		// state that JoyRun has already observed.
		status.State = previousCompute
		if status.RawState == "" {
			status.RawState = previousRaw
			status.Reason = previousReason
			status.Elapsed = previousElapsed
			status.ExitCode = previousExitCode
			status.Start = previousStart
			status.End = previousEnd
		}
	}
	task.ComputeState = status.State
	task.SchedulerState = status.RawState
	task.SchedulerReason = status.Reason
	task.Elapsed = status.Elapsed
	task.ExitCode = status.ExitCode
	task.SchedulerStart = status.Start
	task.SchedulerEnd = status.End
	if previousCompute == task.ComputeState &&
		previousRaw == task.SchedulerState &&
		previousReason == task.SchedulerReason &&
		previousExitCode == task.ExitCode &&
		previousStart == task.SchedulerStart &&
		previousEnd == task.SchedulerEnd {
		// Elapsed time changes on every poll. Persist a freshness timestamp
		// without adding a lifecycle event, but avoid rewriting the row when a
		// caller immediately asks for the same observation again.
		if observed, err := time.Parse(time.RFC3339Nano, previousObservedAt); err == nil &&
			time.Since(observed) < 5*time.Second {
			return task, nil
		}
		task.UpdatedAt = now
		if err := a.Store.UpdateTask(ctx, &task); err != nil {
			return task, err
		}
		return task, nil
	}
	task.UpdatedAt = now
	eventType := "SCHEDULER_STATUS_CHANGED"
	eventMessage := "Scheduler diagnostics refreshed"
	if previousCompute != task.ComputeState || previousRaw != task.SchedulerState {
		eventType = "COMPUTE_STATE_CHANGED"
		eventMessage = "Scheduler state refreshed"
	}
	event := taskEvent(task, eventType, "status",
		eventMessage, map[string]string{
			"previous_compute_state": previousCompute,
			"compute_state":          task.ComputeState,
			"scheduler_state":        task.SchedulerState,
			"scheduler_reason":       task.SchedulerReason,
			"elapsed":                task.Elapsed,
			"exit_code":              task.ExitCode,
			"scheduler_start":        task.SchedulerStart,
			"scheduler_end":          task.SchedulerEnd,
		})
	if err := a.Store.UpdateTaskWithEvent(ctx, &task, event); err != nil {
		return task, err
	}
	return task, nil
}

func (a *App) recoverSchedulerID(
	ctx context.Context,
	cluster model.Cluster,
	task model.Task,
) (string, error) {
	marker, markerErr := remote.ReadFile(ctx, a.Runner, cluster.Host,
		path.Join(task.RemoteDir, "scheduler_id"))
	if markerErr == nil {
		if id := strings.TrimSpace(string(marker)); id != "" {
			if _, err := strconv.ParseUint(id, 10, 64); err == nil {
				return id, nil
			}
		}
	}
	id, err := a.scheduler().FindByTaskID(
		ctx, cluster.Host, task.ID, task.CreatedAt.Add(-time.Minute))
	if err != nil {
		return "", err
	}
	return id, nil
}

func (a *App) Cancel(ctx context.Context, cwd, identifier string) (model.Task, error) {
	if !strings.HasPrefix(identifier, "jr_") {
		return model.Task{}, fault.New("CANCEL_REQUIRES_TASK_ID",
			"cancel requires an exact task ID; inspect the source first, then run `joyrun cancel <task-id>`", false)
	}
	task, _, err := a.ResolveTask(ctx, cwd, identifier)
	if err != nil {
		return task, err
	}
	if task.SchedulerID == "" {
		return task, fault.New("CANCEL_FAILED", "task has no scheduler ID", false).
			WithTask("cancel", "joyrun status "+task.ID, task.ComputeState, task.PullState)
	}
	cluster, ok := a.Config.Clusters[task.ClusterName]
	if !ok {
		return task, fault.New("CLUSTER_NOT_FOUND",
			fmt.Sprintf("cluster %q is no longer configured", task.ClusterName), false)
	}
	if err := a.Store.AppendEvent(ctx, taskEvent(task, "CANCEL_REQUESTED", "cancel",
		"Cancellation requested", nil)); err != nil {
		return task, err
	}
	if err := a.scheduler().Cancel(ctx, cluster.Host, task.SchedulerID); err != nil {
		return task, fault.Wrap("CANCEL_FAILED", "cannot cancel scheduler job", true, err).
			WithTask("cancel", "joyrun status "+task.ID, task.ComputeState, task.PullState)
	}
	if task.Metadata == nil {
		task.Metadata = map[string]string{}
	}
	task.Metadata["cancel_requested_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	task.UpdatedAt = time.Now().UTC()
	if err := a.Store.UpdateTaskWithEvent(ctx, &task, taskEvent(task, "CANCEL_ACCEPTED", "cancel",
		"Scheduler accepted cancellation request; run status to observe the terminal state", nil)); err != nil {
		return task, err
	}
	_ = a.writeMetadata(ctx, cluster, task)
	return task, nil
}

func (a *App) Logs(ctx context.Context, cwd, identifier string, lines int, requested string) (LogResult, error) {
	task, _, err := a.ResolveTask(ctx, cwd, identifier)
	if err != nil {
		return LogResult{}, err
	}
	cluster, ok := a.Config.Clusters[task.ClusterName]
	if !ok {
		return LogResult{}, fault.New("CLUSTER_NOT_FOUND",
			fmt.Sprintf("cluster %q is no longer configured", task.ClusterName), false).
			WithTask("logs", "restore cluster configuration, then run joyrun logs "+task.ID,
				task.ComputeState, task.PullState)
	}
	type candidate struct {
		path string
		kind string
	}
	var candidates []candidate
	if requested != "" {
		if !safeTaskRelative(requested, true) {
			return LogResult{}, fault.New("INVALID_ARGUMENT",
				"log path must be relative to the remote task work directory", false)
		}
		candidates = append(candidates, candidate{path: requested, kind: "requested"})
	} else {
		for _, logPath := range task.Logs {
			if logPath != "" {
				candidates = append(candidates, candidate{path: logPath, kind: "application"})
			}
		}
		if task.SchedulerID != "" {
			candidates = append(candidates,
				candidate{path: scheduler.LogName(task.SchedulerID), kind: "scheduler"},
				candidate{path: "slurm-" + task.SchedulerID + ".out", kind: "scheduler_legacy"})
		}
	}
	checked := make([]string, 0, len(candidates))
	workDir := path.Join(task.RemoteDir, "work")
	var command strings.Builder
	command.WriteString("cd ")
	command.WriteString(remote.Quote(workDir))
	command.WriteString(" || exit 1; ")
	for _, item := range candidates {
		checked = append(checked, item.path)
	}
	if len(checked) == 0 {
		return LogResult{}, fault.New("LOG_NOT_READY", "task has no application or scheduler log candidates yet", true).
			WithTask("logs", "joyrun status "+task.ID, task.ComputeState, task.PullState)
	}
	for index, item := range candidates {
		fmt.Fprintf(&command,
			"if test -f %s; then printf 'JOYRUN_LOG_FOUND|%d\\n'; tail -n %d -- %s; exit 0; fi; ",
			remote.Quote(item.path), index, lines, remote.Quote(item.path))
	}
	command.WriteString("printf 'JOYRUN_LOG_MISSING\\n'")
	stdout, stderr, err := a.Runner.Exec(ctx, cluster.Host, command.String(), nil)
	if err != nil {
		return LogResult{}, fault.Wrap("LOG_FAILED",
			message("cannot inspect remote logs", stderr), true, err).
			WithTask("logs", "joyrun logs "+task.ID, task.ComputeState, task.PullState)
	}
	status, content, _ := strings.Cut(stdout, "\n")
	if strings.HasPrefix(status, "JOYRUN_LOG_FOUND|") {
		index, parseErr := strconv.Atoi(strings.TrimPrefix(status, "JOYRUN_LOG_FOUND|"))
		if parseErr == nil && index >= 0 && index < len(candidates) {
			item := candidates[index]
			return LogResult{TaskID: task.ID, Path: item.path, Kind: item.kind, Content: content}, nil
		}
	}
	if status != "JOYRUN_LOG_MISSING" {
		return LogResult{}, fault.New("LOG_FAILED",
			"unexpected response while inspecting remote logs", true).
			WithTask("logs", "joyrun logs "+task.ID, task.ComputeState, task.PullState)
	}
	return LogResult{}, fault.New("LOG_NOT_READY",
		"none of the expected remote logs exist yet; checked: "+strings.Join(checked, ", "), true).
		WithTask("logs", "joyrun status "+task.ID, task.ComputeState, task.PullState)
}

func (a *App) Pull(ctx context.Context, cwd, identifier string, options PullOptions) (PullResult, error) {
	task, p, err := a.ResolveTask(ctx, cwd, identifier)
	if err != nil {
		return PullResult{}, err
	}
	if !options.Live && !terminalComputeState(task.ComputeState) {
		task, err = a.Status(ctx, cwd, task.ID)
		if err != nil {
			return PullResult{}, err
		}
		if !terminalComputeState(task.ComputeState) {
			return PullResult{}, fault.New("JOB_NOT_COMPLETED",
				fmt.Sprintf("task compute state is %s; use --live to pull available files", task.ComputeState), false).
				WithTask("pull", "joyrun status "+task.ID, task.ComputeState, task.PullState)
		}
	}
	cluster, ok := a.Config.Clusters[task.ClusterName]
	if !ok {
		return PullResult{}, fault.New("CLUSTER_NOT_FOUND",
			fmt.Sprintf("cluster %q is no longer configured", task.ClusterName), false)
	}
	workDir := path.Join(task.RemoteDir, "work")
	remoteFiles, err := a.listRemoteFiles(ctx, cluster, workDir)
	if err != nil {
		if options.DryRun {
			return PullResult{}, fault.As(err).
				WithTask("pull", "joyrun pull "+task.ID+" --dry-run",
					task.ComputeState, task.PullState)
		}
		return PullResult{}, a.failPull(ctx, &task, "list", "cannot list remote task files", err)
	}
	patterns := task.PullPatterns
	if len(options.Include) > 0 {
		patterns = options.Include
	}
	inputs := make(map[string]bool, len(task.InputManifest))
	for _, entry := range task.InputManifest {
		inputs[entry.Path] = true
	}
	var files []string
	var totalBytes int64
	remoteSizes := make(map[string]int64)
	for _, remoteFile := range remoteFiles {
		file := filepath.ToSlash(remoteFile.Path)
		if file == "" || file == "joyrun-job.sh" {
			continue
		}
		if !options.All && !manifest.Match(file, patterns) {
			continue
		}
		if inputs[file] && !options.OverwriteInputs {
			continue
		}
		files = append(files, file)
		remoteSizes[file] = remoteFile.Size
		totalBytes += remoteFile.Size
	}
	sort.Strings(files)
	if len(files) == 0 {
		return PullResult{}, fault.New("NO_FILES_MATCHED",
			"no remote files matched the requested pull policy", false).
			WithTask("pull", "adjust --include or use --all for task "+task.ID,
				task.ComputeState, task.PullState)
	}
	destination := filepath.Join(p.Root, filepath.FromSlash(task.SourceWorkDir))
	if err := localfs.ValidatePullDestination(destination, files); err != nil {
		return PullResult{}, fault.As(err).
			WithTask("pull", "joyrun inspect "+task.ID, task.ComputeState, task.PullState)
	}
	if options.DryRun {
		return PullResult{
			Task: task, Files: files, Destination: destination,
			TotalBytes: totalBytes, DryRun: true,
		}, nil
	}
	task.PullState = model.PullInProgress
	task.UpdatedAt = time.Now().UTC()
	persistCtx, cancelPersist := a.persistenceContext(ctx)
	err = a.Store.UpdateTaskWithEvent(persistCtx, &task, taskEvent(task, "PULL_STARTED", "pull",
		"Selected file transfer started", map[string]string{"destination": destination}))
	cancelPersist()
	if err != nil {
		return PullResult{}, err
	}
	staging := filepath.Join(filepath.Dir(destination), ".joyrun-pull-"+task.ID)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return PullResult{}, a.failPull(ctx, &task, "staging", "cannot create local pull staging directory", err)
	}
	if err := a.Transfer.Pull(ctx, cluster, workDir, staging, files); err != nil {
		return PullResult{}, a.failPull(ctx, &task, "transfer", "cannot pull selected task files", err)
	}
	for _, file := range files {
		sourcePath := filepath.Join(staging, filepath.FromSlash(file))
		destinationPath := filepath.Join(destination, filepath.FromSlash(file))
		info, statErr := os.Stat(sourcePath)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != remoteSizes[file] {
			if statErr == nil {
				statErr = fmt.Errorf("size mismatch: got %d bytes, expected %d", info.Size(), remoteSizes[file])
			}
			return PullResult{}, a.failPull(ctx, &task, "verify", "cannot verify pulled file "+file, statErr)
		}
		if err := localfs.InstallStagedFile(sourcePath, destinationPath); err != nil {
			return PullResult{}, a.failPull(ctx, &task, "install", "cannot install pulled file "+file, err)
		}
	}
	_ = os.RemoveAll(staging)
	now := time.Now().UTC()
	task.PulledAt, task.UpdatedAt = &now, now
	if terminalComputeState(task.ComputeState) {
		task.PullState = model.PullSucceeded
	} else {
		task.PullState = model.PullPartial
	}
	persistCtx, cancelPersist = a.persistenceContext(ctx)
	err = a.Store.UpdateTaskWithEvent(persistCtx, &task, taskEvent(task, "PULL_COMPLETED", "pull",
		"Selected file transfer completed", map[string]string{"files": fmt.Sprintf("%d", len(files))}))
	cancelPersist()
	if err != nil {
		return PullResult{}, err
	}
	return PullResult{
		Task: task, Files: files, Destination: destination,
		TotalBytes: totalBytes,
	}, nil
}

func (a *App) RemoteFiles(ctx context.Context, cwd, identifier string) ([]RemoteFile, error) {
	task, _, err := a.ResolveTask(ctx, cwd, identifier)
	if err != nil {
		return nil, err
	}
	cluster, ok := a.Config.Clusters[task.ClusterName]
	if !ok {
		return nil, fault.New("CLUSTER_NOT_FOUND",
			fmt.Sprintf("cluster %q is no longer configured", task.ClusterName), false).
			WithTask("files", "restore cluster configuration, then run joyrun files "+task.ID,
				task.ComputeState, task.PullState)
	}
	files, err := a.listRemoteFiles(ctx, cluster, path.Join(task.RemoteDir, "work"))
	if err != nil {
		return nil, fault.As(err).
			WithTask("files", "joyrun files "+task.ID, task.ComputeState, task.PullState)
	}
	inputs := make(map[string]bool, len(task.InputManifest))
	for _, entry := range task.InputManifest {
		inputs[entry.Path] = true
	}
	result := make([]RemoteFile, 0, len(files))
	for _, file := range files {
		if file.Path == "joyrun-job.sh" {
			continue
		}
		file.Input = inputs[file.Path]
		result = append(result, file)
	}
	return result, nil
}

func (a *App) listRemoteFiles(
	ctx context.Context,
	cluster model.Cluster,
	workDir string,
) ([]RemoteFile, error) {
	command := "cd " + remote.Quote(workDir) + " && find . -type f -printf '%P\\0%s\\0'"
	stdout, stderr, err := a.Runner.Exec(ctx, cluster.Host, command, nil)
	if err != nil {
		return nil, fault.Wrap("REMOTE_LIST_FAILED",
			message("cannot list remote task files", stderr), true, err)
	}
	records := strings.Split(stdout, "\x00")
	var files []RemoteFile
	for index := 0; index < len(records); {
		name := records[index]
		index++
		if name == "" {
			continue
		}
		var size int64
		if index < len(records) && records[index] != "" {
			rawSize := records[index]
			index++
			size, err = strconv.ParseInt(rawSize, 10, 64)
			if err != nil || size < 0 {
				return nil, fault.New("REMOTE_LIST_FAILED",
					"remote file listing returned an invalid size", true)
			}
		}
		files = append(files, RemoteFile{Path: filepath.ToSlash(name), Size: size})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func (a *App) History(ctx context.Context, cwd, sourcePath string) ([]model.Task, error) {
	p, err := project.Discover(cwd)
	if err != nil {
		return nil, err
	}
	if err := a.Store.BindProject(ctx, p); err != nil {
		return nil, err
	}
	src, _, err := source.Resolve(p, resolveFrom(cwd, sourcePath))
	if err != nil {
		return nil, err
	}
	return a.Store.History(ctx, p.ProjectID, src.RelativePath)
}

func (a *App) Doctor(ctx context.Context, targetName string) DoctorResult {
	var checks []DoctorCheck
	target, ok := a.Config.Targets[targetName]
	checks = append(checks, doctorCheck("target", ok, targetName,
		"configure target "+targetName+" in the JoyRun config"))
	if !ok {
		return doctorResult(checks)
	}
	cluster, ok := a.Config.Clusters[target.Cluster]
	checks = append(checks, doctorCheck("cluster", ok, target.Cluster,
		"configure cluster "+target.Cluster+" in the JoyRun config"))
	if !ok {
		return doctorResult(checks)
	}
	backend := ""
	var backendErr error
	if resolver, ok := a.Transfer.(interface {
		Backend(model.Cluster) (string, error)
	}); ok {
		backend, backendErr = resolver.Backend(cluster)
	}
	executables := []string{"sbatch", "squeue", "sacct", "scancel"}
	if backend == "rsync" {
		executables = append(executables, "rsync")
	}
	command := ": JOYRUN_DOCTOR; root=" + remote.Quote(cluster.RemoteRoot) + "; " +
		"if [ -d \"$root\" ]; then " +
		"if [ -w \"$root\" ]; then root_result=pass; else root_result=not_writable; fi; " +
		"elif [ -e \"$root\" ]; then root_result=not_directory; " +
		"else ancestor=$(dirname -- \"$root\"); " +
		"while [ ! -e \"$ancestor\" ] && [ \"$ancestor\" != / ]; do ancestor=$(dirname -- \"$ancestor\"); done; " +
		"if [ -d \"$ancestor\" ] && [ -w \"$ancestor\" ]; then root_result=creatable:$ancestor; " +
		"else root_result=not_creatable:$ancestor; fi; fi; " +
		"printf 'remote_root|%s\\n' \"$root_result\"; "
	for _, executable := range executables {
		command += "if command -v " + executable + " >/dev/null 2>&1; then " +
			"printf " + remote.Quote(executable+"|pass\n") + "; else " +
			"printf " + remote.Quote(executable+"|missing\n") + "; fi; "
	}
	stdout, stderr, err := a.Runner.Exec(ctx, cluster.Host, command, nil)
	if err != nil {
		checks = append(checks, DoctorCheck{
			Name: "ssh", Status: "fail", OK: false, Blocking: true,
			Message:         message("cannot run remote checks", stderr),
			SuggestedAction: "verify that `ssh " + cluster.Host + "` works non-interactively",
		})
		return doctorResult(checks)
	}
	checks = append(checks, DoctorCheck{
		Name: "ssh", Status: "pass", OK: true, Message: cluster.Host,
	})
	remoteChecks := map[string]string{}
	for _, line := range strings.Split(stdout, "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "|")
		if ok {
			remoteChecks[name] = value
		}
	}
	checks = append(checks, remoteRootDoctorCheck(
		target.Cluster, cluster, remoteChecks["remote_root"]))
	for _, executable := range []string{"sbatch", "squeue", "sacct", "scancel"} {
		available := remoteChecks[executable] == "pass"
		detail := "available"
		if !available {
			detail = executable + " was not found on " + cluster.Host
		}
		checks = append(checks, doctorCheck(executable, available, detail,
			"load or install Slurm so `command -v "+executable+"` succeeds"))
	}
	var transferErr error
	inspector := a.TransferInspector
	if inspector == nil {
		if candidate, ok := a.Transfer.(TransferInspector); ok {
			inspector = candidate
		}
	}
	if backendErr != nil {
		transferErr = backendErr
	} else if backend == "rsync" {
		if remoteChecks["rsync"] != "pass" {
			transferErr = fault.New("TRANSFER_UNAVAILABLE",
				"rsync is not installed on the remote cluster", false)
		}
	} else if inspector == nil {
		transferErr = fault.New("TRANSFER_UNAVAILABLE", "selected transfer backend does not support diagnostics", false)
	} else {
		backend, transferErr = inspector.Check(ctx, cluster)
	}
	detail := errorMessage(transferErr)
	if detail == "" {
		detail = backend + " is available"
	}
	checks = append(checks, doctorCheck("transfer_"+backend, transferErr == nil, detail,
		"install/configure the selected transfer backend or use transfer: auto"))
	return doctorResult(checks)
}

func remoteRootDoctorCheck(clusterName string, cluster model.Cluster, result string) DoctorCheck {
	result = strings.TrimSpace(result)
	switch {
	case result == "pass":
		return DoctorCheck{
			Name: "remote_root", Status: "pass", OK: true,
			Message: cluster.RemoteRoot + " exists and is writable",
		}
	case strings.HasPrefix(result, "creatable:"):
		ancestor := strings.TrimPrefix(result, "creatable:")
		return DoctorCheck{
			Name: "remote_root", Status: "warn", OK: true,
			Message: cluster.RemoteRoot + " does not exist; JoyRun will create it on first submit (writable ancestor: " + ancestor + ")",
		}
	case result == "not_directory":
		return DoctorCheck{
			Name: "remote_root", Status: "fail", OK: false, Blocking: true,
			Message:         cluster.RemoteRoot + " exists but is not a directory",
			SuggestedAction: "change clusters." + clusterName + ".remote_root to a directory",
		}
	case result == "not_writable":
		return DoctorCheck{
			Name: "remote_root", Status: "fail", OK: false, Blocking: true,
			Message:         cluster.RemoteRoot + " exists but is not writable",
			SuggestedAction: "choose a writable remote_root or fix its permissions on " + cluster.Host,
		}
	default:
		ancestor := strings.TrimPrefix(result, "not_creatable:")
		return DoctorCheck{
			Name: "remote_root", Status: "fail", OK: false, Blocking: true,
			Message:         cluster.RemoteRoot + " does not exist and cannot be created from " + ancestor,
			SuggestedAction: "choose a writable remote_root or run `ssh " + cluster.Host + " 'mkdir -p " + cluster.RemoteRoot + "'`",
		}
	}
}

func doctorCheck(name string, ok bool, message, action string) DoctorCheck {
	if ok {
		return DoctorCheck{Name: name, Status: "pass", OK: true, Message: message}
	}
	return DoctorCheck{
		Name: name, Status: "fail", OK: false, Blocking: true,
		Message: message, SuggestedAction: action,
	}
}

func doctorResult(checks []DoctorCheck) DoctorResult {
	ready := true
	for _, check := range checks {
		if check.Blocking && check.Status == "fail" {
			ready = false
		}
	}
	return DoctorResult{Ready: ready, Checks: checks}
}

func (a *App) Recover(ctx context.Context, cwd, taskID, targetName string) (model.Task, error) {
	p, err := project.Discover(cwd)
	if err != nil {
		return model.Task{}, err
	}
	if err := a.Store.BindProject(ctx, p); err != nil {
		return model.Task{}, err
	}
	target, ok := a.Config.Targets[targetName]
	if !ok {
		return model.Task{}, fault.New("TARGET_NOT_FOUND", fmt.Sprintf("target %q not found", targetName), false)
	}
	cluster := a.Config.Clusters[target.Cluster]
	data, err := remote.ReadFile(ctx, a.Runner, cluster.Host, path.Join(cluster.RemoteRoot, taskID, "metadata.json"))
	if err != nil {
		return model.Task{}, err
	}
	var task model.Task
	if err := json.Unmarshal(data, &task); err != nil {
		return task, fault.Wrap("RECOVERY_FAILED", "invalid remote task metadata", false, err)
	}
	if task.ID != taskID || task.ProjectID != p.ProjectID {
		return task, fault.New("RECOVERY_FAILED", "remote metadata does not match task or current project", false)
	}
	if task.Metadata["recovery_format"] != "1" {
		return task, fault.New("RECOVERY_FAILED", "unsupported or missing remote recovery metadata format", false)
	}
	expectedRemoteDir := path.Join(cluster.RemoteRoot, taskID)
	if task.TargetName != targetName || task.ClusterName != target.Cluster ||
		path.Clean(task.RemoteDir) != expectedRemoteDir {
		return task, fault.New("RECOVERY_FAILED",
			"remote metadata target, cluster, or remote directory does not match the recovery location", false)
	}
	if !safeTaskRelative(task.SourcePath, true) ||
		!safeTaskRelative(task.SourceWorkDir, false) {
		return task, fault.New("RECOVERY_FAILED", "remote metadata contains an unsafe source path", false)
	}
	if task.SourceEntry != nil &&
		(*task.SourceEntry == "" || strings.ContainsAny(*task.SourceEntry, "\\\x00") ||
			path.Base(*task.SourceEntry) != *task.SourceEntry) {
		return task, fault.New("RECOVERY_FAILED", "remote metadata contains an unsafe source entry", false)
	}
	if task.ComputeState == "" {
		task.ComputeState = model.ComputeCreated
	}
	if task.PullState == "" {
		task.PullState = model.PullNotPulled
	}
	if task.SchedulerID != "" {
		if _, err := strconv.ParseUint(task.SchedulerID, 10, 64); err != nil {
			return task, fault.New("RECOVERY_FAILED", "remote metadata contains an invalid scheduler ID", false)
		}
	}
	if task.SchedulerID == "" {
		task.SchedulerID, err = a.recoverSchedulerID(ctx, cluster, task)
		if err != nil {
			return task, fault.As(err).WithTask(
				"recovery", "verify Slurm accounting and retry recovery",
				task.ComputeState, task.PullState)
		}
		if task.SchedulerID != "" &&
			(task.ComputeState == model.ComputeCreated ||
				task.ComputeState == model.ComputeSubmissionFailed ||
				task.ComputeState == model.ComputeSubmissionUncertain) {
			task.ComputeState = model.ComputeQueued
		}
	}
	if err := a.Store.ImportTask(ctx, &task); err != nil {
		return task, err
	}
	return a.refreshTask(ctx, task)
}

func (a *App) RecoveryCandidates(
	ctx context.Context,
	cwd, targetName string,
) ([]RecoveryCandidate, error) {
	p, err := project.Discover(cwd)
	if err != nil {
		return nil, err
	}
	target, ok := a.Config.Targets[targetName]
	if !ok {
		return nil, fault.New("TARGET_NOT_FOUND", fmt.Sprintf("target %q not found", targetName), false)
	}
	cluster := a.Config.Clusters[target.Cluster]
	command := "find " + remote.Quote(cluster.RemoteRoot) +
		" -mindepth 2 -maxdepth 2 -type f -name metadata.json -exec sh -c " +
		remote.Quote("for file do directory=${file%/metadata.json}; printf '%s\\0' \"$directory\"; cat -- \"$file\"; printf '\\0'; done") +
		" joyrun-recovery {} +"
	stdout, stderr, err := a.Runner.Exec(ctx, cluster.Host, command, nil)
	if err != nil {
		return nil, fault.Wrap("RECOVERY_SCAN_FAILED",
			message("cannot scan remote JoyRun metadata", stderr), true, err)
	}
	seen := map[string]bool{}
	var candidates []RecoveryCandidate
	records := strings.Split(stdout, "\x00")
	for index := 0; index+1 < len(records); index += 2 {
		directory := records[index]
		data := []byte(records[index+1])
		taskID := path.Base(directory)
		if taskID == "" || !strings.HasPrefix(taskID, "jr_") || seen[taskID] {
			continue
		}
		seen[taskID] = true
		var task model.Task
		if json.Unmarshal(data, &task) != nil ||
			task.ID != taskID ||
			task.ProjectID != p.ProjectID ||
			task.TargetName != targetName ||
			task.ClusterName != target.Cluster ||
			path.Clean(task.RemoteDir) != path.Join(cluster.RemoteRoot, taskID) ||
			!safeTaskRelative(task.SourcePath, true) ||
			!safeTaskRelative(task.SourceWorkDir, false) ||
			task.Metadata["recovery_format"] != "1" {
			continue
		}
		candidates = append(candidates, RecoveryCandidate{
			TaskID: task.ID, SourcePath: task.SourcePath,
			ComputeState: task.ComputeState, UpdatedAt: task.UpdatedAt,
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].UpdatedAt.After(candidates[j].UpdatedAt)
	})
	return candidates, nil
}

func safeTaskRelative(value string, allowDot bool) bool {
	if strings.ContainsAny(value, "\\\x00") {
		return false
	}
	if value == "" {
		return !allowDot
	}
	cleaned := path.Clean(value)
	if allowDot && cleaned == "." {
		return value == "."
	}
	return cleaned == value && !path.IsAbs(cleaned) && cleaned != ".." &&
		!strings.HasPrefix(cleaned, "../")
}

func (a *App) writeMetadata(ctx context.Context, cluster model.Cluster, task model.Task) error {
	data, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return err
	}
	return remote.WriteFile(ctx, a.Runner, cluster.Host, path.Join(task.RemoteDir, "metadata.json"), append(data, '\n'), "600")
}

func (a *App) failSubmission(
	ctx context.Context,
	task *model.Task,
	code, stage, msg string,
	retryable bool,
	cause error,
) error {
	task.ComputeState = model.ComputeSubmissionFailed
	task.UpdatedAt = time.Now().UTC()
	if task.Metadata == nil {
		task.Metadata = map[string]string{}
	}
	task.Metadata["error_code"] = code
	task.Metadata["error"] = cause.Error()
	task.Metadata["error_stage"] = stage
	persistCtx, cancel := a.persistenceContext(ctx)
	defer cancel()
	_ = a.Store.UpdateTaskWithEvent(persistCtx, task, taskEvent(*task, "SUBMISSION_FAILED", stage,
		msg, map[string]string{"code": code, "error": cause.Error()}))
	return fault.Wrap(code, msg, retryable, cause).
		WithTask(stage, "joyrun status "+task.ID, task.ComputeState, task.PullState)
}

func (a *App) failSubmissionUncertain(
	ctx context.Context,
	task *model.Task,
	stage, msg string,
	cause error,
) error {
	task.ComputeState = model.ComputeSubmissionUncertain
	task.UpdatedAt = time.Now().UTC()
	if task.Metadata == nil {
		task.Metadata = map[string]string{}
	}
	task.Metadata["error_code"] = "SUBMISSION_UNCERTAIN"
	task.Metadata["error"] = cause.Error()
	task.Metadata["error_stage"] = stage
	persistCtx, cancel := a.persistenceContext(ctx)
	defer cancel()
	_ = a.Store.UpdateTaskWithEvent(persistCtx, task, taskEvent(*task, "SUBMISSION_UNCERTAIN", stage,
		msg, map[string]string{"code": "SUBMISSION_UNCERTAIN", "error": cause.Error()}))
	return fault.Wrap("SUBMISSION_UNCERTAIN", msg, true, cause).
		WithTask(stage, "joyrun status "+task.ID, task.ComputeState, task.PullState)
}

func (a *App) recordStage(
	ctx context.Context,
	task *model.Task,
	eventType, stage, msg string,
) error {
	task.UpdatedAt = time.Now().UTC()
	persistCtx, cancel := a.persistenceContext(ctx)
	defer cancel()
	return a.Store.UpdateTaskWithEvent(persistCtx, task, taskEvent(*task, eventType, stage, msg, nil))
}

func (a *App) failPull(
	ctx context.Context,
	task *model.Task,
	stage, msg string,
	cause error,
) error {
	code := "PULL_FAILED"
	if errors.Is(ctx.Err(), context.DeadlineExceeded) ||
		fault.As(cause).Code == "PULL_TIMEOUT" {
		code = "PULL_TIMEOUT"
	} else if errors.Is(ctx.Err(), context.Canceled) {
		code = "PULL_CANCELLED"
	}
	task.PullState = model.PullFailed
	task.UpdatedAt = time.Now().UTC()
	if task.Metadata == nil {
		task.Metadata = map[string]string{}
	}
	task.Metadata["error_code"] = code
	task.Metadata["error"] = cause.Error()
	task.Metadata["error_stage"] = stage
	persistCtx, cancel := a.persistenceContext(ctx)
	defer cancel()
	_ = a.Store.UpdateTaskWithEvent(persistCtx, task, taskEvent(*task, "PULL_FAILED", "pull",
		msg, map[string]string{"code": code, "stage": stage, "error": cause.Error()}))
	return fault.Wrap(code, msg, true, cause).
		WithTask("pull", "joyrun pull "+task.ID, task.ComputeState, task.PullState)
}

func taskEvent(task model.Task, eventType, stage, msg string, data map[string]string) model.TaskEvent {
	return model.TaskEvent{
		TaskID: task.ID, Type: eventType, Stage: stage, Message: msg,
		Data: data, CreatedAt: time.Now().UTC(),
	}
}

func terminalComputeState(state string) bool {
	return state == model.ComputeCompleted ||
		state == model.ComputeFailed ||
		state == model.ComputeCancelled
}

func refreshableComputeState(state string) bool {
	return state == model.ComputeCreated ||
		state == model.ComputeSubmissionFailed ||
		state == model.ComputeSubmissionUncertain ||
		state == model.ComputeQueued ||
		state == model.ComputeRunning ||
		state == model.ComputeUnknown
}

func bulkRefreshableComputeState(state string) bool {
	return state == model.ComputeCreated ||
		state == model.ComputeSubmissionUncertain ||
		state == model.ComputeQueued ||
		state == model.ComputeRunning ||
		state == model.ComputeUnknown
}

func (a *App) remoteContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := a.RemoteTimeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	return context.WithTimeout(parent, timeout)
}

func (a *App) submitContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := a.SubmitTimeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	return context.WithTimeout(parent, timeout)
}

func (a *App) recoveryContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := a.RecoveryTimeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
}

func (a *App) persistenceContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := a.PersistenceTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
}

func (a *App) progress(format string, args ...any) {
	if a.Progress != nil {
		fmt.Fprintf(a.Progress, format+"\n", args...)
	}
}

func operationFailure(ctx context.Context, fallback, timeout string) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return timeout
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return "SUBMISSION_CANCELLED"
	}
	return fallback
}

func transferFailure(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "UPLOAD_TIMEOUT"
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return "SUBMISSION_CANCELLED"
	}
	if fault.As(err).Code == "UPLOAD_TIMEOUT" {
		return "UPLOAD_TIMEOUT"
	}
	return "UPLOAD_FAILED"
}

func message(prefix, detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return prefix
	}
	return prefix + ": " + detail
}

func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func resolveFrom(cwd, value string) string {
	if filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(cwd, value)
}

func WriteHumanTask(writer io.Writer, task model.Task) {
	fmt.Fprintf(writer, "%s  compute:%s  pull:%s  %s  %s",
		task.ID, strings.ToUpper(task.ComputeState), strings.ToUpper(task.PullState),
		task.TargetName, task.SourcePath)
	if task.SchedulerID != "" {
		fmt.Fprintf(writer, "  slurm:%s", task.SchedulerID)
	}
	fmt.Fprintln(writer)
}
