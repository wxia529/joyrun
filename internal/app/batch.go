package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/identity"
	"github.com/wxia529/joyrun/internal/localfs"
	"github.com/wxia529/joyrun/internal/manifest"
	"github.com/wxia529/joyrun/internal/model"
	"github.com/wxia529/joyrun/internal/project"
	"github.com/wxia529/joyrun/internal/remote"
	"github.com/wxia529/joyrun/internal/scheduler"
)

const MaxBatchTasks = 100

type BatchFailure struct {
	TaskID string       `json:"task_id,omitempty"`
	Source string       `json:"source,omitempty"`
	Error  *fault.Error `json:"error"`
}

type SubmitManyResult struct {
	BatchID  string         `json:"batch_id"`
	Tasks    []model.Task   `json:"tasks"`
	Failures []BatchFailure `json:"failures,omitempty"`
}

type PullManyOptions struct {
	PullOptions
	Finished bool
	BatchID  string
}

type PullManyItem struct {
	TaskID      string   `json:"task_id"`
	Source      string   `json:"source"`
	Files       []string `json:"files,omitempty"`
	Destination string   `json:"destination,omitempty"`
	TotalBytes  int64    `json:"total_bytes,omitempty"`
	PullState   string   `json:"pull_state"`
}

type PullManyResult struct {
	PullID        string         `json:"pull_id"`
	SourceBatchID string         `json:"source_batch_id,omitempty"`
	Tasks         []PullManyItem `json:"tasks"`
	Failures      []BatchFailure `json:"failures,omitempty"`
	DryRun        bool           `json:"dry_run,omitempty"`
}

