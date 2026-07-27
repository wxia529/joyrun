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

Public template values are:

- `{{ .Input }}` — entry filename, empty for a directory source
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

It shows resolved parameters, files, the planned remote directory, and the
rendered script.

## Commands

```bash
joyrun init [directory]
joyrun targets
joyrun target show gibbs/orca
joyrun target params gibbs/orca
joyrun doctor gibbs/orca

joyrun submit task01/eg.inp -t gibbs/orca
joyrun status task01/eg.inp
joyrun logs task01/eg.inp --lines 200
joyrun pull task01/eg.inp
joyrun cancel task01/eg.inp
joyrun history task01/eg.inp
```

A source path addresses its newest task. A `jr_...` task ID addresses one exact
historical run:

```bash
joyrun status jr_TASK_ID
joyrun pull jr_TASK_ID
```

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
{"ok":true,"result":{"id":"jr_...","state":"running"}}
```

Failure:

```json
{
  "ok": false,
  "error": {
    "code": "SSH_FAILED",
    "message": "cannot connect to gibbs",
    "retryable": true
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
