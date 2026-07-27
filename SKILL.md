---
name: joyrun
description: Operate JoyRun as an agent-friendly remote HPC execution backend over SSH and Slurm. Use when Codex needs to initialize a JoyRun project, inspect configured execution targets, preview or submit a local file/directory to an HPC cluster, check a remote task, read logs, pull selected results, cancel an explicitly identified task, inspect task history, diagnose JoyRun connectivity, or recover a task from remote metadata. Prefer this skill for computational work that should run asynchronously on a configured remote cluster instead of consuming the local machine.
---

# Use JoyRun

Use JoyRun as an execution layer. Prepare and analyze files locally; let JoyRun
perform transport, Slurm submission, state tracking, and result retrieval.

Run commands from the JoyRun project root or one of its descendants. Use
`--json` for every command whose output will be interpreted programmatically.
Treat stdout as the JSON interface and stderr as diagnostics.

## Establish context

1. Confirm that `joyrun` is available:

   ```bash
   joyrun version --json
   ```

2. Locate `.joyrun/project.yaml` in the current directory or an ancestor. If
   the user asked to use JoyRun and the project is not initialized, run:

   ```bash
   joyrun init --json
   ```

3. Discover targets instead of guessing:

   ```bash
   joyrun targets --json
   joyrun target show TARGET --json
   joyrun target params TARGET --json
   ```

4. If multiple targets are plausible and the user's intent does not determine
   one, ask the user to choose. Never infer an expensive target from a filename
   extension alone.

## Validate before submission

Run a local-only preview before the first submission of a source/target
combination, after changing target parameters, or after editing JoyRun config:

```bash
joyrun submit SOURCE -t TARGET --dry-run --json
```

Inspect:

- resolved source and work directory;
- files included in the snapshot;
- ignored files;
- resolved parameters and their sources;
- remote directory;
- rendered job script.

Do not submit if required inputs are absent, unintended large files are
included, or the rendered command does not match the requested calculation.

Before the first real use of a target on the current machine, run:

```bash
joyrun doctor TARGET --json
```

Inspect `result.ok` and every item in `result.checks`. Do not rely only on the
process exit code or the outer JSON `ok`, because doctor reports failed checks
as data.

## Submit

Submit only after preview succeeds:

```bash
joyrun submit SOURCE -t TARGET --json
```

Supply declared target parameters with repeatable flags:

```bash
joyrun submit SOURCE -t TARGET \
  --set cpus=64 \
  --set memory=220G \
  --json
```

Record the returned `result.task.id`, which has the form `jr_...`. Use this
exact task ID for all subsequent operations. A source path resolves to its
newest task and can silently select a later submission.

Submitting a file snapshots its entire containing directory. Submitting a
directory snapshots that directory. Respect `.joyrunignore` and target
`push.exclude`; do not work around them without user intent.

Submission is asynchronous. Return control after JoyRun returns the task ID.
Do not keep a terminal occupied waiting for a long computation.

## Monitor and diagnose

Query an exact task:

```bash
joyrun status jr_TASK_ID --json
```

Interpret normalized states:

- `queued`: accepted and waiting for resources;
- `running`: executing remotely;
- `completed`: computation finished and normal pull is allowed;
- `failed`: remote computation or scheduler failed;
- `cancelled`: explicitly cancelled;
- `unknown`: scheduler has no usable state; investigate before acting.

Read logs without downloading the complete result:

```bash
joyrun logs jr_TASK_ID --lines 200 --json
```

Do not poll in a tight loop. Check only when requested or after a meaningful
interval appropriate to the calculation.

If a task fails, inspect status and logs first. Do not modify inputs or submit
a replacement automatically unless the user authorized an iterative compute
workflow.

## Pull results safely

After `completed`, use the target's default artifact policy:

```bash
joyrun pull jr_TASK_ID --json
```

Prefer explicit subsets when only particular outputs are required:

```bash
joyrun pull jr_TASK_ID \
  --include "*.out" \
  --include "*.xyz" \
  --json
```

Use `--all` only when the user needs every generated file; HPC outputs can be
very large:

```bash
joyrun pull jr_TASK_ID --all --json
```

Submitted input files are protected by default. Never use
`--overwrite-inputs` unless the user explicitly requests restoring or
replacing them.

Use `--live` only for deliberate diagnostic retrieval from a task that has not
completed:

```bash
joyrun pull jr_TASK_ID --live --include "partial.log" --json
```

A transfer failure does not imply computation failure. Retry `pull`; do not
resubmit the calculation.

## History, cancellation, and recovery

Use history to inspect repeated submissions:

```bash
joyrun history SOURCE --json
```

Cancel only when the user explicitly requests cancellation, and use an exact
task ID:

```bash
joyrun cancel jr_TASK_ID --json
```

If the local SQLite index is lost but the task ID and target are known, import
the remote recovery metadata:

```bash
joyrun recover jr_TASK_ID -t TARGET --json
```

Then query status again using the recovered task ID.

## Handle errors

For a failed JSON command, inspect:

```json
{
  "ok": false,
  "error": {
    "code": "ERROR_CODE",
    "message": "diagnostic",
    "retryable": true
  }
}
```

Retry only the failed stage when `retryable` is true:

- retry `status`, `logs`, or `pull` after transient SSH failures;
- retry transfer after `UPLOAD_FAILED` or `PULL_FAILED` only after checking the
  task record;
- never blindly repeat `submit`, because the scheduler may already have
  accepted the job;
- do not treat `PULL_FAILED` as a reason to recompute.

Report the error code, task ID if available, compute state, and safest next
action to the user.