func (a *App) SubmitMany(
	ctx context.Context,
	cwd string,
	sources []string,
	targetName string,
	sets, includes []string,
	partitionOverride string,
	allowProjectRoot bool,
) (SubmitManyResult, error) {
	if len(sources) < 2 {
		return SubmitManyResult{}, fault.New("INVALID_ARGUMENT",
			"batch submission requires at least two distinct sources", false)
	}
	if len(sources) > MaxBatchTasks {
		return SubmitManyResult{}, fault.New("BATCH_TOO_LARGE",
			fmt.Sprintf("submit accepts at most %d sources", MaxBatchTasks), false)
	}
	batchID, err := identity.New("jb_")
	if err != nil {
		return SubmitManyResult{}, fault.Wrap("TASK_CREATE_FAILED", "cannot allocate batch ID", false, err)
	}
	p, err := project.Discover(cwd)
	if err != nil {
		return SubmitManyResult{}, err
	}
	target, ok := a.Config.Targets[targetName]
	if !ok {
		return SubmitManyResult{}, fault.New("TARGET_NOT_FOUND",
			fmt.Sprintf("target %q not found", targetName), false)
	}
	cluster := a.Config.Clusters[target.Cluster]
	staging, err := os.MkdirTemp("", "joyrun-batch-submit-*")
	if err != nil {
		return SubmitManyResult{}, fault.Wrap("SOURCE_SNAPSHOT_FAILED",
			"cannot create batch staging directory", false, err)
	}
	defer os.RemoveAll(staging)

	tasks := make([]model.Task, 0, len(sources))
	taskPointers := make([]*model.Task, 0, len(sources))
	seenSources := map[string]bool{}
	for _, sourcePath := range sources {
		_, task, localWorkDir, err := a.prepare(
			ctx, cwd, sourcePath, targetName, sets, includes,
			partitionOverride, false, allowProjectRoot,
		)
		if err != nil {
			return SubmitManyResult{}, err
		}
		if seenSources[task.SourcePath] {
			return SubmitManyResult{}, fault.New("DUPLICATE_SOURCE",
				fmt.Sprintf("source %q appears more than once in the batch", task.SourcePath), false)
		}
		seenSources[task.SourcePath] = true
		selection, err := uploadSelection(
			p.Root, localWorkDir, model.Source{Entry: task.SourceEntry}, target, includes)
		if err != nil {
			return SubmitManyResult{}, err
		}
		snapshotDir, inputManifest, _, cleanup, err :=
			manifest.Snapshot(localWorkDir, selection)
		if err != nil {
			return SubmitManyResult{}, err
		}
		if err := validateRequestedIncludes(includes, inputManifest); err != nil {
			cleanup()
			return SubmitManyResult{}, err
		}
		task.InputManifest = inputManifest
		task.Metadata["batch_id"] = batchID
		task.Metadata["batch_index"] = strconv.Itoa(len(tasks))
		task.Revision = 1
		if err := os.WriteFile(filepath.Join(snapshotDir, "joyrun-job.sh"),
			[]byte(task.RenderedScript), 0o700); err != nil {
			cleanup()
			return SubmitManyResult{}, fault.Wrap("SNAPSHOT_PREPARE_FAILED",
				"cannot add rendered job script to batch snapshot", false, err)
		}
		taskRoot := filepath.Join(staging, task.ID)
		if err := os.MkdirAll(taskRoot, 0o700); err != nil {
			cleanup()
			return SubmitManyResult{}, err
		}
		if err := os.Rename(snapshotDir, filepath.Join(taskRoot, "work")); err != nil {
			cleanup()
			return SubmitManyResult{}, fault.Wrap("SNAPSHOT_PREPARE_FAILED",
				"cannot assemble batch snapshot", false, err)
		}
		cleanup()
		metadata, err := json.MarshalIndent(task, "", "  ")
		if err != nil {
			return SubmitManyResult{}, err
		}
		if err := os.WriteFile(filepath.Join(taskRoot, "metadata.json"),
			append(metadata, '\n'), 0o600); err != nil {
			return SubmitManyResult{}, err
		}
		tasks = append(tasks, task)
	}
	for index := range tasks {
		taskPointers = append(taskPointers, &tasks[index])
	}
	if err := a.Store.BindProject(ctx, p); err != nil {
		return SubmitManyResult{}, err
	}
	if err := a.Store.CreateTasks(ctx, taskPointers); err != nil {
		return SubmitManyResult{}, err
	}
	a.progress("Uploading %d task snapshot(s) in one batch...", len(tasks))
	if err := a.Transfer.Push(ctx, cluster, staging, cluster.RemoteRoot, nil); err != nil {
		result := SubmitManyResult{BatchID: batchID}
		for index := range tasks {
			failure := a.failSubmission(ctx, &tasks[index], transferFailure(ctx, err),
				"snapshot", "cannot upload batch task snapshots", true, err)
			result.Failures = append(result.Failures, BatchFailure{
				TaskID: tasks[index].ID, Source: tasks[index].SourcePath, Error: fault.As(failure),
			})
		}
		return result, nil
	}
	jobs := make([]scheduler.BatchJob, 0, len(tasks))
	for _, task := range tasks {
		jobs = append(jobs, scheduler.BatchJob{
			TaskID: task.ID, WorkDir: path.Join(task.RemoteDir, "work"),
			Partition: task.Metadata["partition"],
		})
	}
	a.progress("Submitting %d independent Slurm job(s) in one remote session...", len(tasks))
	submitCtx, cancelSubmit := a.batchSubmitContext(ctx, len(tasks))
	submitted, submitErr := (scheduler.Slurm{Runner: a.Runner}).SubmitMany(
		submitCtx, cluster.Host, jobs)
	cancelSubmit()
	if submitErr != nil {
		recoveryCtx, cancelRecovery := a.recoveryContext(ctx)
		recovered, _ := a.batchSchedulerMarkers(recoveryCtx, cluster, tasks)
		cancelRecovery()
		for taskID, schedulerID := range recovered {
			submitted.SchedulerIDs[taskID] = schedulerID
		}
	}
	result := SubmitManyResult{BatchID: batchID}
	for index := range tasks {
		task := &tasks[index]
		if schedulerID := submitted.SchedulerIDs[task.ID]; schedulerID != "" {
			now := time.Now().UTC()
			task.SchedulerID = schedulerID
			task.ComputeState = model.ComputeQueued
			task.SubmittedAt, task.UpdatedAt = &now, now
			persistCtx, cancelPersist := a.persistenceContext(ctx)
			err = a.Store.UpdateTaskWithEvent(persistCtx, task, taskEvent(*task,
				"SCHEDULER_ACCEPTED", "submit", "Scheduler accepted batch task",
				map[string]string{"scheduler_id": schedulerID, "batch_id": batchID}))
			cancelPersist()
			if err != nil {
				result.Failures = append(result.Failures, BatchFailure{
					TaskID: task.ID, Source: task.SourcePath, Error: fault.As(err),
				})
				continue
			}
			result.Tasks = append(result.Tasks, *task)
			continue
		}
		if message := submitted.Failures[task.ID]; message != "" {
			cause := errors.New(message)
			failure := a.failSubmission(ctx, task, "SUBMIT_FAILED", "submit",
				"Slurm rejected batch task", false, cause)
			result.Failures = append(result.Failures, BatchFailure{
				TaskID: task.ID, Source: task.SourcePath, Error: fault.As(failure),
			})
			continue
		}
		cause := submitErr
		if cause == nil {
			cause = errors.New("batch submit returned no result for task")
		}
		failure := a.failSubmissionUncertain(ctx, task, "submit",
			"batch submission may have succeeded, but its scheduler ID was not recovered", cause)
		result.Failures = append(result.Failures, BatchFailure{
			TaskID: task.ID, Source: task.SourcePath, Error: fault.As(failure),
		})
	}
	return result, nil
}

