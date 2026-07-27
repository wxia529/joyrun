# JoyRun

JoyRun is a local-first remote task runner for HPC:

```text
local project -> OpenSSH -> offline HPC -> Slurm -> rsync/SFTP results back
```

Give it a local source and a user-defined target. JoyRun snapshots the source
directory into an isolated remote task, submits it without blocking, tracks the
Slurm state, and pulls selected results back beside the original input.

```bash
joyrun submit task01/eg.inp -t gibbs/orca
joyrun status task01/eg.inp
joyrun pull task01/eg.inp
```

JoyRun does not know what ORCA, VASP, or another scientific program means.
Targets are complete user-owned job-script templates.

## Scope and responsibilities

JoyRun is an execution and transport layer. It is responsible for:

- snapshotting a local source into an isolated remote task;
- transferring files, submitting Slurm jobs, and querying scheduler state;
- recording task state, submission provenance, and lifecycle events;
- safely pulling user-selected results back to the source directory.

The user or calling agent is responsible for:

- choosing the target, resources, software environment, and artifact policy;
- providing a correct job script;
- interpreting scientific output;
- deciding whether and how to modify, restart, or resubmit a calculation.

JoyRun does not interpret results, modify inputs, restart failed calculations,
build workflow DAGs, or expose a general-purpose remote shell. It has no
required daemon: remote actions occur only when a JoyRun command is run.

## Build

JoyRun requires Go 1.24 or later at build time and the OpenSSH `ssh` command
at runtime. On Linux and macOS, `rsync` is preferred when it is available on
both the local and remote systems. Native Windows uses SFTP over the bundled
OpenSSH client and does not require WSL, Cygwin, MSYS2, or rsync.

```bash
git clone git@github.com:wxia529/joyrun.git
cd joyrun
make build
./bin/joyrun version
```

Cross-compile a native Windows binary from Linux or macOS with:

```bash
GOOS=windows GOARCH=amd64 go build -trimpath -o joyrun.exe ./cmd/joyrun
```

## Configure

Copy [the example configuration](examples/config.yaml) to:

```text
~/.config/joyrun/config.yaml
```

`$XDG_CONFIG_HOME` is honored. `JOYRUN_CONFIG` or the global `--config` option
can select another file.

A cluster describes connectivity and scheduling:

```yaml
clusters:
  gibbs:
    host: gibbs
    scheduler: slurm
    remote_root: /scratch/your-user/joyrun
    transfer: auto
```

`host` is an OpenSSH alias, so existing keys, `ssh-agent`, `ProxyJump`,
`known_hosts`, ports, and usernames continue to work. JoyRun never stores
credentials.

`transfer` accepts:

- `auto` — Windows uses SFTP; Linux/macOS use rsync when both ends provide it,
  otherwise SFTP
- `rsync` — require rsync explicitly
- `sftp` — use the OpenSSH SFTP subsystem explicitly

SFTP uploads use temporary remote files followed by rename. Downloads use a
temporary file in the destination directory followed by an atomic replacement.
The rsync backend remains available with its native partial-transfer support.

A target describes one way to run software on a cluster:

```yaml
targets:
  gibbs/orca:
    cluster: gibbs
    source:
      kind: file
      patterns: ["*.inp"]
    params:
      cpus:
        type: int
        default: 32
    script: |
      #!/bin/bash
      #SBATCH --cpus-per-task={{ .Params.cpus }}
      #SBATCH --job-name={{ .Stem }}
      orca {{ .Input }} > {{ .Stem }}.out
    push:
      exclude: ["*.out", "*.gbw"]
    pull:
      default: ["*.out", "*.xyz", "*.gbw"]
    logs: ["{{ .Stem }}.out"]
```

`source.kind` is the target's input contract:

- `file` — submission must name one concrete entry file
- `directory` — submission must name a directory
- `either` — both forms are accepted

Optional `source.patterns` validates file entry names. JoyRun rejects a
directory submitted to a file target before rendering or creating a task, with
an actionable command that names the sole matching candidate when possible.
Every target must declare `source.kind` explicitly. JoyRun does not infer an
input contract from the script.

Public template values are:

- `{{ .Input }}` — entry filename for a `file` source
- `{{ .Stem }}` — entry filename without its final extension
- `{{ .Name }}` — source working-directory name
- `{{ .TaskID }}` — JoyRun task ID
- `{{ .WorkDir }}` — absolute remote working directory
- `{{ .Params.name }}` — resolved target parameter

Script substitutions are POSIX-shell quoted automatically. Templates
intentionally allow only these direct substitutions. Pipelines,
functions, conditionals, loops, and declarations are rejected while loading
the configuration.

Parameters support `string`, `int`, `float`, and `bool`, plus `default`,
`required`, `choices`, and `description`. Override declared parameters with a
repeatable `--set`:

```bash
joyrun submit task01/eg.inp -t gibbs/orca \
  --set cpus=64
```

## Initialize and preview

Initialize a project once:

```bash
cd my-project
joyrun init
```

This creates `.joyrun/project.yaml` with a stable Project ID. The global SQLite
database uses that ID plus source-relative paths, so moving the entire project
does not break task lookup.

