# JoyRun v0.1 design

JoyRun models one execution as:

```text
Project + Source + Target + resolved Params -> immutable Task
```

Each target has a source contract (`file`, `directory`, or `either`). File
targets can constrain entry names with glob patterns. Targets that use
`.Input` or `.Stem` require a file entry, so directory submissions fail before
rendering and before task persistence.

Each target also declares `push.mode`. `entry` is valid only for a file target
and snapshots the selected entry plus target-level `push.include` dependencies
and per-submission `--include` dependencies. Target-level includes are for
files required by every run; optional coordinates and restart files should be
selected explicitly for one submission. Every requested `--include` pattern
must match an uploaded file.
`workdir` snapshots the whole source working directory. Exclusions always win,
including built-in `.joyrun/` and `.git/` rules. Optional file-count and
total-size limits reject unexpectedly broad snapshots before SSH is used.

Clusters may record verified per-partition hardware facts. Targets separately
declare opaque software identity and placement policy: one default partition
and an allowlist. Submission can override the default only with
`--partition`; JoyRun passes the result to `sbatch --partition`, and templates
may consume the resolved `.Partition` object. `target nodes` queries that same
partition. JoyRun does not parse scientific inputs,
infer resource needs, choose a partition from live load, or validate resource
compatibility. It exposes facts so the user or calling agent can perform that
reasoning before submission.

The project root is not a valid upload working directory by default, including
when a file source lives directly beneath it. `--allow-project-root` is an
explicit escape hatch for intentional whole-project scope.

## Persistence and recovery

The SQLite database under the platform data directory (XDG data on Unix,
LocalAppData on Windows) is the primary index and state store. A project is
addressed by the ID in `.joyrun/project.yaml`, not by its absolute path. Each
command rebinds that ID to the current path, so a project can be moved without
losing source-path lookup.

The first public database format is independently versioned as
`release_channel=stable`, `schema_version=1`, and `schema_label=stable-1`.
Pre-release `development/dev-3` databases are rejected rather than migrated;
remote metadata remains available for deliberate recovery. Future stable
schema changes require explicit, tested migrations.

Every remote task directory also contains `metadata.json`. It is updated after
submission and status changes. If the local database is lost, a known task can
be imported with:

```bash
joyrun recover jr_TASK_ID -t cluster/target
```

The target identifies the cluster from which metadata must be read.
`joyrun recover --scan -t cluster/target` discovers compatible metadata for
the current Project ID without opening the local database; it never imports
candidates automatically.

## Compute state, pull progress, and events

JoyRun deliberately separates remote computation from the latest pull
operation:

```text
compute_state: created -> queued -> running -> completed|failed|cancelled
pull_state:    not_pulled -> pulling -> pulled|partial|failed
```

`submission_failed` means the submission pipeline failed before JoyRun could
confirm a scheduler job. `unknown` means Slurm did not provide a usable state;
neither state authorizes an automatic resubmission.

`pull_state` makes no statement about scientific importance, completeness, or
remote retention. It records only whether the latest requested set of files
was pulled.

State changes and operational milestones are appended to `task_events`.
`joyrun inspect TASK --events` exposes this audit trail. The current task row
is the efficient materialized view; events are not replayed to derive normal
command results.

`joyrun status --all` refreshes non-terminal tasks. It recovers a scheduler ID
from the remote marker when available, queries Slurm, and records an event only
when observable state changes. It never retries or resubmits a task.

Status snapshots also retain Slurm's raw state, elapsed duration, reason, and
exit code. These are scheduler diagnostics, not scientific interpretation.

## Remote layout

Each submission gets an isolated directory:

```text
REMOTE_ROOT/
└── jr_TASK_ID/
    ├── metadata.json
    ├── scheduler_id
    └── work/
        ├── joyrun-job.sh
        └── submitted files
```

The `scheduler_id` marker is written atomically by the same remote shell that
runs `sbatch`. Every submitted job also carries the immutable Slurm comment
`joyrun:<task-id>`. If SSH disconnects before the marker or stdout arrives,
JoyRun reconciles that comment through `squeue` and `sacct`.

Task rows use an optimistic revision. A stale command cannot overwrite state
recorded by a concurrent JoyRun process; it receives `DATABASE_CONFLICT` and
must reload the task.

JoyRun passes `--output=joyrun-slurm-%j.log` to `sbatch`, reserving a scheduler
diagnostic log independently of target-owned application logs. Log lookup uses
configured application candidates first, then the reserved scheduler log, and
finally the legacy `slurm-%j.out` name for older tasks.

## Trust boundary

Target scripts are trusted user configuration. Built-in values and string
parameters are POSIX-shell quoted before they are rendered into a job script;
line-breaking values are rejected. Use typed parameters and `choices` for
values that should be constrained. JoyRun never stores SSH passwords or
private keys.

## Transfer backends

JoyRun keeps transfer policy behind one interface:

- rsync remains the preferred Unix backend and provides incremental,
  partial-file transfers.
- SFTP is carried through the system OpenSSH client with
  `ssh <host> -s sftp`. This retains OpenSSH config, agent, known-host and
  ProxyJump behavior without implementing authentication inside JoyRun.
- `auto` selects SFTP on Windows. On Linux and macOS it starts with rsync when
  available locally, without an extra SSH probe, and retries with SFTP if
  rsync fails. `doctor` performs the explicit remote backend check.

OpenSSH operations use bounded connection setup and server keepalives. rsync
uses a 90-second inactivity timeout rather than a total-duration limit.

SFTP writes uploads and downloads to same-directory temporary files before
renaming them. Windows-specific pull validation rejects paths that NTFS cannot
represent safely.

## v0.1 boundaries

- OpenSSH command-line client
- rsync and OpenSSH/SFTP transfer backends
- Slurm only
- no daemon, workflow DAG, software auto-detection, or automatic restart
- no scientific interpretation, arbitrary remote shell API, or automatic
  resubmission
- no tar/zstd packing; a future transfer strategy can add it without changing
  the task model