func (a *App) batchSubmitContext(
	parent context.Context,
	tasks int,
) (context.Context, context.CancelFunc) {
	timeout := a.SubmitTimeout
	if timeout <= 0 {
		timeout = 45*time.Second + time.Duration(tasks)*time.Second
		if timeout > 5*time.Minute {
			timeout = 5 * time.Minute
		}
	}
	return context.WithTimeout(parent, timeout)
}

func (a *App) batchSchedulerMarkers(
	ctx context.Context,
	cluster model.Cluster,
	tasks []model.Task,
) (map[string]string, error) {
	var command strings.Builder
	for _, task := range tasks {
		fmt.Fprintf(&command,
			"if test -f %s; then printf '%s\\0'; head -n1 -- %s; printf '\\0'; fi; ",
			remote.Quote(path.Join(task.RemoteDir, "scheduler_id")), task.ID,
			remote.Quote(path.Join(task.RemoteDir, "scheduler_id")))
	}
	stdout, stderr, err := a.Runner.Exec(ctx, cluster.Host, command.String(), nil)
	if err != nil {
		return nil, fault.Wrap("STATUS_FAILED",
			message("cannot reconcile batch scheduler markers", stderr), true, err)
	}
	result := map[string]string{}
	records := strings.Split(stdout, "\x00")
	for index := 0; index+1 < len(records); index += 2 {
		id := strings.TrimSpace(records[index+1])
		if _, err := strconv.ParseUint(id, 10, 64); err == nil {
			result[records[index]] = id
		}
	}
	return result, nil
}

type batchPullPlan struct {
	task        model.Task
	cluster     model.Cluster
	files       []string
	remoteFiles []string
	destination string
	totalBytes  int64
}

