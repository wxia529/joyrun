package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"sort"
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
	RemoteDir      string                `json:"remote_dir"`
	Params         map[string]any        `json:"params"`
	ParamSources   map[string]string     `json:"param_sources"`
	Files          []string              `json:"files"`
	Ignored        []string              `json:"ignored"`
	RenderedScript string                `json:"rendered_script"`
	InputManifest  []model.ManifestEntry `json:"input_manifest"`
}

type SubmitResult struct {
	Task model.Task `json:"task"`
}

type PullOptions struct {
	All             bool
	Include         []string
	OverwriteInputs bool
	Live            bool
}

type PullResult struct {
	Task        model.Task `json:"task"`
	Files       []string   `json:"files"`
	Destination string     `json:"destination"`
}

type DoctorCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
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

func (a *App) Preview(ctx context.Context, cwd, sourcePath, targetName string, sets []string) (Preview, model.Task, string, error) {
	p, err := project.Discover(cwd)
	if err != nil {
		return Preview{}, model.Task{}, "", err
	}
	if err := a.Store.BindProject(ctx, p); err != nil {
		return Preview{}, model.Task{}, "", err
	}
	src, localWorkDir, err := source.Resolve(p, resolveFrom(cwd, sourcePath))
	if err != nil {
		return Preview{}, model.Task{}, "", err
	}
	target, ok := a.Config.Targets[targetName]
	if !ok {
		return Preview{}, model.Task{}, "", fault.New("TARGET_NOT_FOUND", fmt.Sprintf("target %q not found", targetName), false)
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
	inputManifest, ignored, err := manifest.Build(localWorkDir, p.Root, target.Push.Exclude)
	if err != nil {
		return Preview{}, model.Task{}, "", err
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
	task := model.Task{
		ID: taskID, ProjectID: p.ProjectID, SourcePath: src.RelativePath, SourceWorkDir: src.WorkDir,
		SourceEntry: src.Entry, TargetName: targetName, ClusterName: target.Cluster,
		RemoteDir: remoteDir, State: model.StateCreated, ResolvedParams: params,
		RenderedScript: script, TargetHash: jtemplate.TargetHash(target), InputManifest: inputManifest,
		PullPatterns: append([]string{}, target.Pull.Default...),
		PushExcludes: manifest.ExcludePatterns(p.Root, localWorkDir, target.Push.Exclude),
		Logs:         logs, CreatedAt: now, UpdatedAt: now,
	}
	preview := Preview{
		TaskID: taskID, Source: src, Target: targetName, Cluster: target.Cluster,
		RemoteDir: remoteDir, Params: params, ParamSources: paramSources, Files: files,
		Ignored: ignored, RenderedScript: script, InputManifest: inputManifest,
	}
	return preview, task, localWorkDir, nil
}

func (a *App) Submit(ctx context.Context, cwd, sourcePath, targetName string, sets []string) (SubmitResult, error) {
	_, task, localWorkDir, err := a.Preview(ctx, cwd, sourcePath, targetName, sets)
	if err != nil {
		return SubmitResult{}, err
	}
	snapshotDir, inputManifest, _, cleanup, err := manifest.Snapshot(localWorkDir, task.PushExcludes)
	if err != nil {
		return SubmitResult{}, err
	}
	defer cleanup()
	task.InputManifest = inputManifest
	if err := a.Store.CreateTask(ctx, task); err != nil {
		return SubmitResult{}, err
	}
	cluster := a.Config.Clusters[task.ClusterName]
	workDir := path.Join(task.RemoteDir, "work")
	task.State = model.StateUploading
	task.UpdatedAt = time.Now().UTC()
	_ = a.Store.UpdateTask(ctx, task)
	if _, stderr, err := a.Runner.Exec(ctx, cluster.Host, "mkdir -p "+remote.Quote(workDir), nil); err != nil {
		return SubmitResult{}, a.failTask(ctx, &task, "SSH_FAILED", message("cannot create remote task directory", stderr), true, err)
	}
	if err := a.writeMetadata(ctx, cluster, task); err != nil {
		return SubmitResult{}, a.failTask(ctx, &task, "REMOTE_METADATA_FAILED", "cannot write recovery metadata", true, err)
	}
	if err := a.Transfer.Push(ctx, cluster, snapshotDir, workDir, nil); err != nil {
		return SubmitResult{}, a.failTask(ctx, &task, "UPLOAD_FAILED", "cannot upload task files", true, err)
	}
	if err := remote.WriteFile(ctx, a.Runner, cluster.Host, path.Join(workDir, "joyrun-job.sh"), []byte(task.RenderedScript), "700"); err != nil {
		return SubmitResult{}, a.failTask(ctx, &task, "UPLOAD_FAILED", "cannot upload rendered job script", true, err)
	}
	slurm := scheduler.Slurm{Runner: a.Runner}
	schedulerID, err := slurm.Submit(ctx, cluster.Host, workDir)
	if err != nil {
		// Submission may have succeeded even if the SSH connection dropped before
		// stdout arrived. The remote scheduler_id marker resolves that ambiguity.
		data, recoveryErr := remote.ReadFile(ctx, a.Runner, cluster.Host, path.Join(task.RemoteDir, "scheduler_id"))
		if recoveryErr != nil || strings.TrimSpace(string(data)) == "" {
			return SubmitResult{}, a.failTask(ctx, &task, "SUBMIT_FAILED", "cannot submit task to Slurm", true, err)
		}
		schedulerID = strings.TrimSpace(string(data))
	}
	now := time.Now().UTC()
	task.SchedulerID = schedulerID
	task.State = model.StateQueued
	task.SubmittedAt = &now
	task.UpdatedAt = now
	if err := a.Store.UpdateTask(ctx, task); err != nil {
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
		_ = a.Store.UpdateTask(ctx, task)
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

func (a *App) Status(ctx context.Context, cwd, identifier string) (model.Task, error) {
	task, _, err := a.ResolveTask(ctx, cwd, identifier)
	if err != nil {
		return task, err
	}
	if task.SchedulerID == "" {
		return task, nil
	}
	cluster, ok := a.Config.Clusters[task.ClusterName]
	if !ok {
		return task, fault.New("CLUSTER_NOT_FOUND", fmt.Sprintf("cluster %q is no longer configured", task.ClusterName), false)
	}
	state, raw, err := (scheduler.Slurm{Runner: a.Runner}).Status(ctx, cluster.Host, task.SchedulerID)
	if err != nil {
		return task, err
	}
	task.State, task.SchedulerState, task.UpdatedAt = state, raw, time.Now().UTC()
	if err := a.Store.UpdateTask(ctx, task); err != nil {
		return task, err
	}
	_ = a.writeMetadata(ctx, cluster, task)
	return task, nil
}

func (a *App) Cancel(ctx context.Context, cwd, identifier string) (model.Task, error) {
	task, _, err := a.ResolveTask(ctx, cwd, identifier)
	if err != nil {
		return task, err
	}
	if task.SchedulerID == "" {
		return task, fault.New("CANCEL_FAILED", "task has no scheduler ID", false)
	}
	cluster := a.Config.Clusters[task.ClusterName]
	if err := (scheduler.Slurm{Runner: a.Runner}).Cancel(ctx, cluster.Host, task.SchedulerID); err != nil {
		return task, err
	}
	task.State, task.SchedulerState, task.UpdatedAt = model.StateCancelled, "CANCELLED", time.Now().UTC()
	if err := a.Store.UpdateTask(ctx, task); err != nil {
		return task, err
	}
	_ = a.writeMetadata(ctx, cluster, task)
	return task, nil
}

func (a *App) Logs(ctx context.Context, cwd, identifier string, lines int) (string, model.Task, error) {
	task, _, err := a.ResolveTask(ctx, cwd, identifier)
	if err != nil {
		return "", task, err
	}
	cluster := a.Config.Clusters[task.ClusterName]
	logPath := ""
	if len(task.Logs) > 0 {
		logPath = task.Logs[0]
	} else if task.SchedulerID != "" {
		logPath = "slurm-" + task.SchedulerID + ".out"
	}
	if logPath == "" {
		return "", task, fault.New("LOG_NOT_AVAILABLE", "task has no configured or scheduler log", false)
	}
	command := fmt.Sprintf("cd %s && tail -n %d -- %s", remote.Quote(path.Join(task.RemoteDir, "work")), lines, remote.Quote(logPath))
	stdout, stderr, err := a.Runner.Exec(ctx, cluster.Host, command, nil)
	if err != nil {
		return "", task, fault.Wrap("LOG_FAILED", message("cannot read remote log", stderr), true, err)
	}
	return stdout, task, nil
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
		if task.State != model.StateCompleted {
			return PullResult{}, fault.New("JOB_NOT_COMPLETED", fmt.Sprintf("task is %s; use --live to pull available files", task.State), false)
		}
	}
	cluster := a.Config.Clusters[task.ClusterName]
	workDir := path.Join(task.RemoteDir, "work")
	stdout, stderr, err := a.Runner.Exec(ctx, cluster.Host, "cd "+remote.Quote(workDir)+" && find . -type f -printf '%P\\0'", nil)
	if err != nil {
		return PullResult{}, fault.Wrap("PULL_FAILED", message("cannot list remote task files", stderr), true, err)
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
	for _, file := range strings.Split(stdout, "\x00") {
		file = filepath.ToSlash(file)
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
	}
	sort.Strings(files)
	if err := localfs.ValidatePullPaths(files); err != nil {
		return PullResult{}, err
	}
	destination := filepath.Join(p.Root, filepath.FromSlash(task.SourceWorkDir))
	if err := a.Transfer.Pull(ctx, cluster, workDir, destination, files); err != nil {
		return PullResult{}, err
	}
	now := time.Now().UTC()
	task.ResultsPulledAt, task.UpdatedAt = &now, now
	if err := a.Store.UpdateTask(ctx, task); err != nil {
		return PullResult{}, err
	}
	return PullResult{Task: task, Files: files, Destination: destination}, nil
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

func (a *App) Doctor(ctx context.Context, targetName string) []DoctorCheck {
	var checks []DoctorCheck
	target, ok := a.Config.Targets[targetName]
	checks = append(checks, DoctorCheck{Name: "target", OK: ok, Message: targetName})
	if !ok {
		return checks
	}
	cluster, ok := a.Config.Clusters[target.Cluster]
	checks = append(checks, DoctorCheck{Name: "cluster", OK: ok, Message: target.Cluster})
	if !ok {
		return checks
	}
	if err := remote.Check(ctx, a.Runner, cluster.Host); err != nil {
		checks = append(checks, DoctorCheck{Name: "ssh", OK: false, Message: err.Error()})
		return checks
	}
	checks = append(checks, DoctorCheck{Name: "ssh", OK: true, Message: cluster.Host})
	command := "test -d " + remote.Quote(cluster.RemoteRoot) + " && test -w " + remote.Quote(cluster.RemoteRoot)
	_, stderr, err := a.Runner.Exec(ctx, cluster.Host, command, nil)
	checks = append(checks, DoctorCheck{Name: "remote_root", OK: err == nil, Message: strings.TrimSpace(stderr)})
	for _, executable := range []string{"sbatch", "squeue", "sacct", "scancel"} {
		_, stderr, err := a.Runner.Exec(ctx, cluster.Host, "command -v "+executable, nil)
		checks = append(checks, DoctorCheck{Name: executable, OK: err == nil, Message: strings.TrimSpace(stderr)})
	}
	backend, err := a.Transfer.Check(ctx, cluster)
	checks = append(checks, DoctorCheck{Name: "transfer_" + backend, OK: err == nil, Message: errorMessage(err)})
	return checks
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
	if task.SchedulerID == "" {
		marker, markerErr := remote.ReadFile(ctx, a.Runner, cluster.Host, path.Join(cluster.RemoteRoot, taskID, "scheduler_id"))
		if markerErr == nil {
			task.SchedulerID = strings.TrimSpace(string(marker))
			if task.SchedulerID != "" && (task.State == model.StateCreated || task.State == model.StateUploading) {
				task.State = model.StateQueued
			}
		}
	}
	if err := a.Store.ImportTask(ctx, task); err != nil {
		return task, err
	}
	return task, nil
}

func (a *App) writeMetadata(ctx context.Context, cluster model.Cluster, task model.Task) error {
	data, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return err
	}
	return remote.WriteFile(ctx, a.Runner, cluster.Host, path.Join(task.RemoteDir, "metadata.json"), append(data, '\n'), "600")
}

func (a *App) failTask(ctx context.Context, task *model.Task, code, msg string, retryable bool, cause error) error {
	task.State = model.StateFailed
	task.UpdatedAt = time.Now().UTC()
	if task.Metadata == nil {
		task.Metadata = map[string]string{}
	}
	task.Metadata["error_code"] = code
	task.Metadata["error"] = cause.Error()
	_ = a.Store.UpdateTask(ctx, *task)
	return fault.Wrap(code, msg, retryable, cause)
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
	fmt.Fprintf(writer, "%s  %s  %s  %s", task.ID, strings.ToUpper(task.State), task.TargetName, task.SourcePath)
	if task.SchedulerID != "" {
		fmt.Fprintf(writer, "  slurm:%s", task.SchedulerID)
	}
	fmt.Fprintln(writer)
}