Preview performs local validation and makes no SSH connection or database task
record:

```bash
joyrun submit task01/eg.inp -t gibbs/orca --dry-run
```

It shows source kind, work directory, entry file, resolved template values and
parameters, the upload snapshot, planned remote directory, and rendered
script. A source-contract mismatch is a hard preview error rather than a
script containing empty `.Input` or `.Stem` values.

## Commands

```bash
joyrun init [directory]
joyrun target list
joyrun target show gibbs/orca
joyrun doctor gibbs/orca

joyrun submit task01/eg.inp -t gibbs/orca
joyrun status task01/eg.inp
joyrun status --all
joyrun list [task01/eg.inp]
joyrun inspect jr_TASK_ID
joyrun inspect jr_TASK_ID --events
joyrun logs task01/eg.inp --lines 200
joyrun pull task01/eg.inp
joyrun cancel jr_TASK_ID
```

JoyRun reserves `joyrun-slurm-<jobid>.log` for scheduler diagnostics. `logs`
first reads the first configured application log that exists, then falls back
to this scheduler log. For tasks created by older JoyRun versions it also
checks `slurm-<jobid>.out`. If no candidate exists yet, the retryable
`LOG_NOT_READY` error lists every checked path.

`doctor` reports checks as `PASS`, `WARN`, or `FAIL`. A missing `remote_root`
whose nearest existing ancestor is writable is a non-blocking warning because
the first submission will create it. An existing but unwritable root, or a
root that cannot be created, is blocking, includes a suggested action, and
causes a nonzero process exit status.

A source path addresses its newest task. A `jr_...` task ID addresses one exact
historical run:

```bash
joyrun status jr_TASK_ID
joyrun pull jr_TASK_ID
```

Use source paths for convenient lookup and exact Task IDs for mutations such
as cancellation. `cancel` rejects source paths so a later submission cannot be
cancelled accidentally.

JoyRun records compute state and pull progress independently:

- `compute_state`: `created`, `submission_failed`, `queued`, `running`,
  `completed`, `failed`, `cancelled`, or `unknown`
- `pull_state`: `not_pulled`, `pulling`, `pulled`, `partial`, or `failed`

For example, a completed calculation whose download failed remains
`compute_state=completed` and becomes `pull_state=failed`. Retry
`pull`; do not recompute it.

`pull_state` describes only the latest requested pull operation. It does not
claim that every remote file still exists or that every scientifically
important file has been downloaded.

`list` restores the project's task view across sessions; an optional source
shows its submission history. `inspect` returns the immutable submission
snapshot, including parameters, manifest, and rendered script.
`inspect --events` includes the append-only lifecycle events. `status --all`
queries Slurm for all non-terminal tasks and records observed transitions
without creating a new calculation.

For a file source, JoyRun uploads the file's entire containing directory.
For a directory source, it uploads the directory contents. `.joyrunignore`,
target `push.exclude`, and a built-in `.joyrun/` exclusion control the snapshot.

Default pull patterns come from the task's target snapshot:

```bash
joyrun pull task01/eg.inp
joyrun pull task01/eg.inp --include "*.out" --include "*.xyz"
joyrun pull task01/eg.inp --all
```

Files present in the submitted input manifest are protected even with `--all`.
Use `--overwrite-inputs` explicitly to replace them. Output files may be
updated normally. `--live` permits pulling currently available output before
the task completes.

## Agent/JSON interface

All operational commands are non-interactive. Add `--json` anywhere in a
command to receive one JSON document on stdout:

```bash
joyrun status task01/eg.inp --json
```

Success:

```json
{"ok":true,"result":{"id":"jr_...","compute_state":"running","pull_state":"not_pulled"}}
```

Failure:

```json
{
  "ok": false,
  "error": {
    "code": "SSH_FAILED",
    "message": "cannot connect to gibbs",
    "retryable": true,
    "stage": "upload",
    "suggested_action": "joyrun status jr_...",
    "compute_state": "submission_failed",
    "pull_state": "not_pulled"
  }
}
```

Progress and diagnostics use stderr and never contaminate JSON stdout.

## Data and recovery

The primary task database is:

```text
$XDG_DATA_HOME/joyrun/joyrun.db
```

or `~/.local/share/joyrun/joyrun.db` when `XDG_DATA_HOME` is unset.
`JOYRUN_DB` can override it.

The current database format is explicitly development-only. Its metadata is
marked `release_channel=development` and `schema_label=dev-1`. JoyRun rejects
older or differently marked databases instead of migrating them.

On Windows the defaults are:

```text
%APPDATA%\joyrun\config.yaml
%LOCALAPPDATA%\joyrun\joyrun.db
```

Windows pulls reject reserved filenames, unsupported characters, and
case-insensitive collisions such as `A.out` and `a.out` rather than silently
overwriting data.

Every remote task contains a `metadata.json` recovery snapshot. If the local
database is lost, recover a known task ID with:

```bash
joyrun recover jr_TASK_ID -t gibbs/orca
```

See [the v0.1 design notes](docs/design.md) for the state and recovery model.