func (a *App) PullMany(
	ctx context.Context,
	cwd string,
	identifiers []string,
	options PullManyOptions,
) (PullManyResult, error) {
	pullID, err := identity.New("jp_")
	if err != nil {
		return PullManyResult{}, err
	}
	p, err := project.Discover(cwd)
	if err != nil {
		return PullManyResult{}, err
	}
	if err := a.Store.BindProject(ctx, p); err != nil {
		return PullManyResult{}, err
	}
	var tasks []model.Task
	var selectionFailures []BatchFailure
	if options.Finished {
		status := a.StatusAll(ctx, cwd)
		for _, failure := range status.Failures {
			selectionFailures = append(selectionFailures, BatchFailure{
				TaskID: failure.TaskID, Error: failure.Error,
			})
		}
		seenSources := map[string]bool{}
		for _, task := range status.Tasks {
			if seenSources[task.SourcePath] {
				continue
			}
			seenSources[task.SourcePath] = true
			if terminalComputeState(task.ComputeState) &&
				task.PullState != model.PullSucceeded {
				tasks = append(tasks, task)
			}
		}
	} else if options.BatchID != "" {
		indexed, err := a.Store.ListTasks(ctx, p.ProjectID)
		if err != nil {
			return PullManyResult{}, err
		}
		for _, task := range indexed {
			if task.Metadata["batch_id"] == options.BatchID {
				tasks = append(tasks, task)
			}
		}
		sort.SliceStable(tasks, func(i, j int) bool {
			left, leftErr := strconv.Atoi(tasks[i].Metadata["batch_index"])
			right, rightErr := strconv.Atoi(tasks[j].Metadata["batch_index"])
			if leftErr == nil && rightErr == nil && left != right {
				return left < right
			}
			return tasks[i].CreatedAt.Before(tasks[j].CreatedAt)
		})
	} else {
		seen := map[string]bool{}
		for _, identifier := range identifiers {
			task, _, err := a.ResolveTask(ctx, cwd, identifier)
			if err != nil {
				return PullManyResult{}, err
			}
			if !seen[task.ID] {
				seen[task.ID] = true
				tasks = append(tasks, task)
			}
		}
	}
	if len(tasks) == 0 {
		if len(selectionFailures) > 0 {
			return PullManyResult{
				PullID: pullID, SourceBatchID: options.BatchID,
				Failures: selectionFailures, DryRun: options.DryRun,
			}, nil
		}
		return PullManyResult{}, fault.New("NO_TASKS_MATCHED",
			"no tasks matched the batch pull selection", false)
	}
	if len(tasks) > MaxBatchTasks {
		return PullManyResult{}, fault.New("BATCH_TOO_LARGE",
			fmt.Sprintf("pull accepts at most %d tasks", MaxBatchTasks), false)
	}
	if !options.Live {
		tasks, err = a.refreshTaskBatch(ctx, tasks)
		if err != nil {
			return PullManyResult{}, err
		}
		for _, task := range tasks {
			if !terminalComputeState(task.ComputeState) {
				return PullManyResult{}, fault.New("JOB_NOT_COMPLETED",
					fmt.Sprintf("task %s is %s; use --live for available files",
						task.ID, task.ComputeState), false)
			}
		}
	}
	listings, err := a.listRemoteFilesBatch(ctx, tasks)
	if err != nil {
		return PullManyResult{}, err
	}
	plans := make([]batchPullPlan, 0, len(tasks))
	destinations := map[string]string{}
	for _, task := range tasks {
		cluster, ok := a.Config.Clusters[task.ClusterName]
		if !ok {
			return PullManyResult{}, fault.New("CLUSTER_NOT_FOUND",
				fmt.Sprintf("cluster %q is no longer configured", task.ClusterName), false)
		}
		patterns := task.PullPatterns
		if len(options.Include) > 0 {
			patterns = options.Include
		}
		inputs := map[string]bool{}
		for _, entry := range task.InputManifest {
			inputs[entry.Path] = true
		}
		plan := batchPullPlan{
			task: task, cluster: cluster,
			destination: filepath.Join(p.Root, filepath.FromSlash(task.SourceWorkDir)),
		}
		for _, file := range listings[task.ID] {
			if file.Path == "joyrun-job.sh" ||
				(!options.All && !manifest.Match(file.Path, patterns)) ||
				(inputs[file.Path] && !options.OverwriteInputs) {
				continue
			}
			localPath := filepath.Clean(filepath.Join(
				plan.destination, filepath.FromSlash(file.Path)))
			key := localPath
			if runtime.GOOS == "windows" {
				key = strings.ToLower(key)
			}
			if previous, ok := destinations[key]; ok && previous != task.ID {
				return PullManyResult{}, fault.New("BATCH_LOCAL_CONFLICT",
					fmt.Sprintf("tasks %s and %s both write %s", previous, task.ID, localPath), false)
			}
			destinations[key] = task.ID
			plan.files = append(plan.files, file.Path)
			plan.remoteFiles = append(plan.remoteFiles,
				path.Join(path.Base(task.RemoteDir), "work", file.Path))
			plan.totalBytes += file.Size
		}
		if len(plan.files) == 0 {
			return PullManyResult{}, fault.New("NO_FILES_MATCHED",
				"no remote files matched task "+task.ID, false)
		}
		if err := localfs.ValidatePullDestination(plan.destination, plan.files); err != nil {
			return PullManyResult{}, err
		}
		plans = append(plans, plan)
	}
	result := PullManyResult{
		PullID: pullID, SourceBatchID: options.BatchID,
		Failures: selectionFailures, DryRun: options.DryRun,
	}
	if options.DryRun {
		for _, plan := range plans {
			result.Tasks = append(result.Tasks, PullManyItem{
				TaskID: plan.task.ID, Source: plan.task.SourcePath,
				Files: plan.files, Destination: plan.destination,
				TotalBytes: plan.totalBytes, PullState: plan.task.PullState,
			})
		}
		return result, nil
	}
	groups := map[string][]batchPullPlan{}
	for _, plan := range plans {
		groups[plan.task.ClusterName] = append(groups[plan.task.ClusterName], plan)
	}
	for _, group := range groups {
		cluster := group[0].cluster
		staging, err := os.MkdirTemp(p.Root, ".joyrun-batch-pull-*")
		if err != nil {
			return result, err
		}
		defer os.RemoveAll(staging)
		taskPointers := make([]*model.Task, 0, len(group))
		events := make([]model.TaskEvent, 0, len(group))
		for index := range group {
			group[index].task.PullState = model.PullInProgress
			group[index].task.UpdatedAt = time.Now().UTC()
			taskPointers = append(taskPointers, &group[index].task)
			events = append(events, taskEvent(group[index].task,
				"PULL_STARTED", "pull", "Batch result transfer started",
				map[string]string{"pull_id": pullID}))
		}
		persistCtx, cancelPersist := a.persistenceContext(ctx)
		err = a.Store.UpdateTasksWithEvents(persistCtx, taskPointers, events)
		cancelPersist()
		if err != nil {
			return result, err
		}
		var remoteFiles []string
		for _, plan := range group {
			remoteFiles = append(remoteFiles, plan.remoteFiles...)
		}
		err = a.Transfer.Pull(ctx, cluster, cluster.RemoteRoot, staging, remoteFiles)
		if err != nil {
			_ = os.RemoveAll(staging)
			for index := range group {
				task := group[index].task
				failure := a.failPull(ctx, &task, "transfer",
					"cannot pull batch task files", err)
				result.Failures = append(result.Failures, BatchFailure{
					TaskID: task.ID, Source: task.SourcePath, Error: fault.As(failure),
				})
			}
			continue
		}
		for index := range group {
			plan := &group[index]
			task := plan.task
			installErr := error(nil)
			for fileIndex, file := range plan.files {
				sourcePath := filepath.Join(staging,
					filepath.FromSlash(plan.remoteFiles[fileIndex]))
				destinationPath := filepath.Join(plan.destination, filepath.FromSlash(file))
				if err := localfs.InstallStagedFile(sourcePath, destinationPath); err != nil {
					installErr = err
					break
				}
			}
			if installErr != nil {
				failure := a.failPull(ctx, &task, "install",
					"cannot install batch result files", installErr)
				result.Failures = append(result.Failures, BatchFailure{
					TaskID: task.ID, Source: task.SourcePath, Error: fault.As(failure),
				})
				continue
			}
			now := time.Now().UTC()
			task.PulledAt, task.UpdatedAt = &now, now
			if terminalComputeState(task.ComputeState) {
				task.PullState = model.PullSucceeded
			} else {
				task.PullState = model.PullPartial
			}
			persistCtx, cancelPersist := a.persistenceContext(ctx)
			err = a.Store.UpdateTaskWithEvent(persistCtx, &task, taskEvent(task,
				"PULL_COMPLETED", "pull", "Batch result transfer completed",
				map[string]string{"files": strconv.Itoa(len(plan.files)), "pull_id": pullID}))
			cancelPersist()
			if err != nil {
				result.Failures = append(result.Failures, BatchFailure{
					TaskID: task.ID, Source: task.SourcePath, Error: fault.As(err),
				})
				continue
			}
			result.Tasks = append(result.Tasks, PullManyItem{
				TaskID: task.ID, Source: task.SourcePath, Files: plan.files,
				Destination: plan.destination, TotalBytes: plan.totalBytes,
				PullState: task.PullState,
			})
		}
		_ = os.RemoveAll(staging)
	}
	order := make(map[string]int, len(tasks))
	for index, task := range tasks {
		order[task.ID] = index
	}
	sort.SliceStable(result.Tasks, func(i, j int) bool {
		return order[result.Tasks[i].TaskID] < order[result.Tasks[j].TaskID]
	})
	sort.SliceStable(result.Failures, func(i, j int) bool {
		return order[result.Failures[i].TaskID] < order[result.Failures[j].TaskID]
	})
	return result, nil
}

