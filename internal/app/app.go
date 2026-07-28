package app

import (
	"context"
	"encoding/json"
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
	Config   model.Config
	Store    *store.Store
	Runner   remote.Runner
	Transfer Transfer
}

type Transfer interface {
	Push(ctx context.Context, cluster model.Cluster, localDir, remoteDir string, excludes []string) error
	Pull(ctx context.Context, cluster model.Cluster, remoteDir, localDir string, files []string) error
	Check(ctx context.Context, cluster model.Cluster) (string, error)
}

type Preview struct {
	TaskID         string                `json:"task_id"`
	Source         model.Source          `json:"source"`
	Target         string                `json:"target"`
	Cluster        string                `json:"cluster"`
	Push           model.PushPolicy      `json:"push"`
	RemoteDir      string                `json:"remote_dir"`
	Params         map[string]any        `json:"params"`
	ParamSources   map[string]string     `json:"param_sources"`
	Files          []string              `json:"upload_files"`
	Ignored        []string              `json:"ignored"`
	RenderedScript string                `json:"rendered_script"`
	InputManifest  []model.ManifestEntry `json:"input_manifest"`
	TemplateValues TemplateValues        `json:"template_values"`
	SchedulerLog   string                `json:"scheduler_log"`
}

type TemplateValues struct {
	Input   string `json:"input"`
	Stem    string `json:"stem"`
	Name    string `json:"name"`
	WorkDir string `json:"workdir"`
}

type SubmitResult struct {
	Task model.Task `json:"task"`
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
	allowProjectRoot bool,
) (Preview, model.Task, string, error) {
	return a.prepare(ctx, cwd, sourcePath, targetName, sets, includes, true, allowProjectRoot)
}

