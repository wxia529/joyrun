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

## Install with an AI agent

Copy and send this prompt to your coding agent:

```text
Install https://raw.githubusercontent.com/wxia529/joyrun/main/SKILL.md as a global user-level JoyRun skill so it is available in all projects, then follow it to install the latest stable JoyRun release for this machine.
```

The prompt explicitly requests a global user-level skill rather than a
project-local instruction. The skill tells the agent how to detect the
platform, install without guessing an archive name, and stop for authorization
or compatibility problems. It does not authorize silent installation or
upgrades during normal JoyRun task operations. You can also
[review the skill on GitHub](https://github.com/wxia529/joyrun/blob/main/SKILL.md)
before using it.

## Install and build

The official installers detect the operating system and CPU architecture,
download the matching archive from the latest stable
[GitHub release](https://github.com/wxia529/joyrun/releases), verify it against
`SHA256SUMS`, and install it without `sudo`.

Linux or macOS:

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
  -File .\install.ps1 -AddToPath
```

Download the installer as a file and inspect it when required by local
security policy; do not pipe a remote script directly into a shell.
`ExecutionPolicy Bypass` above applies only to the child PowerShell process
running this downloaded file; it does not change the user or machine policy.
By default, JoyRun is installed in `~/.local/bin` on Linux/macOS and
`%LOCALAPPDATA%\Programs\JoyRun` on Windows.

Re-running the same installer performs a verified upgrade. Check first without
changing files, or pin an exact release:

```bash
sh install.sh --check
sh install.sh --version v0.1.0
```

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass `
  -File .\install.ps1 -Check
powershell.exe -NoProfile -ExecutionPolicy Bypass `
  -File .\install.ps1 -Version v0.1.0
```

The default always resolves GitHub's latest stable release; it never selects a
prerelease automatically. Installation keeps the immediately previous binary
as `joyrun.previous` (or `joyrun.previous.exe`) for manual rollback. The
installer does not modify PATH on Linux/macOS, and Windows PATH changes require
the explicit `-AddToPath` option. See the
[installation and upgrade guide](docs/install.md) for platform details and
agent-safety rules.

Alternatively, install from source with Go 1.24 or later:

```bash
go install github.com/wxia529/joyrun/cmd/joyrun@latest
```

JoyRun requires the OpenSSH `ssh` command at runtime. On Linux and macOS,
`rsync` is preferred when it is available on both the local and remote
systems. Native Windows uses SFTP over the bundled OpenSSH client and does not
require WSL, Cygwin, MSYS2, or rsync.

To build or install a checkout:

```bash
git clone git@github.com:wxia529/joyrun.git
cd joyrun
make build
./bin/joyrun version
sudo make install
```

Cross-compile a native Windows binary from Linux or macOS with:

```bash
GOOS=windows GOARCH=amd64 go build -trimpath -o joyrun.exe ./cmd/joyrun
```

## Configure

Find or create the user configuration without manually locating a
platform-specific directory:

```bash
joyrun config path
joyrun config init
joyrun config validate
```

`config init` refuses to overwrite an existing file. It creates a commented,
valid starter at the path printed by `config path`.

Alternatively, copy [the complete example configuration](examples/config.yaml)
to:

```text
~/.config/joyrun/config.yaml
```

`$XDG_CONFIG_HOME` is honored. `JOYRUN_CONFIG` or the global `--config` option
can select another file.

### Configure with an AI agent

Copy this prompt to your coding agent and replace the placeholders:

```text
Follow https://raw.githubusercontent.com/wxia529/joyrun/main/SKILL.md to create a JoyRun target from my existing Slurm script at <SCRIPT_PATH> for inputs under <SOURCE_PATH>. Do not guess cluster-specific values, submit a real job, or overwrite unrelated configuration. Validate the configuration, run doctor, and finish with a dry-run.
```

The Skill routes configuration work to the detailed
[Agent configuration guide](docs/agent-configuration.md). The agent should ask
only for values that cannot be established from the existing script or cluster
documentation.

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
      mode: entry
      include: ["*.xyz"]
      limits:
        max_files: 20
        max_total_size: 2GiB
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

Every target must also declare an upload boundary:

- `push.mode: entry` uploads only the selected source file plus files matching
  `push.include`; it is restricted to `source.kind: file` targets.
- `push.mode: workdir` uploads the complete source working directory after
  exclusions.

`push.exclude`, the project `.joyrunignore`, and built-in `.joyrun/` and
`.git/` rules take precedence over inclusion. Optional
`push.limits.max_files` and `push.limits.max_total_size` reject unexpectedly
large snapshots locally. Size values accept units such as `2GB` and `2GiB`.

JoyRun rejects any source whose working directory is the project root. This
protects agents from accidentally uploading the entire project. Use
`--allow-project-root` only when that scope is deliberate.

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

This creates `.joyrun/project.yaml` with a stable Project ID and a
`.joyrun/.gitignore` that keeps the machine-local identity out of Git. The
global SQLite database uses that ID plus source-relative paths, so moving the
entire project does not break task lookup while a fresh Git clone receives a
new identity when initialized.

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
joyrun config path
joyrun config init
joyrun config validate
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
joyrun logs jr_TASK_ID --file alternate.log
joyrun files jr_TASK_ID
joyrun pull task01/eg.inp
joyrun pull jr_TASK_ID --dry-run
joyrun cancel jr_TASK_ID
```

JoyRun reserves `joyrun-slurm-<jobid>.log` for scheduler diagnostics. `logs`
first reads the first configured application log that exists, then falls back
to this scheduler log. For tasks created by older JoyRun versions it also
checks `slurm-<jobid>.out`. If no candidate exists yet, the retryable
`LOG_NOT_READY` error lists every checked path.
Use `--file PATH` to select a specific log relative to the remote task work
directory.

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

Status also records the raw Slurm state, elapsed time, start/end timestamps,
pending/failure reason, and exit code when Slurm provides them. Human output
exposes these fields directly; JSON includes `scheduler_state`,
`scheduler_reason`, `elapsed`, `exit_code`, `scheduler_start`, and
`scheduler_end`.

`pull_state` describes only the latest requested pull operation. It does not
claim that every remote file still exists or that every scientifically
important file has been downloaded.

`list` restores the project's task view across sessions; an optional source
shows its submission history. `inspect` returns the immutable submission
snapshot, including parameters, manifest, and rendered script.
`inspect --events` includes the append-only lifecycle events. `status --all`
queries Slurm for all non-terminal tasks and records observed transitions
without creating a new calculation.

The target's `push.mode` determines whether JoyRun uploads only the selected
file and explicit dependencies or the directory contents. The resulting input
manifest is frozen in the Task. Preview the exact bounded snapshot before
submission:

```bash
joyrun submit task01/eg.inp -t gibbs/orca --dry-run
```

Default pull patterns come from the task's target snapshot:

```bash
joyrun files jr_TASK_ID
joyrun pull jr_TASK_ID --dry-run
joyrun pull task01/eg.inp
joyrun pull task01/eg.inp --include "*.out" --include "*.xyz"
joyrun pull task01/eg.inp --all
```

`files` lists remote paths, sizes, and which paths were submitted inputs.
`pull --dry-run` applies the real pull policy and input protection, validates
the local destination, and reports the exact files and total bytes without
transferring them or changing `pull_state`.

If no remote files match, JoyRun returns `NO_FILES_MATCHED` instead of
recording an empty pull as successful.

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
Snapshot preparation and transfers report their current phase there. rsync
uses aggregate byte progress; SFTP reports each file and its size.

## Data and recovery

The primary task database is:

```text
$XDG_DATA_HOME/joyrun/joyrun.db
```

or `~/.local/share/joyrun/joyrun.db` when `XDG_DATA_HOME` is unset.
`JOYRUN_DB` can override it.

The first public database format is independently versioned as
`release_channel=stable`, `schema_version=1`, and `schema_label=stable-1`.
JoyRun rejects the pre-release `development/dev-3` database instead of
migrating it. Start with a new database for v0.1.0; remote task metadata can
recover tasks that must be retained. Future stable schema changes require an
explicit migration.

On Windows the defaults are:

```text
%APPDATA%\joyrun\config.yaml
%LOCALAPPDATA%\joyrun\joyrun.db
```

Windows pulls reject reserved filenames, unsupported characters, and
case-insensitive collisions such as `A.out` and `a.out` rather than silently
overwriting data.

Every remote task contains a `metadata.json` recovery snapshot. If the local
database is lost, discover matching remote metadata for the current Project ID
and Target, then recover a selected task:

```bash
joyrun recover --scan -t gibbs/orca
joyrun recover jr_TASK_ID -t gibbs/orca
```

Recovery scanning does not require a usable local task database and never
imports tasks automatically.

See [the v0.1 design notes](docs/design.md) for the state and recovery model.
Maintainers should complete the
[real HPC acceptance checklist](docs/acceptance.md) before publishing a
release.

## License

Copyright 2026 Wanting Xia.

JoyRun is licensed under the
[Apache License 2.0](LICENSE).