func (a *App) refreshTaskBatch(ctx context.Context, tasks []model.Task) ([]model.Task, error) {
	groups := map[string][]int{}
	for index, task := range tasks {
		if terminalComputeState(task.ComputeState) {
			continue
		}
		if task.SchedulerID == "" {
			updated, err := a.refreshTask(ctx, task)
			if err != nil {
				return nil, err
			}
			tasks[index] = updated
			continue
		}
		groups[task.ClusterName] = append(groups[task.ClusterName], index)
	}
	for clusterName, indexes := range groups {
		cluster, ok := a.Config.Clusters[clusterName]
		if !ok {
			return nil, fault.New("CLUSTER_NOT_FOUND",
				fmt.Sprintf("cluster %q is no longer configured", clusterName), false)
		}
		ids := make([]string, 0, len(indexes))
		for _, index := range indexes {
			ids = append(ids, tasks[index].SchedulerID)
		}
		statuses, err := (scheduler.Slurm{Runner: a.Runner}).Statuses(ctx, cluster.Host, ids)
		if err != nil {
			return nil, err
		}
		for _, index := range indexes {
			updated, err := a.applySchedulerStatus(ctx, tasks[index], statuses[tasks[index].SchedulerID])
			if err != nil {
				return nil, err
			}
			tasks[index] = updated
		}
	}
	return tasks, nil
}

