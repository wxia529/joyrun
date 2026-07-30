# Command Guide

JoyRun commands are non-interactive. Add `--json` for one machine-readable
document on stdout; progress and diagnostics remain on stderr.

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
joyrun submit SOURCE... -t TARGET [--partition NAME] [--set KEY=VALUE]
joyrun submit SOURCE... -t TARGET [--include GLOB] --dry-run
joyrun submit --glob GLOB -t TARGET
joyrun submit --from FILE -t TARGET
joyrun status SOURCE_OR_TASK
joyrun status --all
joyrun list [SOURCE]
joyrun inspect SOURCE_OR_TASK [--events]
joyrun logs SOURCE_OR_TASK [--file PATH] [--lines N]
joyrun cancel TASK_ID
```

Always preview a new source/target combination. A source path resolves to its
newest task; use the exact `jr_...` ID for cancellation and other mutations.
Submission is asynchronous and returns after Slurm acceptance is recorded.

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
`status --all` batches active scheduler IDs by cluster. Use exact
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