func (a *App) prepare(
	ctx context.Context,
	cwd, sourcePath, targetName string,
	sets []string,
	includes []string,
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
	if len(includes) > 0 {
		encoded, _ := json.Marshal(includes)
		metadata["submit_includes"] = string(encoded)
	}
	task := model.Task{
		ID: taskID, ProjectID: p.ProjectID, SourcePath: src.RelativePath, SourceWorkDir: src.WorkDir,
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
		TaskID: taskID, Source: src, Target: targetName, Cluster: target.Cluster, Push: resolvedPush,
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
	allowProjectRoot bool,
) (SubmitResult, error) {
	_, task, localWorkDir, err := a.prepare(
		ctx, cwd, sourcePath, targetName, sets, includes, false, allowProjectRoot,
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
	if err := a.Store.CreateTask(ctx, &task); err != nil {
		return SubmitResult{}, err
	}
	cluster := a.Config.Clusters[task.ClusterName]
	workDir := path.Join(task.RemoteDir, "work")
	task.UpdatedAt = time.Now().UTC()
	if err := a.Store.UpdateTaskWithEvent(ctx, &task, taskEvent(task, "UPLOAD_STARTED", "upload",
		"Uploading immutable input snapshot", nil)); err != nil {
		return SubmitResult{}, err
	}
	if _, stderr, err := a.Runner.Exec(ctx, cluster.Host, "mkdir -p "+remote.Quote(workDir), nil); err != nil {
		return SubmitResult{}, a.failSubmission(ctx, &task, "SSH_FAILED", "upload",
			message("cannot create remote task directory", stderr), true, err)
	}
	if err := a.writeMetadata(ctx, cluster, task); err != nil {
		return SubmitResult{}, a.failSubmission(ctx, &task, "REMOTE_METADATA_FAILED", "upload",
			"cannot write recovery metadata", true, err)
	}
	if err := a.Transfer.Push(ctx, cluster, snapshotDir, workDir, nil); err != nil {
		return SubmitResult{}, a.failSubmission(ctx, &task, "UPLOAD_FAILED", "upload",
			"cannot upload task files", true, err)
	}
	if err := remote.WriteFile(ctx, a.Runner, cluster.Host, path.Join(workDir, "joyrun-job.sh"), []byte(task.RenderedScript), "700"); err != nil {
		return SubmitResult{}, a.failSubmission(ctx, &task, "UPLOAD_FAILED", "upload",
			"cannot upload rendered job script", true, err)
	}
	task.UpdatedAt = time.Now().UTC()
	if err := a.Store.UpdateTaskWithEvent(ctx, &task, taskEvent(task, "UPLOAD_COMPLETED", "upload",
		"Input snapshot and rendered script uploaded", nil)); err != nil {
		return SubmitResult{}, err
	}
	if err := a.Store.AppendEvent(ctx, taskEvent(task, "SUBMIT_STARTED", "submit",
		"Submitting task to scheduler", nil)); err != nil {
		return SubmitResult{}, err
	}
	slurm := scheduler.Slurm{Runner: a.Runner}
	schedulerID, err := slurm.Submit(ctx, cluster.Host, workDir, task.ID)
	if err != nil {
		// Submission may have succeeded even if the SSH connection dropped before
		// stdout arrived. Recover from the marker or the immutable Slurm comment.
		recoveredID, recoveryErr := a.recoverSchedulerID(ctx, cluster, task)
		if recoveryErr != nil || recoveredID == "" {
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
	if err := a.Store.UpdateTaskWithEvent(ctx, &task, taskEvent(task, "SCHEDULER_ACCEPTED", "submit",
		"Scheduler accepted task", map[string]string{"scheduler_id": schedulerID})); err != nil {
		// The scheduler accepted the job. Preserve the complete recovery record
		// remotely even when local persistence is unavailable.
		_ = a.writeMetadata(ctx, cluster, task)
		return SubmitResult{}, err
	}
	if err := a.writeMetadata(ctx, cluster, task); err != nil {
		if task.Metadata == nil {
			task.Metadata = map[string]string{}
		}
		task.Metadata["remote_metadata_error"] = err.Error()
		task.UpdatedAt = time.Now().UTC()
		_ = a.Store.UpdateTaskWithEvent(ctx, &task, taskEvent(task, "REMOTE_METADATA_WARNING", "recovery",
			"Scheduler accepted task but metadata refresh failed", map[string]string{"error": err.Error()}))
	}
	return SubmitResult{Task: task}, nil
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
	result := StatusAllResult{}
	for _, task := range tasks {
		if !refreshableComputeState(task.ComputeState) {
			result.Tasks = append(result.Tasks, task)
			continue
		}
		updated, err := a.refreshTask(ctx, task)
		if err != nil {
			result.Failures = append(result.Failures, StatusFailure{
				TaskID: task.ID, Error: fault.As(err),
			})
			continue
		}
		result.Tasks = append(result.Tasks, updated)
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
	status, err := (scheduler.Slurm{Runner: a.Runner}).Status(ctx, cluster.Host, task.SchedulerID)
	if err != nil {
		return task, fault.As(err).
			WithTask("status", "joyrun status "+task.ID, task.ComputeState, task.PullState)
	}
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
		// Elapsed time changes on every poll. Return the fresh value without
		// creating a database revision and lifecycle event for a timer tick.
		return task, nil
	}
	task.UpdatedAt = time.Now().UTC()
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
	_ = a.writeMetadata(ctx, cluster, task)
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
	id, err := (scheduler.Slurm{Runner: a.Runner}).FindByTaskID(
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
	if err := (scheduler.Slurm{Runner: a.Runner}).Cancel(ctx, cluster.Host, task.SchedulerID); err != nil {
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
	for _, item := range candidates {
		checked = append(checked, item.path)
		command := fmt.Sprintf("cd %s && if test -f %s; then printf 'FOUND\\n'; tail -n %d -- %s; else printf 'MISSING\\n'; fi",
			remote.Quote(workDir), remote.Quote(item.path), lines, remote.Quote(item.path))
		stdout, stderr, err := a.Runner.Exec(ctx, cluster.Host, command, nil)
		if err != nil {
			return LogResult{}, fault.Wrap("LOG_FAILED",
				message("cannot inspect remote log "+item.path, stderr), true, err).
				WithTask("logs", "joyrun logs "+task.ID, task.ComputeState, task.PullState)
		}
		status, content, _ := strings.Cut(stdout, "\n")
		if status == "FOUND" {
			return LogResult{TaskID: task.ID, Path: item.path, Kind: item.kind, Content: content}, nil
		}
		if status != "MISSING" {
			return LogResult{}, fault.New("LOG_FAILED",
				"unexpected response while inspecting remote log "+item.path, true).
				WithTask("logs", "joyrun logs "+task.ID, task.ComputeState, task.PullState)
		}
	}
	if len(checked) == 0 {
		return LogResult{}, fault.New("LOG_NOT_READY", "task has no application or scheduler log candidates yet", true).
			WithTask("logs", "joyrun status "+task.ID, task.ComputeState, task.PullState)
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
	if !options.Live {
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
	if err := a.Store.UpdateTaskWithEvent(ctx, &task, taskEvent(task, "PULL_STARTED", "pull",
		"Selected file transfer started", map[string]string{"destination": destination})); err != nil {
		return PullResult{}, err
	}
	if err := a.Transfer.Pull(ctx, cluster, workDir, destination, files); err != nil {
		return PullResult{}, a.failPull(ctx, &task, "transfer", "cannot pull selected task files", err)
	}
	now := time.Now().UTC()
	task.PulledAt, task.UpdatedAt = &now, now
	if terminalComputeState(task.ComputeState) {
		task.PullState = model.PullSucceeded
	} else {
		task.PullState = model.PullPartial
	}
	if err := a.Store.UpdateTaskWithEvent(ctx, &task, taskEvent(task, "PULL_COMPLETED", "pull",
		"Selected file transfer completed", map[string]string{"files": fmt.Sprintf("%d", len(files))})); err != nil {
		return PullResult{}, err
	}
	_ = a.writeMetadata(ctx, cluster, task)
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
	if err := remote.Check(ctx, a.Runner, cluster.Host); err != nil {
		checks = append(checks, DoctorCheck{
			Name: "ssh", Status: "fail", OK: false, Blocking: true, Message: err.Error(),
			SuggestedAction: "verify that `ssh " + cluster.Host + "` works non-interactively",
		})
		return doctorResult(checks)
	}
	checks = append(checks, DoctorCheck{
		Name: "ssh", Status: "pass", OK: true, Message: cluster.Host,
	})
	checks = append(checks, a.checkRemoteRoot(ctx, target.Cluster, cluster))
	for _, executable := range []string{"sbatch", "squeue", "sacct", "scancel"} {
		_, stderr, err := a.Runner.Exec(ctx, cluster.Host, "command -v "+executable, nil)
		detail := "available"
		if err != nil {
			detail = strings.TrimSpace(stderr)
			if detail == "" {
				detail = executable + " was not found on " + cluster.Host
			}
		}
		checks = append(checks, doctorCheck(executable, err == nil, detail,
			"load or install Slurm so `command -v "+executable+"` succeeds"))
	}
	backend, err := a.Transfer.Check(ctx, cluster)
	detail := errorMessage(err)
	if detail == "" {
		detail = backend + " is available"
	}
	checks = append(checks, doctorCheck("transfer_"+backend, err == nil, detail,
		"install/configure the selected transfer backend or use transfer: auto"))
	return doctorResult(checks)
}

func (a *App) checkRemoteRoot(ctx context.Context, clusterName string, cluster model.Cluster) DoctorCheck {
	command := "root=" + remote.Quote(cluster.RemoteRoot) + "; " +
		"if [ -d \"$root\" ]; then " +
		"if [ -w \"$root\" ]; then printf 'pass'; else printf 'not_writable'; fi; " +
		"elif [ -e \"$root\" ]; then printf 'not_directory'; " +
		"else ancestor=$(dirname -- \"$root\"); " +
		"while [ ! -e \"$ancestor\" ] && [ \"$ancestor\" != / ]; do ancestor=$(dirname -- \"$ancestor\"); done; " +
		"if [ -d \"$ancestor\" ] && [ -w \"$ancestor\" ]; then printf 'creatable:%s' \"$ancestor\"; " +
		"else printf 'not_creatable:%s' \"$ancestor\"; fi; fi"
	stdout, stderr, err := a.Runner.Exec(ctx, cluster.Host, command, nil)
	if err != nil {
		return DoctorCheck{
			Name: "remote_root", Status: "fail", OK: false, Blocking: true,
			Message:         message("cannot inspect remote_root "+cluster.RemoteRoot, stderr),
			SuggestedAction: "verify the path and permissions on " + cluster.Host,
		}
	}
	result := strings.TrimSpace(stdout)
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
			(task.ComputeState == model.ComputeCreated || task.ComputeState == model.ComputeSubmissionFailed) {
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
		" -mindepth 2 -maxdepth 2 -type f -name metadata.json -printf '%h\\0'"
	stdout, stderr, err := a.Runner.Exec(ctx, cluster.Host, command, nil)
	if err != nil {
		return nil, fault.Wrap("RECOVERY_SCAN_FAILED",
			message("cannot scan remote JoyRun metadata", stderr), true, err)
	}
	seen := map[string]bool{}
	var candidates []RecoveryCandidate
	for _, directory := range strings.Split(stdout, "\x00") {
		taskID := path.Base(directory)
		if taskID == "" || !strings.HasPrefix(taskID, "jr_") || seen[taskID] {
			continue
		}
		seen[taskID] = true
		data, err := remote.ReadFile(ctx, a.Runner, cluster.Host,
			path.Join(cluster.RemoteRoot, taskID, "metadata.json"))
		if err != nil {
			continue
		}
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
	_ = a.Store.UpdateTaskWithEvent(ctx, task, taskEvent(*task, "SUBMISSION_FAILED", stage,
		msg, map[string]string{"code": code, "error": cause.Error()}))
	return fault.Wrap(code, msg, retryable, cause).
		WithTask(stage, "joyrun status "+task.ID, task.ComputeState, task.PullState)
}

func (a *App) failPull(
	ctx context.Context,
	task *model.Task,
	stage, msg string,
	cause error,
) error {
	task.PullState = model.PullFailed
	task.UpdatedAt = time.Now().UTC()
	if task.Metadata == nil {
		task.Metadata = map[string]string{}
	}
	task.Metadata["error_code"] = "PULL_FAILED"
	task.Metadata["error"] = cause.Error()
	task.Metadata["error_stage"] = stage
	_ = a.Store.UpdateTaskWithEvent(ctx, task, taskEvent(*task, "PULL_FAILED", "pull",
		msg, map[string]string{"stage": stage, "error": cause.Error()}))
	return fault.Wrap("PULL_FAILED", msg, true, cause).
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
		state == model.ComputeQueued ||
		state == model.ComputeRunning ||
		state == model.ComputeUnknown
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
