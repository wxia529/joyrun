# JoyRun

JoyRun is a local-first remote task runner for HPC:

```text
local project -> OpenSSH -> offline HPC -> Slurm -> rsync/SFTP results back
```

Give JoyRun a bounded local source and a user-defined Target. It creates an
isolated remote task, submits it without blocking, tracks Slurm state, and
pulls selected results back beside the original input.

```bash
joyrun submit task01/eg.inp -t gibbs/orca
joyrun status task01/eg.inp
joyrun pull task01/eg.inp
```

## Scope

JoyRun is an execution, transport, and provenance layer. It:

- snapshots explicit inputs into one isolated directory per submission;
- transfers files, submits Slurm jobs, and queries scheduler state;
- records task state, resolved configuration, and lifecycle events;
- safely pulls user-selected results back to the source directory.

JoyRun deliberately does not interpret scientific results, modify inputs,
choose resources, restart calculations, build workflow DAGs, or expose a
general-purpose remote shell. The user or calling Agent remains responsible
for scientific input, resource consistency, and decisions to resubmit.

Targets are complete user-owned job-script templates. JoyRun does not contain
ORCA, Gaussian, VASP, or other application parsers.

## Install

Official installers select the matching release archive, verify
`SHA256SUMS`, and install without elevated privileges.

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

The defaults are `~/.local/bin` and
`%LOCALAPPDATA%\Programs\JoyRun`. Re-running the installer performs a verified
upgrade and preserves the previous binary. See
[Installation and Upgrades](docs/install.md) for update checks, exact-version
pins, rollback behavior, and platform details.

Install from source with Go 1.24 or later:

```bash
go install github.com/wxia529/joyrun/cmd/joyrun@latest
```

Or build a checkout without `sudo`:

```bash
make build
make install PREFIX="$HOME/.local"
```

JoyRun uses the system OpenSSH client and never stores credentials. Linux and
macOS prefer rsync when available on both ends and fall back to SFTP. Native
Windows uses OpenSSH SFTP without requiring WSL, Cygwin, MSYS2, or rsync.

## Install with an AI Agent

Copy this prompt to a coding Agent:

```text
Install https://github.com/wxia529/joyrun/releases/latest/download/SKILL.md as a global user-level JoyRun skill so it is available in all projects, then follow it to install the latest stable JoyRun release for this machine.
```

The Skill and configuration guide are published with each release so their
commands match the stable binary. For a pinned binary, install the matching:

```text
https://github.com/wxia529/joyrun/releases/download/vX.Y.Z/SKILL.md
```

## First run

JoyRun needs one user configuration and one Project identity.

```bash
joyrun config init
joyrun config validate
cd my-project
joyrun init
```

`config init` creates a commented starter without overwriting an existing
file. `init` creates `.joyrun/project.yaml`; the global SQLite index uses its
Project ID plus source-relative paths, so moving the project does not break
task lookup.

List and inspect configured Targets:

```bash
joyrun target list
joyrun target show gibbs/orca
joyrun target params gibbs/orca
joyrun doctor gibbs/orca
```

Always preview a new source/Target combination:

```bash
joyrun submit task01/eg.inp -t gibbs/orca --dry-run
```

Preview performs no SSH operation and creates no database task. Check the
source contract, software identity, partition facts, parameters, exact upload
manifest, remote directory, and rendered script before submitting.

## Configuration model

A Cluster records connectivity and verified partition facts:

```yaml
clusters:
  gibbs:
    host: gibbs
    scheduler: slurm
    remote_root: /scratch/your-user/joyrun
    transfer: auto
    partitions:
      community:
        cores_per_node: 64
        memory_per_node: 240G
```

`host` is an OpenSSH alias. Keep usernames, ports, keys, `ProxyJump`, and
authentication in OpenSSH configuration. Omit unknown hardware facts instead
of estimating them.

A Target identifies software, constrains placement, defines its input
boundary, and owns the full job script:

```yaml
targets:
  gibbs/orca:
    cluster: gibbs
    software: {name: orca, version: "6.1.1"}
    placement:
      default_partition: community
      allowed_partitions: [community]
    source:
      kind: file
      patterns: ["*.inp"]
    params:
      cpus: {type: int, default: 32}
    push:
      mode: entry
      limits: {max_files: 20, max_total_size: 2GiB}
      exclude: ["*.out", "*.tmp"]
    script: |
      #!/bin/bash
      #SBATCH --cpus-per-task={{ .Params.cpus }}
      #SBATCH --partition={{ .Partition.Name }}
      #SBATCH --job-name={{ .Stem }}
      orca {{ .Input }} > {{ .Stem }}.out
    pull:
      default: ["*.out", "*.xyz", "*.gbw"]
    logs: ["{{ .Stem }}.out"]
```

`source.kind` is `file`, `directory`, or `either`. `push.mode: entry` uploads
the selected file plus declared or explicit `--include` dependencies;
`workdir` uploads the bounded working directory after exclusions. The project
root is rejected unless `--allow-project-root` is explicitly supplied.

The partition is selected through `placement` and optional `--partition`.
JoyRun passes it directly to `sbatch --partition`. It exposes partition facts
but does not decide whether application-level cores or memory are appropriate.

