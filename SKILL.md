---
name: joyrun
description: Install, configure, update, and operate JoyRun as an agent-friendly remote HPC execution backend over SSH and Slurm. Use when Codex needs to install or check a JoyRun release, create or modify cluster and target configuration from an existing HPC submission workflow, initialize a JoyRun project, inspect configured execution targets, preview or submit a local file/directory to an HPC cluster, check a remote task, read logs, pull selected results, cancel an explicitly identified task, inspect task history, diagnose JoyRun connectivity, or recover a task from remote metadata. Prefer this skill for computational work that should run asynchronously on a configured remote cluster instead of consuming the local machine.
---

# Use JoyRun

This Skill is released for JoyRun `__JOYRUN_VERSION__`. If the installed
binary reports another stable version, install the matching Skill before
operating it.

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

   If it is unavailable, do not guess an archive name or silently install it.
   When the user explicitly requested installation, download the official
   standalone installer as a file, then invoke it so platform detection and
   SHA-256 verification remain centralized:

   Linux/macOS:

   ```bash
   curl -fsSLO \
     https://github.com/wxia529/joyrun/releases/latest/download/install.sh
   sh install.sh
   ```

   Windows PowerShell:

   ```powershell
   Invoke-WebRequest `
     https://github.com/wxia529/joyrun/releases/latest/download/install.ps1 `
     -OutFile install.ps1
   powershell.exe -NoProfile -ExecutionPolicy Bypass `
     -File .\install.ps1
   ```

   If the user asked to use JoyRun but did not authorize installing software,
   explain that JoyRun is missing and request approval before downloading or
   changing files outside the project. Never pipe a remote script into a
   shell, use `sudo`, bypass checksum verification, or install from a
   non-official repository. Do not add a directory to PATH without explicit
   authorization; invoke the installed absolute path when necessary.

   The Windows execution-policy setting applies only to that child process; do
   not modify the persisted user or machine policy. After installation, verify
   by name when available or by the reported absolute installation path:

   ```bash
   joyrun version --json
   "$HOME/.local/bin/joyrun" version --json
   ```

   ```powershell
   & "$env:LOCALAPPDATA\Programs\JoyRun\joyrun.exe" version --json
   ```

   If configuration is missing, locate or create it explicitly:

   ```bash
   joyrun config path --json
   joyrun config init --json
   joyrun config validate --json
   ```

2. Locate `.joyrun/project.yaml` in the current directory or an ancestor. If
   the user asked to use JoyRun and the project is not initialized, run:

   ```bash
   joyrun init --json
   ```

3. Discover targets instead of guessing:

   ```bash
   joyrun target list --json
   joyrun target show TARGET --json
   joyrun target params TARGET --json
   ```

4. If multiple targets are plausible and the user's intent does not determine
   one, ask the user to choose. Never infer an expensive target from a filename
   extension alone.

## Create or modify configuration

When asked to create a cluster or target, read and follow the complete
configuration guide before editing:

https://github.com/wxia529/joyrun/releases/download/__JOYRUN_VERSION__/agent-configuration.md

Treat an existing Slurm script and cluster documentation as the source of
truth. Do not guess cluster-specific values, overwrite unrelated configuration,
store credentials, or submit a real job while configuring JoyRun. Finish with
`config validate`, `target show`, `target params`, `doctor`, and a representative
`submit --dry-run`. A real submission requires a separate explicit request.

When the user asks about available nodes or current capacity, use:

```bash
joyrun target nodes TARGET --json
```

Select another declared placement only when needed:

```bash
joyrun target nodes TARGET --partition APPROVED_PARTITION --json
```

Never invent a partition, bypass `placement.allowed_partitions`,
automatically change a submission, or promise that idle nodes guarantee
immediate scheduling.

## Check and upgrade JoyRun

Do not check for updates during normal task operations. Check only when the
user requests version or update information. Do not assume an installer from a
previous session still exists or is current; download a fresh official
installer before checking or upgrading:

```bash
curl -fsSLO \
  https://github.com/wxia529/joyrun/releases/latest/download/install.sh
sh install.sh --check
```

