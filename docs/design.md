# JoyRun v0.2 design

This document describes the core Task, Source, Target, persistence, transfer,
and scheduler contracts. The daemon is the single execution backend; its
mandatory contract is specified in the [local daemon development design](daemon-design.md).
The daemon remains local: it does not install a process on HPC systems or
expand JoyRun into scientific workflow logic.

The daemon architecture and implementation contract are specified in the
[local daemon development design](daemon-design.md). The development daemon
now performs local admission, durable dispatch, per-cluster serialization,
cache-first status, and explicit operation waiting; the remaining typed-service
extraction is an internal follow-up and does not change the CLI contract.

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

Submit previews are retained as local audit records with `dry_run` metadata.
They remain in task history and never open SSH or create a Slurm job. The
cache-only `watch` dashboard excludes them by default; `--include-dry-run`
explicitly includes preview history.

The current development database format is `release_channel=stable`,
`schema_version=1`, and `schema_label=stable-2`. Existing stable-1 databases
must be upgraded explicitly with `joyrun database upgrade --to stable-2`; JoyRun
never migrates them silently. Remote metadata remains available for deliberate
recovery. Future stable schema changes require explicit, tested migrations.

Every remote task directory also contains immutable submission
`metadata.json`; the atomically written `scheduler_id` marker completes its
recovery identity after Slurm acceptance. Routine status and pull state remain
in SQLite rather than causing additional SSH writes. If the local database is
lost, a known task can be imported with:

```bash
joyrun recover jr_TASK_ID -t cluster/target
```

The target identifies the cluster from which metadata must be read.
`joyrun recover --scan -t cluster/target` discovers compatible metadata for
the current Project ID without opening the local database; it never imports
candidates automatically.

## Batch operations

Multi-source `submit` is transport batching, not a new scheduler abstraction.
All sources use one Target and pass local validation before remote changes.
JoyRun uploads isolated `jr_...` directories once and submits their independent
scripts through one remote shell. Each accepted job writes its own scheduler
marker, so partial success remains recoverable.

Multi-Task `pull` groups Tasks by cluster, lists files in one command, downloads into
project-local staging once, then installs files into each Source directory.
Input protection still applies. Tasks mapping to the same local result path
fail preflight with `BATCH_LOCAL_CONFLICT`.

The `jb_...` batch ID is stored in Task metadata rather than as a second job
object. It selects the original group for `pull --batch`; every `jr_...`
Task remains independently inspectable and retryable. A separate ephemeral
`jp_...` pull ID correlates the events from one batch transfer and is never a
Task selector.

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

`joyrun status --all` refreshes active tasks with known scheduler IDs in one
Slurm query per configured cluster, then records an event only when observable
state changes. Records without a scheduler ID stay local during bulk refresh
so old failed submissions cannot cause an SSH operation per record. Use
`joyrun status TASK` for explicit scheduler-ID reconciliation. Neither command
retries or resubmits a task.

Status snapshots also retain Slurm's raw state, elapsed duration, reason, and
exit code. These are scheduler diagnostics, not scientific interpretation.
Remote recovery metadata preserves submission identity; routine status and
pull bookkeeping is authoritative in the local SQLite index and does not cause
an extra metadata SSH write.

## Idempotent submission admission

Before admission, JoyRun computes a SHA-256 submission key from the Project ID,
Source, immutable input manifest, Target name and cluster, selected partition,
resolved parameters, and rendered execution script. Pull patterns, logs, push
filters, and other result-selection policy are deliberately excluded. It
excludes the random Task ID and remote directory, so
retrying the same request addresses the same submission. SQLite serializes the
key check and task insert, preventing two local processes racing with the same
request from both reaching Slurm.

When a matching Task already exists, JoyRun returns it without opening a new
remote submission session. This includes `submission_uncertain`, the critical
lost-SSH-response case. `--force-new` is the explicit escape hatch for a
deliberate rerun. The key is also stored in remote `metadata.json`, allowing
`recover --scan` to preserve the same submission identity if the local index is
lost.

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

## v0.2 boundaries

- OpenSSH command-line client
- rsync and OpenSSH/SFTP transfer backends
- Slurm only
- local daemon, durable detached Operations, adaptive reconciliation, and
  resumable transfers
- no workflow DAG, software auto-detection, or automatic restart
- no scientific interpretation, arbitrary remote shell API, or automatic
  resubmission
- no tar/zstd packing; a future transfer strategy can add it without changing
  the task model