See the [minimal smoke configuration](examples/smoke-config.yaml), the
[complete application example](examples/config.yaml), and
[Agent Configuration Guide](docs/agent-configuration.md) for template
variables, typed parameters, upload limits, output policies, and validation.

To ask an Agent to adapt an existing Slurm script:

```text
Follow https://github.com/wxia529/joyrun/releases/latest/download/SKILL.md to create a JoyRun target from my existing Slurm script at <SCRIPT_PATH> for inputs under <SOURCE_PATH>. Do not guess cluster-specific values, submit a real job, or overwrite unrelated configuration. Validate the configuration, run doctor, and finish with a dry-run.
```

## Task workflow

Submit asynchronously and record the returned `jr_...` Task ID:

```bash
joyrun submit task01/eg.inp -t gibbs/orca --json
joyrun status jr_TASK_ID --json
joyrun logs jr_TASK_ID --lines 200 --json
```

The primary way to submit multiple jobs is to list every Source path directly.
The paths do not need to share a directory, filename, or naming pattern:

```bash
joyrun submit \
  benzene/opt.inp \
  water/frequency.inp \
  methane/single-point.inp \
  -t gibbs/orca \
  --dry-run
```

Use `--glob` only when the desired Sources follow a reliable pattern, or
`--from` when a reviewed text file contains one Source path per line:

```bash
joyrun submit --glob "task*/*.inp" -t gibbs/orca --json
joyrun submit --from sources.txt -t gibbs/orca --json
```

All three forms create the same kind of batch. JoyRun validates every Source
locally, uploads once, and opens one remote Slurm submission session while
preserving one independent Task and Slurm job per Source. Every Source in the
command shares the Target, partition, parameter overrides, and dependency
includes. Partial failures are reported per Task. One batch accepts at most
100 distinct Sources.

A source path resolves to its newest task. Use an exact Task ID for
cancellation, recovery, and other mutations.

Inspect remote files and preview a pull:

```bash
joyrun files jr_TASK_ID
joyrun pull jr_TASK_ID --dry-run
joyrun pull jr_TASK_ID
```

Pull several independent tasks with one transfer per cluster:

```bash
joyrun pull jr_TASK1 jr_TASK2 --dry-run
joyrun pull jr_TASK1 jr_TASK2 --json
joyrun pull --batch jb_BATCH_ID --json
joyrun pull --glob "task*/*.inp" --json
joyrun pull --finished --json
```

`--batch` selects the independent Tasks created by one multi-source `submit`.
`--finished` selects only the newest terminal, not-yet-pulled Task for each
Source. Explicit IDs can resynchronize older or already-pulled results. These
selection modes are mutually exclusive. One batch accepts at most 100 Tasks.
Paths in a `--from` file are resolved from the current working directory.

Successful non-preview `submit` and all `pull` results return arrays named
`tasks` and `failures` in JSON, even when only one Task is selected.
Multi-source submission additionally returns `batch_id`; submit preview
returns a `previews` array. A total command failure uses the normal top-level
`error` response.

Default pull patterns are frozen at submission. Submitted inputs remain
protected even with `--all`; replacing them requires
`--overwrite-inputs`. A transfer failure does not imply computation failure:
retry `pull`, not the calculation.

JoyRun tracks computation and pull progress independently:

```text
compute_state: created -> submission_failed|submission_uncertain
                       -> queued -> running -> completed|failed|cancelled
pull_state:    not_pulled -> pulling -> pulled|partial|failed
```

`submission_uncertain` means the `sbatch` connection ended before JoyRun could
prove whether Slurm accepted the job. Run `status` on that exact Task ID;
never repeat `submit` until reconciliation is complete.

`status --all` batches active jobs into one Slurm query per cluster. Records
without a scheduler ID remain local; reconcile one explicitly with
`status jr_TASK_ID`. Status never resubmits.
`inspect --events` returns the immutable submission snapshot and append-only
lifecycle events. If the local index is lost, remote `metadata.json` can be
discovered with `recover --scan` and imported one task at a time.

## Agent and JSON contract

All operational commands are non-interactive. Under `--json`, stdout contains
exactly one document; progress and diagnostics use stderr.

```json
{"ok":true,"result":{"id":"jr_...","compute_state":"running","pull_state":"not_pulled"}}
```

Errors include a stable code, retryability, and recovery context when
available. Agents must not blindly repeat `submit`, because Slurm may already
have accepted the job.

The [JoyRun Skill](SKILL.md) defines safe Agent operation, including bounded
uploads, resource review, monitoring, pull selection, cancellation, and
recovery.

## Documentation

- [Command Guide](docs/commands.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Installation and Upgrades](docs/install.md)
- [Agent Configuration Guide](docs/agent-configuration.md)
- [Design and state model](docs/design.md)
- [Real HPC Acceptance Checklist](docs/acceptance.md)
- [Changelog](CHANGELOG.md)

The first public SQLite schema is independently versioned as
`stable/stable-1`. See the Changelog before upgrading across configuration or
database compatibility boundaries.

## License

Copyright 2026 Wanting Xia.

JoyRun is licensed under the [Apache License 2.0](LICENSE).