func (a *App) listRemoteFilesBatch(
	ctx context.Context,
	tasks []model.Task,
) (map[string][]RemoteFile, error) {
	groups := map[string][]model.Task{}
	for _, task := range tasks {
		groups[task.ClusterName] = append(groups[task.ClusterName], task)
	}
	result := map[string][]RemoteFile{}
	for clusterName, group := range groups {
		cluster, ok := a.Config.Clusters[clusterName]
		if !ok {
			return nil, fault.New("CLUSTER_NOT_FOUND",
				fmt.Sprintf("cluster %q is no longer configured", clusterName), false)
		}
		var command strings.Builder
		for _, task := range group {
			fmt.Fprintf(&command,
				"(cd %s && find . -type f -printf %s) || exit 1; ",
				remote.Quote(path.Join(task.RemoteDir, "work")),
				remote.Quote(task.ID+"\\0%P\\0%s\\0"))
		}
		stdout, stderr, err := a.Runner.Exec(ctx, cluster.Host, command.String(), nil)
		if err != nil {
			return nil, fault.Wrap("REMOTE_LIST_FAILED",
				message("cannot list batch task files", stderr), true, err)
		}
		records := strings.Split(stdout, "\x00")
		for index := 0; index+2 < len(records); index += 3 {
			size, err := strconv.ParseInt(records[index+2], 10, 64)
			if err != nil || size < 0 {
				return nil, fault.New("REMOTE_LIST_FAILED",
					"remote batch file listing returned an invalid size", true)
			}
			result[records[index]] = append(result[records[index]], RemoteFile{
				Path: filepath.ToSlash(records[index+1]), Size: size,
			})
		}
	}
	return result, nil
}
