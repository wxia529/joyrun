# Command Guide

JoyRun commands are non-interactive. Add `--json` for one machine-readable
document on stdout; progress and diagnostics remain on stderr.

## Local daemon

```bash
joyrun daemon start
joyrun daemon status
joyrun daemon logs --lines 100
joyrun daemon stop
joyrun daemon run --foreground
joyrun database upgrade --to stable-2 [--dry-run]
joyrun operation list
joyrun operation show OPERATION_ID
joyrun operation tasks OPERATION_ID
joyrun operation wait OPERATION_ID --until accepted|terminal [--timeout DURATION]
joyrun operation cancel OPERATION_ID
joyrun operation retry OPERATION_ID
```

The daemon is required for all project, task, scheduler, and remote commands.
It runs only on the local machine, owns the IPC endpoint, and is the sole
SQLite/SSH/Slurm controller. If it is unavailable, JoyRun returns
`DAEMON_REQUIRED` instead of opening a direct connection. `daemon stop` drains
the listener; it never cancels Slurm jobs or deletes remote task directories.
Read-only `config`/`target` inspection, version output, daemon control, and
explicit database maintenance are the only local exceptions.

Normal `submit` and `pull` calls admit durable local Operations and return a
`jo_...` identifier; the worker continues after the CLI exits. Use
`operation wait` when the caller explicitly needs to block. Inspect or retry
with `operation`.
Use `joyrun operation tasks OPERATION_ID` to inspect task-level progress for a
batch or pull operation without decoding its result payload.
The daemon refreshes active Tasks and honors opt-in `--auto-pull=completed` or
`terminal`.

## Project and configuration

```bash
joyrun init [directory]
joyrun config path
joyrun config init
joyrun config validate
joyrun doctor TARGET
```

`init` creates the machine-local Project ID used for path-independent task
lookup. `config init` refuses to overwrite an existing configuration. `doctor`
checks SSH, the remote root, Slurm commands, and the selected transfer backend
without submitting a job.

## Targets and capacity

```bash
joyrun target list
joyrun target show TARGET
joyrun target params TARGET
joyrun target nodes TARGET [--partition ALLOWED_NAME]
```

`target nodes` is a timestamped Slurm observation, not a start-time
prediction. An override must appear in `placement.allowed_partitions`.

## Submit and monitor

```bash
joyrun submit SOURCE... -t TARGET [--partition NAME] [--set KEY=VALUE] [--force-new]
joyrun submit SOURCE... -t TARGET [--include GLOB] --dry-run
joyrun submit --glob GLOB -t TARGET
joyrun submit --from FILE -t TARGET
joyrun status SOURCE_OR_TASK [--cached|--refresh]
joyrun status --all [--cached|--refresh]
joyrun watch [--once] [--project ID] [--target TARGET] [--state STATE] [--attention] [--limit N]
joyrun list [SOURCE]
joyrun inspect SOURCE_OR_TASK [--events]
joyrun logs SOURCE_OR_TASK [--file PATH] [--lines N]
joyrun cancel TASK_ID
```

Always preview a new source/target combination. A source path resolves to its
newest task; use the exact `jr_...` ID for cancellation and other mutations.
All submission is daemon-owned. Local admission returns immediately; use
`operation wait` for an explicit acknowledgement boundary.

Normal submission is idempotent. JoyRun fingerprints the immutable input
manifest and execution intent, and a repeated command reuses the existing Task
without calling Slurm again. This protects retries after an SSH timeout or lost
response. Use `--force-new` only when the user explicitly wants another run of
the same inputs; it creates a new `jr_...` Task and scheduler job.

List multiple Source paths directly as the primary batch interface. They may
have unrelated directories and filenames:

```bash
joyrun submit \
  benzene/opt.inp \
  water/frequency.inp \
  methane/single-point.inp \
  -t TARGET \
  --dry-run
```

Use `--glob` only for Sources with a reliable shared pattern. Use `--from` for
a reviewed list containing one Source path per line; blank lines and `#`
comments are ignored, and paths resolve from the current working directory.
One resolved Source uses the single-task path; 2–100 Sources use transport
batching. All Sources share the Target, partition, parameters, and includes.
JoyRun validates all Sources before SSH and preserves one independent Task and
Slurm job for each Source.
`status --all` batches active scheduler IDs by cluster. In daemon mode normal
status is cache-first; `--cached` forces a local-only view, while `--refresh`
makes the remote refresh explicit. Use exact
`status TASK_ID` when a task has no scheduler ID and needs reconciliation.

## Inspect and pull files

```bash
joyrun files SOURCE_OR_TASK
joyrun pull SOURCE_OR_TASK --dry-run
joyrun pull SOURCE_OR_TASK
joyrun pull SOURCE_OR_TASK --include GLOB
joyrun pull SOURCE_OR_TASK --all
joyrun pull SOURCE_OR_TASK --live --include GLOB
joyrun pull SOURCE_OR_TASK... [--glob GLOB] [--from FILE] [--dry-run]
joyrun pull --batch BATCH_ID [--dry-run]
```

Default patterns are frozen in the Task at submission. Submitted inputs are
protected even with `--all`; `--overwrite-inputs` is required to replace them.
Use `--live` only for deliberate diagnostics before computation completes.
Batch pull detects two Tasks writing the same local path before transfer.
`--batch` selects all Tasks from one multi-source submission. Otherwise select
Task IDs or Source paths explicitly. `--batch` and explicit selectors are
mutually exclusive. A batch contains at most 100 Tasks.

In JSON mode, successful non-preview submission and pull results expose
`tasks` and `failures` arrays. Submit preview exposes `previews`; a total
command failure uses the top-level error envelope. `submit` adds `batch_id`
only when multiple Sources are submitted.

## Recovery

```bash
joyrun recover --scan -t TARGET
joyrun recover TASK_ID -t TARGET
```

Scanning finds remote metadata matching the current Project ID and never
imports automatically. Recover imports one selected task into the global
SQLite index; it does not resubmit computation.