```powershell
Invoke-WebRequest `
  https://github.com/wxia529/joyrun/releases/latest/download/install.ps1 `
  -OutFile install.ps1
powershell.exe -NoProfile -ExecutionPolicy Bypass `
  -File .\install.ps1 -Check
```

The check is read-only. Upgrade only when the user explicitly requests it by
running the same official installer without `--check`/`-Check`. Use an exact
version when the user requests reproducibility:

```bash
sh install.sh --version vX.Y.Z
```

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass `
  -File .\install.ps1 -Version vX.Y.Z
```

The default installer channel is the latest stable release and never selects a
prerelease. Before upgrading across a documented database compatibility
boundary, stop and report the compatibility requirement; never delete,
reinitialize, or silently migrate the user's database. After upgrading, run
`joyrun version --json`. A checksum, platform, or version-verification failure
is blocking and must not be bypassed.

## Validate before submission

Run a local-only preview before the first submission of a source/target
combination, after changing target parameters, or after editing JoyRun config:

```bash
joyrun submit SOURCE -t TARGET --dry-run --json
```

Inspect:

- resolved source kind, work directory, and entry file;
- resolved `.Input`, `.Stem`, `.Name`, and `.WorkDir` values;
- declared software identity and resolved partition facts;
- files included in the upload snapshot;
- ignored files;
- `push.mode`, explicit dependency patterns, and upload limits;
- resolved parameters and their sources;
- remote directory;
- rendered job script.

Do not submit if required inputs are absent, unintended large files are
included, or the rendered command does not match the requested calculation.
Treat `SOURCE_KIND_MISMATCH` and `SOURCE_PATTERN_MISMATCH` as input-selection
errors; do not work around them by submitting a containing directory to a file
target.

For `push.mode: entry`, confirm that only the selected input and intentional
dependencies are present. Treat target `push.include` as dependencies required
by every run. Add optional coordinates or restart data for this task with
repeatable, preferably exact `--include` values:

```bash
joyrun submit SOURCE -t TARGET \
  --include structure.xyz \
  --include previous.gbw \
  --dry-run --json
```

Never include a checkpoint, wavefunction, or restart file merely because one
exists beside the input; first establish that the requested calculation uses
it. JoyRun rejects missing requested dependencies locally. For
`push.mode: workdir`, inspect the whole manifest because every non-excluded
file is part of the task. Never add
`--allow-project-root` merely to bypass `PROJECT_ROOT_UPLOAD_FORBIDDEN`; use it
only when the user explicitly intends to upload from the complete project
root.

## Review scientific resource consistency

JoyRun exposes facts; it does not interpret scientific input or decide resource
fitness. Before a real submission, the agent must:

1. read `joyrun target show TARGET --json` and `target params`;
2. read the selected scientific input and identify only explicit
   software-specific core, process, thread, and memory directives;
3. inspect `submit --dry-run --json`, including `software`, `partition`,
   resolved params, upload manifest, and rendered `#SBATCH` requests;
4. compare the input directives with the rendered allocation and configured
   partition facts.

Stop before submission when the software identity is inconsistent, an explicit
input request exceeds the rendered allocation, or the rendered request cannot
fit a known per-node fact. Report the exact conflict and ask the user whether
to change the input, Target params, or partition. Never silently edit input or
resource parameters.

Missing facts mean **unknown**, not invalid. If `memory_per_node` is absent,
state that memory compatibility could not be verified; do not invent a machine
value or a per-software memory estimate. A plausible but inefficient allocation
is a warning, not a JoyRun error. Query `target nodes` only when the user asks
about current capacity or when choosing among already allowed partitions is
part of the request; live idleness is not resource validation.

Before the first real use of a target on the current machine, run:

```bash
joyrun doctor TARGET --json
```

Inspect `result.ready` and every item in `result.checks`. `warn` checks are
non-blocking; `fail` checks are blocking and make the command exit nonzero.

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

Supply optional input dependencies with repeatable `--include` flags:

```bash
joyrun submit SOURCE -t TARGET \
  --include structure.xyz \
  --json
```

Record the returned `result.task.id`, which has the form `jr_...`. Use this
exact task ID for all subsequent operations. A source path resolves to its
newest task and can silently select a later submission.

The target controls upload scope. `push.mode: entry` snapshots only the
selected file, always-required target dependencies, and explicit
per-submission `--include` dependencies. `push.mode: workdir` snapshots the
directory. Respect upload limits, `.joyrunignore`, and target `push.exclude`;
do not work around them without user intent. Treat
`UPLOAD_POLICY_EXCEEDED`, `SOURCE_ENTRY_EXCLUDED`, and
`PROJECT_ROOT_UPLOAD_FORBIDDEN` as safety failures requiring review, not flags
to bypass automatically.

Submission is asynchronous. Return control after JoyRun returns the task ID.
Do not keep a terminal occupied waiting for a long computation.

## Monitor and diagnose

Query an exact task:

```bash
joyrun status jr_TASK_ID --json
```

Interpret `compute_state`:

- `created`: local task exists but scheduler acceptance is not confirmed;
- `submission_failed`: submission pipeline failed; check status before considering
  another submission;
- `submission_uncertain`: `sbatch` may have succeeded but its scheduler ID was
  not recovered; query the exact Task with `status` and never resubmit it
  blindly;
- `queued`: accepted and waiting for resources;
- `running`: executing remotely;
- `completed`: computation finished and normal pull is allowed;
- `failed`: remote computation or scheduler failed;
- `cancelled`: explicitly cancelled;
- `unknown`: scheduler has no usable state; investigate before acting.

Interpret `pull_state` independently:

- `not_pulled`: no successful pull has been recorded;
- `pulling`: a pull is in progress;
- `pulled`: the latest requested file set was pulled successfully;
- `partial`: files were pulled with `--live`;
- `failed`: the latest pull failed; computation state is unchanged.

Do not interpret `pull_state` as a claim that all remote files still exist or
that every scientifically important file has been downloaded.

At the start of a new session, restore context with:

```bash
joyrun list --json
joyrun inspect jr_TASK_ID --json
joyrun inspect jr_TASK_ID --events --json
```

Use `joyrun status --all --json` to refresh all non-terminal tasks. Status
refresh never submits a replacement task.

Read logs without downloading the complete result:

```bash
joyrun logs jr_TASK_ID --lines 200 --json
joyrun logs jr_TASK_ID --file alternate.log --lines 200 --json
```

JoyRun selects an existing configured application log and automatically falls
back to its reserved scheduler log. `LOG_NOT_READY` is retryable and lists the
paths checked.

Do not poll in a tight loop. Check only when requested or after a meaningful
interval appropriate to the calculation.

If a task fails, inspect status and logs first. Do not modify inputs or submit
a replacement automatically unless the user authorized an iterative compute
workflow.

## Pull results safely

For any terminal compute state—`completed`, `failed`, or `cancelled`—normal
pull is allowed. Inspect status and logs before pulling a failed or cancelled
task because its outputs may be incomplete. Use the target's default patterns:

```bash
joyrun pull jr_TASK_ID --json
```

Inspect remote paths and preview the exact pull first when file sizes or
artifact selection are uncertain:

```bash
joyrun files jr_TASK_ID --json
joyrun pull jr_TASK_ID --dry-run --json
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
replacing them. Other selected files are outputs and may replace same-named
local outputs. Before pulling a historical task or into a directory containing
newer results, inspect `files`, run `pull --dry-run`, and confirm that replacing
those outputs matches the user's intent.

Use `--live` only for deliberate diagnostic retrieval from a task that has not
completed:

```bash
joyrun pull jr_TASK_ID --live --include "partial.log" --json
```

A transfer failure does not imply computation failure. Retry `pull`; do not
resubmit the calculation.

`NO_FILES_MATCHED` means the selected pull policy found no transferable
outputs. Inspect the task and use an appropriate `--include` pattern or
`--all`; do not treat an empty selection as retrieved results.

## History, cancellation, and recovery

Pass a source to `list` to inspect repeated submissions:

```bash
joyrun list SOURCE --json
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

If the task ID is unknown, discover candidates scoped to the current Project
ID and Target without opening the local task database:

```bash
joyrun recover --scan -t TARGET --json
```

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

Report the error code, task ID if available, compute state, pull state, and
safest next action to the user.
