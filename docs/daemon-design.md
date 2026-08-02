# JoyRun Local Daemon Development Design

Status: implementation contract; mandatory daemon baseline implemented
Scope: local background controller; no process is installed on an HPC cluster

This document describes the current development baseline. All project, task,
scheduler, and remote commands route through the authenticated local daemon;
configuration inspection, version, daemon control, and explicit database
maintenance are the only local exceptions.

Implementation note: the current tree includes stable-2 Operation persistence,
explicit database upgrade, immutable detached admission, restart-safe leases,
adaptive polling, opt-in automatic pull, OpenSSH multiplexing, atomic local
pull installation, and resumable rsync/SFTP transfer files. Transfer-item
accounting is represented in the schema but is not yet populated per byte.

The current release implements the daemon lifecycle, authenticated local IPC,
single-instance ownership, mandatory CLI routing, durable Operations, detached
snapshots, polling, and background pulls. The remaining
limits are deliberate: no daemon-side scientific interpretation, no automatic
resubmission, and no per-byte transfer ledger yet.

## 1. Purpose

Before the daemon baseline, every command ran in a short-lived process. The
current architecture removes that runtime path: clients submit authenticated
IPC requests, and only the daemon opens SQLite or starts OpenSSH, rsync, or
SFTP. Batch commands still reduce remote round trips, while the daemon adds a
durable operation queue, continuous scheduler reconciliation, connection reuse,
and work after the CLI disconnects.

The local daemon adds reliability and coordination without changing JoyRun's
scientific boundary. It is a per-user controller that owns mutable local state,
coordinates cluster access, and resumes safe operations after interruption.

The target design is intended to provide:

- one SQLite writer while it is running;
- durable submit, reconcile, pull, and cancel operations;
- adaptive background Slurm polling with explicit freshness metadata;
- cluster-level concurrency limits and optional OpenSSH multiplexing;
- continued work after the invoking CLI exits;
- safe restart after a daemon or computer crash;
- opt-in result retrieval and transfer bookkeeping;
- identical human and JSON command contracts across IPC requests.

## 2. Non-goals

The daemon does not:

- run on an HPC login or compute node;
- store SSH credentials or replace OpenSSH authentication;
- interpret scientific output, convergence, or correctness;
- modify scientific input files;
- choose resources, partitions, or restart parameters;
- automatically resubmit computation;
- create workflow DAGs;
- expose an arbitrary remote shell;
- delete remote task data automatically;
- monitor or mutate Tasks outside the current user's JoyRun Project boundary.

## 3. Safety and consistency invariants

The implementation must preserve these invariants:

1. A Task is an immutable execution snapshot. Background activity may update
   observed state but never its inputs, rendered script, Target snapshot, or
   resolved parameters.
2. A durable operation is recorded before its first external side effect.
3. Once scheduler acceptance becomes uncertain, no code path may issue another
   `sbatch` for that Task until reconciliation proves the attempt was rejected.
4. Local cached state may optimize reads but never authorize `--force-new`,
   retry, cancel, overwrite, or deletion without a remote refresh.
5. Transfer completion is recorded only after files are atomically installed
   in their final local paths.
6. A daemon crash may delay work, but must not create a duplicate Slurm job or
   expose a partial local result file.
7. Remote connectivity failure preserves the last confirmed state and records
   stale freshness; it never invents a terminal result.
8. Automatic background work is limited to Tasks and file policies explicitly
   admitted by the user.

Exactly-once Slurm submission cannot be proven across an arbitrary network
partition because Slurm does not provide a client idempotency API. JoyRun
therefore chooses at-most-once behavior in ambiguous cases: it may require
manual reconciliation rather than risk a second job.

## 4. Direct CLI execution model

The current path is:

```text
CLI process
  -> load config.yaml
  -> open global joyrun.db
  -> construct app.App
  -> spawn OpenSSH/rsync/SFTP subprocesses
  -> update Task and task_events
  -> close database and exit
```

Reusable components already exist:

- `internal/app`: operation orchestration and safety policy;
- `internal/store`: SQLite Task and event persistence;
- `internal/scheduler`: Slurm adapter and batch status queries;
- `internal/remote`: OpenSSH command execution;
- `internal/transfer`: rsync and OpenSSH/SFTP backends;
- `internal/manifest`: immutable input snapshot and file hashing;
- `internal/config`: cluster, Target, partition, and parameter validation.

The daemon must call the same application services. CLI mode and daemon mode
must not develop separate submission or pull implementations.

## 5. Target architecture

```text
joyrun CLI / Agent
        |
        | local versioned IPC
        v
JoyRun daemon (one per OS user)
  |-- API server and request registry
  |-- durable operation dispatcher
  |-- Slurm poller and submission reconciler
  |-- durable operation worker and grouped status poller
  |-- SSH connection/multiplex manager
  |-- transfer queue and file installer
  |-- config manager
  |-- SQLite store (single writer)
        |
        | OpenSSH / rsync / SFTP
        v
Offline HPC login nodes and Slurm
```

There is one daemon for all Projects owned by the local user. Project ID
remains the authorization and lookup boundary. There is no daemon per Project
and no daemon on the cluster.

## 6. Process lifecycle

### 6.1 Public commands

```bash
joyrun daemon start
joyrun daemon run --foreground
joyrun daemon status
joyrun daemon stop
joyrun daemon logs [--lines N]
joyrun database upgrade --to stable-2 [--dry-run]
joyrun operation list
joyrun operation show jo_OPERATION_ID
joyrun operation cancel jo_OPERATION_ID
joyrun operation retry jo_OPERATION_ID
```

`run --foreground` is the service-manager entry point and is useful for tests.
`start` launches the current executable detached, waits for a successful IPC
handshake, and then exits. `stop` requests graceful shutdown and waits up to 15
seconds; it does not cancel Slurm jobs or durable operations.

Starting an already compatible daemon is an idempotent success. An incompatible
daemon returns `DAEMON_VERSION_MISMATCH`. A stale socket is removed only after
the process acquires the exclusive lock and proves no daemon owns it. If `stop`
times out it returns `DAEMON_STOP_TIMEOUT`; the CLI never kills the process or
removes its lock behind its back.

The first daemon release uses explicit startup. Automatic startup and native
service installation are deferred until lifecycle behavior is proven on Linux,
macOS, and Windows.

Submit and pull are admitted as durable `jo_...` Operations. The worker
executes the application services after the client exits, records
stdout/stderr, snapshots the configuration and selected input bytes, and
reclaims expired leases after restart. Batch selectors are expanded during
admission before the immutable operation payload is stored.

### 6.2 Runtime paths

Unix paths (the implementation uses `XDG_STATE_HOME`, not `XDG_RUNTIME_DIR`):

```text
$XDG_STATE_HOME/joyrun/run/daemon.sock
$XDG_STATE_HOME/joyrun/run/daemon.lock
$XDG_STATE_HOME/joyrun/run/daemon.secret
$XDG_STATE_HOME/joyrun/daemon.log
```

When `XDG_STATE_HOME` is absent, Unix falls back to
`~/.local/share/joyrun/...`. Runtime directories and files are owner-only:
directory `0700`, secret `0600`, socket `0600`.

Windows uses:

```text
\\.\pipe\joyrun-daemon
%LOCALAPPDATA%\joyrun\run\daemon.lock
%LOCALAPPDATA%\joyrun\run\daemon.secret
%LOCALAPPDATA%\joyrun\daemon.log
```

The named pipe uses the operating system's default named-pipe security and the
random owner-only session secret provides the second authentication layer.

### 6.3 Single-instance ownership

The daemon holds an OS-level exclusive lock for its lifetime. The PID file is
diagnostic only; process existence is not used as the lock primitive.

Stateful CLI commands follow this routing rule:

```text
IPC handshake succeeds       -> use daemon
IPC unavailable              -> DAEMON_REQUIRED; never open SQLite or SSH
```

Configuration inspection and database maintenance are the only local
exceptions. This prevents a half-started daemon and a CLI from acting
concurrently.

### 6.4 Shutdown and restart

On graceful shutdown the daemon:

1. stops accepting new mutation requests;
2. finishes or checkpoints current database transactions;
3. requests cancellation of local remote-command and transfer contexts;
4. cancels local request contexts; interrupted Operations are requeued by the
   worker, while scheduler ambiguity is reconciled by the normal submission
   safety path;
5. closes SSH control connections;
6. closes SQLite and removes runtime socket/secret files;
7. releases the single-instance lock.

It never calls `scancel` during shutdown. On startup, expired operation leases
are reclaimed before workers begin polling.

## 7. IPC protocol

### 7.1 Transport and framing

Unix uses a Unix domain stream socket; Windows uses a byte-mode named pipe.
Messages are length-prefixed JSON frames:

```text
4-byte unsigned big-endian length | UTF-8 JSON payload
```

The maximum frame is 8 MiB. Requests exceeding the limit fail with
`IPC_MESSAGE_TOO_LARGE`. Logs remain line-limited and file transfers never pass
through IPC.

### 7.2 Handshake

Every connection begins with:

```json
{
  "type": "hello",
  "protocol": 1,
  "client_version": "v0.x.y",
  "secret": "base64-session-secret"
}
```

The daemon answers with its version, protocol range, instance ID, PID, database
schema label, and startup time. A mismatched protocol or binary contract returns
`DAEMON_VERSION_MISMATCH` and instructs the user to restart the daemon after an
upgrade.

The random 256-bit session secret is generated at daemon startup and stored in
the owner-only runtime file. It supplements socket or pipe permissions; it is
not an SSH credential.

### 7.3 Request model

```json
{
  "type": "request",
  "request_id": "rq_...",
  "method": "command.execute",
  "cwd": "/absolute/project/path",
  "args": ["status", "task01/eg.inp", "--json"]
}
```

Responses use the existing JoyRun envelope:

```json
{"type":"response","request_id":"rq_...","ok":true,"exit_code":0,"stdout":"..."}
```

or:

```json
{"type":"response","request_id":"rq_...","ok":false,"exit_code":1,"error":"..."}
```

The current protocol returns one final response per request. It has no progress
subscription or intermediate progress frames yet; long-running work must use
durable Operations and `operation show/list`. Disconnecting a client does not
cancel an already admitted Operation.

The daemon executes the same raw CLI arguments as the client. Synchronous
commands return their final stdout/stderr; normal `submit` and `pull` admit a
durable Operation and return after local admission is complete. The result can
be inspected later with `operation show/list` or waited on explicitly with
`operation wait`.

### 7.4 Cancellation semantics

Ctrl+C cancels the current request context. A detached operation already
admitted to SQLite continues. Explicit operation cancellation is:

```bash
joyrun operation cancel jo_OPERATION_ID
```

It cancels only the local operation. It never cancels a Slurm job; that remains
`joyrun cancel jr_TASK_ID`.

Read-only refresh requests may be cancelled with the client connection because
they do not represent durable intent.

Current cancellation is intentionally narrower: a running submit is rejected
as unsafe (`OPERATION_CANCEL_UNSAFE`); queued or running pull work can be
marked cancelled and its context is stopped. `operation cancel` changes the
local Operation only; it never calls `scancel`. Scheduler cancellation remains
the separate `joyrun cancel TASK` command. Partially installed files are not
rolled back, and compute state is unchanged.

### 7.5 Method contract and backpressure

The client sends raw CLI arguments plus its working directory; the daemon runs
the normal command parser and application service, so direct and daemon modes
share validation and output contracts.

Initial protocol methods are:

| Method | Durable | Result |
|---|---:|---|
| `project.init` | no | Project identity |
| `task.submit` | yes | Task list, batch ID, Operation ID, dedup flags |
| `task.status` | no | Task states and freshness |
| `task.list` / `task.inspect` | no | cached Task records/events |
| `task.logs` / `task.files` | no | bounded remote text or file inventory |
| `task.pull` | yes | pull plan/results and Operation ID |
| `task.cancel` | yes | cancellation results and Operation ID |
| `target.nodes` / `doctor.run` | no | existing structured results |
| `task.recover` | yes | imported Task results |
| `operation.list/show/wait/cancel/retry` | mutation-dependent | Operation records |

The server permits at most 64 simultaneous local clients and 8 MiB frames.
Handlers are independently served; durable work is bounded by the dispatcher,
which admits at most eight operations globally and one active operation per
cluster key. There is no progress-frame buffer yet.

## 8. Command routing

Commands that remain local and do not require the daemon:

```text
version
help
config path/init/validate
target list/show/params
submit --dry-run
```

Commands routed through a running daemon:

```text
init
submit
status / status --all
list / inspect / inspect --events
logs / files
pull
cancel
target nodes
doctor
recover
operation list/show/cancel/retry
```

Local-only reads may continue to read SQLite directly only when the daemon lock
is free. While the daemon runs, all database access goes through IPC so one
process owns schema compatibility and revision handling.

## 9. Durable operation model

The implemented worker executes durable submit/pull command envelopes, persists
terminal stdout/stderr, renews and reclaims leases, refreshes active Projects
with conservative adaptive intervals, and enqueues opt-in automatic pulls.
Transfer files resume through rsync `--partial` or SFTP partial files; the
schema's transfer-item ledger is reserved for a later progress UI.

### 9.1 Identity

Durable operation IDs use `jo_...`. Task IDs remain `jr_...`; submission batch
IDs remain `jb_...`; pull correlation IDs remain `jp_...`. An Operation is not
a Task selector.

### 9.2 Operation kinds

```text
submit
reconcile_submission
pull
cancel_scheduler
recover_task
```

`logs`, `files`, `doctor`, and node queries are request-scoped reads and are not
persisted unless a later use case demonstrates a recovery requirement.

### 9.3 Operation states

```text
queued
running
waiting_reconcile
succeeded
partially_succeeded
failed
cancelled
```

`waiting_reconcile` is used for scheduler acceptance ambiguity. Retry timing is
stored in operation fields, but there are no separate `waiting_remote`,
`waiting_local`, or `retry_scheduled` states yet. Only `succeeded`,
`partially_succeeded`, `failed`, and `cancelled` are terminal.
Batch submit or pull uses `partially_succeeded` when at least one member reached
its intended result and at least one member failed; member results remain
individually retryable.

Terminal Operation state does not replace Task `compute_state` or `pull_state`.
For example, a status refresh operation may fail while the Task remains
`running`, and a pull operation may fail while compute remains `completed`.

### 9.4 Leases

A dispatcher claims an Operation with an owner instance ID and lease expiry.
Workers renew leases every 10 seconds. Default lease duration is 30 seconds.
After a crash, startup may reclaim an expired lease. A reclaimed submit at or
after `submit_started` enters reconciliation rather than executing `sbatch`.

### 9.5 Retry policy

Operation attempts, `next_attempt_at`, and lease data are persisted, and
`joyrun operation retry jo_ID` explicitly requeues the same Operation after
revalidation. Automatic timed retry/backoff and jitter are not implemented yet;
failed operations remain terminal until an explicit retry or a new command.
Scheduler submission is never automatically repeated after `submit_started`.

## 10. Database design

The daemon requires one explicit schema upgrade from `stable-1` to `stable-2`.
It must not silently migrate the database. The upgrade command supports a dry
run, creates a timestamped backup, and applies the operation-table migration in
one transaction. The backup is retained until the user removes it.

```bash
joyrun database upgrade --to stable-2 --dry-run
joyrun database upgrade --to stable-2
```

The daemon refuses to start against `stable-1` with `DATABASE_UPGRADE_REQUIRED`.

The current migration checkpoints the database, makes a timestamped file backup,
validates the `stable/stable-1` marker, creates the operation/event, transfer,
and cluster-runtime tables and indexes, updates the schema label, and commits
transactionally. Dry-run validates the marker and reports the planned backup
path without writing. It does not yet rewrite historical Task provenance,
load config, run integrity checks, or populate transfer/runtime rows.

### 10.1 Task provenance

The current `tasks` table keeps scheduler freshness and provenance in its
existing columns plus the JSON `metadata` field. Submission keys, rendered
script/config snapshots, auto-pull flags, and recovered-state markers are not
separate Task columns or unique SQL indexes. The store still indexes
`(project_id, source_path, created_at DESC)`; duplicate admission is checked in
application code while the SQLite write transaction is held.

### 10.2 Operations table

```sql
CREATE TABLE operations (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  project_id TEXT NOT NULL,
  cluster_key TEXT NOT NULL DEFAULT '',
  dedup_key TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  stage TEXT NOT NULL,
  payload TEXT NOT NULL,
  result TEXT NOT NULL,
  attempt INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL DEFAULT 0,
  retry_deadline_at TEXT,
  next_attempt_at TEXT,
  lease_owner TEXT NOT NULL DEFAULT '',
  lease_expires_at TEXT,
  error_code TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  retryable INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  started_at TEXT,
  updated_at TEXT NOT NULL,
  finished_at TEXT,
  FOREIGN KEY(project_id) REFERENCES projects(id)
);

CREATE TABLE operation_tasks (
  operation_id TEXT NOT NULL,
  task_id TEXT NOT NULL,
  ordinal INTEGER NOT NULL,
  state TEXT NOT NULL DEFAULT 'pending',
  result TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY(operation_id, task_id),
  UNIQUE(operation_id, ordinal),
  FOREIGN KEY(operation_id) REFERENCES operations(id) ON DELETE CASCADE,
  FOREIGN KEY(task_id) REFERENCES tasks(id)
);

CREATE TABLE operation_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  operation_id TEXT NOT NULL,
  state TEXT NOT NULL,
  stage TEXT NOT NULL,
  message TEXT NOT NULL DEFAULT '',
  data TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  FOREIGN KEY(operation_id) REFERENCES operations(id) ON DELETE CASCADE
);
```

Required indexes:

```text
(state, next_attempt_at)
(project_id, created_at DESC)
(operation_tasks.task_id, operation_id)
(cluster_key, state)
(operation_events.operation_id, id)
unique (kind, dedup_key) where dedup_key != ''
```

Payload and result are JSON command envelopes. They currently contain the
detached CLI arguments, Project/config paths, and snapshot references; explicit
per-kind schema versioning is future work. Secrets and raw environment
variables are forbidden in both fields.

`max_attempts`, `retry_deadline_at`, and `next_attempt_at` are persisted fields
reserved for future automatic retry scheduling; the current worker does not
interpret them as a timed retry policy.

`operations.payload` freezes all flags that affect behavior, including exact
Task order, overwrite policy, pull selection, and Operation retry policy.
It never stores an unresolved glob because later filesystem changes must not
change an admitted Operation.

### 10.3 Transfer items

Each pull Operation freezes its intended file set before transfer:

```sql
CREATE TABLE transfer_items (
  operation_id TEXT NOT NULL,
  task_id TEXT NOT NULL,
  ordinal INTEGER NOT NULL,
  remote_path TEXT NOT NULL,
  local_path TEXT NOT NULL,
  expected_size INTEGER NOT NULL,
  expected_sha256 TEXT NOT NULL DEFAULT '',
  transferred_size INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL,
  error_code TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY(operation_id, task_id, remote_path),
  FOREIGN KEY(operation_id) REFERENCES operations(id) ON DELETE CASCADE,
  FOREIGN KEY(task_id) REFERENCES tasks(id)
);
```

Allowed states are `planned`, `transferring`, `staged`, `installed`, and
`failed`. The required index is `(operation_id, ordinal)`.

This schema is reserved for a future per-file progress/audit UI. The current
transfer manager resumes using rsync/SFTP partial files but does not populate
one row per transferred file.

### 10.4 Cluster runtime table

Persist only operational backoff, not scheduler truth:

```sql
CREATE TABLE cluster_runtime (
  cluster_key TEXT PRIMARY KEY,
  config_hash TEXT NOT NULL,
  cluster_name TEXT NOT NULL,
  last_contact_at TEXT,
  last_success_at TEXT,
  last_error_code TEXT NOT NULL DEFAULT '',
  next_poll_at TEXT,
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL
);
```

The table is reserved for future persisted polling backoff. Current polling
does not populate it or persist cluster runtime state.

## 11. Submission identity and duplicate protection

### 11.1 Submission key

The current implementation stores one `submission_key` in Task metadata. It is
the SHA-256 fingerprint of scheduler execution intent:

```text
Project ID
normalized Source and Source entry
immutable input manifest paths/sizes/hashes
Target name and execution hash
secret-free cluster snapshot and partition
resolved parameters
rendered script hash and scheduler arguments
```

It includes the Project/source identity, immutable input manifest, Target and
cluster identity, selected partition, resolved parameters, and rendered
execution script. Pull/log/push policy changes are excluded from the key, so a
result-selection edit cannot create a second compute job. The full Target hash
is still retained for audit.

An exact retry keeps the existing key. A normal repeated submit with the same
key is deduplicated to the existing Task. `--force-new` deliberately bypasses
that deduplication after the remote safety guard succeeds and creates a new
Task ID and remote directory.

### 11.2 Atomic admission

Task admission checks the matching `submission_key` while holding the SQLite
write lock, so two processes cannot both create the same exact Task. Detached
Operation admission is persisted separately before the worker creates the Task.

### 11.3 `--force-new` guard

Before admitting `--force-new`, the daemon remotely refreshes every matching
prior Task with the same submission key. A cached nonterminal or uncertain
state never authorizes a new run; refresh failure blocks with
`SUBMISSION_SAFETY_UNCONFIRMED`.

Allowed prior states:

```text
submission_failed
completed
failed
cancelled
```

Blocking prior states:

```text
created
submission_uncertain
queued
running
unknown
```

Remote refresh failure also blocks with `SUBMISSION_SAFETY_UNCONFIRMED`. A
cached nonterminal or unverified terminal state never authorizes a new run.

### 11.4 Retry versus new run

`joyrun retry jr_TASK_ID` retries a failed stage of the same Task. It requires
an exact Task ID and never changes the input snapshot.

- `submission_failed` before `submit_started`: resume metadata/upload/script.
- explicit `sbatch` rejection: issue a new scheduler attempt for the same Task.
- `submission_uncertain`: run reconciliation only.
- queued/running/terminal compute state: reject retry.

`joyrun submit ... --force-new` creates a distinct Task and is used only for an
intentional new computation, such as regenerating a deleted wavefunction.

## 12. Submission state machine

```text
operation queued
  -> task_reserved
  -> snapshot_ready
  -> remote_metadata_written
  -> snapshot_uploaded
  -> submit_started
  -> scheduler_accepted -> succeeded
                         -> waiting_reconcile
                         -> submission_failed (explicit rejection only)
```

Stage recovery rules:

| Last durable stage | Restart action |
|---|---|
| `task_reserved` | recreate immutable local snapshot and verify manifest |
| `snapshot_ready` | write remote metadata |
| `remote_metadata_written` | resume upload |
| `snapshot_uploaded` | verify script marker, then submit |
| `submit_started` | reconcile marker/comment; never call `sbatch` directly |
| `scheduler_accepted` | refresh Slurm status only |

The immutable snapshot may be reconstructed only if every current source hash
still matches the recorded input manifest. Otherwise fail with
`SOURCE_SNAPSHOT_UNAVAILABLE`; never upload changed bytes under the same Task.

### 12.1 Durable local snapshot

Before a detached submit is acknowledged, selected Source files are copied and
symlinks dereferenced into:

```text
<data-dir>/operations/jo_ID/project/
<data-dir>/operations/jo_ID/config.yaml
```

The snapshot directory is owner-only. JoyRun copies into a provisional
directory, hashes the copied bytes, verifies that each Source file did not
change during the copy, and retries the complete snapshot at most twice. A
third change fails with `SOURCE_CHANGED_DURING_SNAPSHOT`. Only then does one
transaction commit the Task, manifest, Operation, and final staging path; a
transaction failure removes the provisional directory.

The snapshot is retained until remote upload and metadata hashes are confirmed,
then removed. If staging is unexpectedly lost, reconstruction from the Source
is allowed only under the exact-hash rule above. Admission checks free local
space before copying and returns `LOCAL_SPACE_INSUFFICIENT` without any remote
change. Detached admission persists the Operation after snapshot creation; the
worker later creates the Task through the normal submit service rather than
committing Task and Operation in one transaction.

### 12.2 Remote submission marker protocol

Each Task uploads immutable `metadata.json`, its rendered script, and a small
submission wrapper before scheduler submission. The wrapper:

1. writes an atomic `submit_started` marker containing Task ID, submission key,
   and attempt number;
2. calls `sbatch --parsable` with Slurm comment `joyrun:<task-id>`;
3. on exit zero, parses the scheduler adapter's Slurm job-ID token (including
   the optional cluster suffix emitted by `--parsable`) and atomically renames
   `scheduler_id.tmp` to `scheduler_id`;
4. on normal nonzero exit, emits the explicit `JOYRUN_SUBMIT_REJECTED` marker
   (or the batch `REJ` record) and bounded stderr so the client can classify a
   scheduler rejection;
5. never treats SSH exit 255, timeout, or an unclassified shell exit as Slurm
   rejection.

The wrapper and comment close different uncertainty windows: the marker is the
fast path, while Slurm queue/accounting lookup recovers acceptance if the SSH
response or marker write was lost. A pre-submit Slurm query alone is not a
duplicate-prevention mechanism because it races with `sbatch`.

Before `submit_started`, a remote setup failure is safely `submission_failed`.
After it, only a valid `scheduler_id`, Slurm comment match, or an explicit
rejection record may resolve acceptance. Unclassified stderr text alone is
never proof.

For a batched submit, marker and stage updates remain per Task. If the remote
session ends after accepting only part of the batch, accepted members reconcile,
members without `submit_started` resume safely, and started members remain
uncertain. The batch wrapper never reruns its whole loop blindly.

## 13. Scheduler polling and reconciliation

### 13.1 Poll grouping

The poller groups active Tasks by cluster and issues one `Statuses` request per
cluster, chunked at 500 scheduler IDs. Queue rows override accounting rows.
The daemon poll worker refreshes each Project conservatively; concurrent
request coalescing is not implemented yet.

Explicit multi-Task commands remain the primary batching API. One
`joyrun submit SOURCE...` creates one submit Operation with isolated Tasks and
uses the batch path where the selected backend supports it. One
`joyrun pull TASK...` similarly plans and transfers compatible Tasks as a
group. Detached Operations currently retain their own durable result and are
processed by the single local dispatcher; cross-Operation coalescing is a
future optimization, not a correctness requirement.

`list` and `inspect` are cache-only views and never start SSH. `status` follows
the freshness policy below. `status --all` groups selected nonterminal Tasks by
cluster key and therefore uses at most one scheduler session per cluster key
per refresh cycle, subject to 500-ID chunks inside that session.

### 13.2 Adaptive intervals

Current polling is conservative and deterministic: the worker wakes every five
seconds locally and refreshes each Project only when its state-dependent
interval expires. The current implementation does not yet add random jitter
or persisted cluster backoff. The target intervals are:

| Task state | Interval |
|---|---:|
| `submission_uncertain`, first 2 minutes | 10 seconds |
| `submission_uncertain`, 2–10 minutes | 30 seconds |
| `submission_uncertain`, later | 2 minutes |
| `queued` | 30 seconds |
| `running` | 30 seconds |
| `unknown` | 30 seconds |
| terminal | no periodic polling |

Persisted per-cluster backoff is reserved for a later worker refinement; a
failed refresh currently leaves the last confirmed state marked stale and is
retried at the same state-dependent interval.

### 13.3 Freshness response

Status JSON adds:

```json
{
  "freshness": {
    "source": "remote",
    "observed_at": "2026-08-02T12:00:00Z",
    "age_seconds": 3,
    "stale": false
  }
}
```

Normal `status` is cache-first and performs no SSH; `status --cached` is the
explicit local-only spelling, while `status --refresh` requests remote
confirmation. The daemon poll worker updates the cache using the state-
dependent intervals above.

### 13.4 Missing scheduler ID

Reconciliation performs, in one cluster session when possible:

1. read atomic `scheduler_id` markers for all selected Tasks;
2. query active Slurm jobs by `joyrun:<task-id>` comment;
3. query accounting by the same comment and bounded creation time;
4. persist recovered IDs and observations atomically.

A single no-match result does not prove rejection. The Task remains
`submission_uncertain`; no automatic resubmission occurs.

Terminal scheduler state is monotonic: a later missing accounting row never
downgrades a remotely confirmed terminal Task. A nonterminal Task absent from
both queue and accounting becomes `unknown` with a stale timestamp. `doctor`
checks whether the cluster accepts `--comment` and exposes comments through
queue/accounting commands. If comment lookup is unavailable, JoyRun reports the
weaker recovery capability and never substitutes ambiguous job-name matching.

## 14. Worker and concurrency boundaries

The current daemon uses a durable-operation dispatcher with up to eight active
workers and one active operation per cluster key. Batch commands still group
compatible Tasks before remote execution. The effective baseline limits are:

```text
status/reconcile queries: 1 grouped refresh in flight
submit sessions:          1 in flight, up to 100 Tasks per batch
uploads:                  1 in flight
downloads:                1 in flight
interactive reads:        up to 2 in flight
```

The table remains a conservative target for finer operation classes; the
current implementation enforces the global/cluster operation boundary and
reuses OpenSSH multiplexing for remote sessions.

Priority order:

```text
cancel > reconcile uncertain > submit > explicit status > pull > background poll
```

Long waits are not required for ordinary submit/pull IPC calls: they create a
durable Operation and return. `operation wait` is an explicit blocking call;
other IPC requests remain available while it waits.

## 15. SSH connection management

The daemon continues to rely on system OpenSSH and `~/.ssh/config`. It does not
parse private keys or reimplement ProxyJump, agent, known-host, or certificate
handling.

### 15.1 Multiplexing capability

On platforms where OpenSSH control sockets are supported, daemon commands share
an owner-only OpenSSH control path equivalent to:

```text
ssh -o ControlMaster=auto -o ControlPersist=300 -o ControlPath=<owner-only-control-path> HOST
```

All SSH and rsync invocations receive the same `ControlPath` when daemon mode
provides one. OpenSSH creates and retires the master connection on demand;
JoyRun does not manage a separate SSH process or run `ssh -O check` yet.
Multiplexing is a detected optimization, not a correctness dependency. On an
unsupported OpenSSH build or platform, commands use ordinary OpenSSH.

### 15.2 Failure classification

Connection setup, authentication, remote command rejection, inactivity timeout,
and local cancellation remain distinct error codes. After `submit_started`, SSH
exit 255, timeout, and unclassified wrapper failure make scheduler acceptance
uncertain; only the wrapper's rejection marker proves explicit rejection.

### 15.3 Keepalive and limits

Retain `BatchMode=yes`, bounded connection setup, server keepalive, and rsync's
90-second inactivity timeout. The daemon must not impose a total duration limit
on active large transfers.

## 16. Transfer queue and data organization

### 16.1 Pull planning

Before a transfer the daemon freezes:

- exact Task IDs;
- remote and local roots;
- selected relative files;
- expected sizes and optional checksums;
- input-protection decision;
- overwrite policy;
- collision-free destination map.

Changing local project location after planning pauses the Operation and
re-resolves Project ID before installation.

Immediately before installing each file, JoyRun repeats input protection and
local-conflict checks against the current destination. Planning permission is
not treated as permission to overwrite a file changed while transfer ran.

### 16.2 Staging and installation

Downloads go to owner-only staging directories beneath the destination
filesystem so final rename remains atomic. A file is installed only after size
and, when available, checksum verification. Windows path validation occurs
before transfer.

Rsync retains partial files for resume. SFTP uses `.joyrun-part` remote files
for upload and same-directory local temporary files for download. On daemon
restart, staging referenced by durable Operations remains available for retry.
Automatic removal of unreferenced staging is not implemented yet.

The first daemon release does not automatically delete Tasks, terminal
Operations, Operation events, input manifests, remote Task directories, or
daemon logs. A future prune/rotation command must be explicit and separately
designed.

### 16.3 Automatic pull

Automatic pull is opt-in per Task:

```bash
joyrun submit SOURCE -t TARGET --auto-pull=completed
joyrun submit SOURCE -t TARGET --auto-pull=terminal
```

Policies:

```text
off        default; never background-pull
completed  pull Target default patterns after successful computation
terminal   pull Target default patterns after completed/failed/cancelled
```

Automatic pull never implies `--all`, `--live`, or `--overwrite-inputs`. It
never deletes remote data. Failed automatic pull remains a retryable pull
Operation and does not alter compute state.

If the Project has moved, the daemon follows its current Project ID binding. If
the Project is offline or its identity cannot be proven, the pull Operation
fails without recreating the old absolute directory.

The terminal-state transition and automatic-pull admission use the Task metadata
flag `auto_pull_enqueued` to avoid duplicate enqueueing during normal polling.
There is no SQL unique constraint for this yet, so crash recovery remains
best-effort. No matched remote files and a confirmed disappeared file are
terminal pull errors; local conflict is non-retryable until the user issues a
new explicit pull policy.

### 16.4 Data inventory boundary

The daemon records only files observed during `files` or a pull plan. It may
record path, size, checksum, local destination, and last observation. It does
not classify wavefunctions, checkpoints, convergence, or scientific value.

## 17. Configuration management

Each request loads and validates its selected configuration. Detached Operations
copy that file into their owner-only operation directory before acknowledgement,
so a later edit cannot change an admitted script or Target. Invalid replacement
configuration therefore affects only new direct requests; existing Tasks and
detached Operations retain their snapshots. A long-lived in-memory config cache
is deliberately not used yet.

Existing Tasks retain frozen Target parameters, rendered script, pull patterns,
and Target hash, together with their cluster name and remote directory. The
current implementation still resolves the cluster host and remote root from
the selected configuration when a later status/pull command runs; a detached
Operation avoids this drift by copying its config snapshot.

Multiple `--config` files are supported by independent request contexts. Every
Task records its Target/script snapshot; the daemon does not merge
configurations.

Workers, status grouping, and SSH multiplexing use the selected cluster
configuration for each request. There is no in-memory reload watcher or
runtime-config cache yet; new requests load the selected config and detached
Operations use their copied config snapshot. Scientific Target configuration
remains unchanged.

## 18. Security and Project boundary

- IPC is authenticated by owner-only OS permissions and a runtime secret.
- Request `cwd` and config paths are canonicalized; NUL, traversal, and unsafe
  symlink transitions are rejected.
- Every Task mutation validates current Project ID, not merely current path.
- Exact Task ID remains mandatory for cancel, retry, recovery import, overwrite,
  and other destructive or externally visible mutations.
- The daemon never scans another Project automatically.
- Out-of-project mutation retains the Skill's repeated explicit-confirmation
  requirement and is not made easier by background operation.
- Remote paths always remain descendants of the configured Task remote root.
- Logs redact session secrets and never include SSH environment or private key
  contents.

## 19. Observability

### 19.1 Daemon status

```bash
joyrun daemon status --json
```

reports:

```text
version and protocol
PID and uptime
database schema
daemon instance ID and start time
```

It never reports credentials or arbitrary environment variables.

### 19.2 Logs

`daemon logs` reads the configured daemon log file. Structured rotation,
per-operation log fields, and progress streaming are future observability work;
JSON stdout remains exactly one final document.

### 19.3 Events

Task lifecycle events remain in `task_events`. Operation state transitions stay
in Operation records and optional `operation_events`; they must not flood Task
history with polling timer ticks. A Task event is added only for meaningful
compute, pull, submission, cancellation, or recovery changes.

### 19.4 Resource and connection targets

With no active or uncertain Tasks and no queued Operations, the daemon performs
no SSH traffic. It may wake locally for lease cleanup and config checks. Memory
queues are bounded; due Operations are paged from SQLite rather than all loaded
at startup.

Expected logical remote sessions, excluding one optional persistent master
connection:

| User action | Remote session target per cluster |
|---|---:|
| submit 1 Task | 1 upload + 1 submit |
| submit 10 Tasks in one command | 1 upload + 1 submit |
| `status --all` | 1 scheduler session per refresh cycle |
| pull 1 or 10 compatible Tasks | 1 listing + 1 transfer |
| cached `list` / `inspect` | 0 |

OpenSSH multiplexing removes repeated authentication and TCP setup but does not
change these logical session counts. Session-count metrics are not exposed yet;
the table is an operational target for acceptance tests.

## 20. Failure scenarios

| Failure | Required behavior |
|---|---|
| CLI exits during upload | daemon continues; Task/Operation remain inspectable |
| daemon crashes during upload | resume partial transfer after lease recovery |
| daemon crashes before `submit_started` | resume last safe stage |
| daemon crashes after `submit_started` | reconcile marker/comment; no `sbatch` |
| SSH response lost after Slurm acceptance | recover same Task and scheduler ID |
| explicit Slurm rejection | mark `submission_failed`; allow exact Task retry |
| local network offline | preserve state, mark stale, and retry on the next polling interval |
| config becomes invalid | block the affected request; detached Operations use their saved snapshot |
| project moves | rebind Project ID and recompute destinations |
| local result changes during pull | install atomically or return local conflict |
| remote file disappears | fail only the pull Operation; preserve compute state |
| database busy/corrupt | stop mutations; never fall back to a second writer |
| daemon/client version mismatch | require daemon restart; no mutation |
| computer shuts down | recover expired leases on next startup |

Daemon-specific stable error codes are:

```text
DAEMON_UNAVAILABLE
DAEMON_VERSION_MISMATCH
DAEMON_STOP_TIMEOUT
IPC_AUTH_FAILED
IPC_MESSAGE_TOO_LARGE
DATABASE_UPGRADE_REQUIRED
OPERATION_NOT_FOUND
OPERATION_CANCEL_UNSAFE
OPERATION_NOT_RETRYABLE
SUBMISSION_SAFETY_UNCONFIRMED
SOURCE_CHANGED_DURING_SNAPSHOT
SOURCE_SNAPSHOT_UNAVAILABLE
LOCAL_SPACE_INSUFFICIENT
```

They use the existing structured JoyRun error envelope and retryable flag.
Daemon transport errors never replace a more specific persisted Operation
error. Existing CLI process exit-code behavior remains unchanged.

## 21. Package layout

```text
internal/
  daemon/
    server.go          lifecycle and supervision
    client.go          CLI transport
    protocol.go        frames and version negotiation
    lock.go            platform single-instance lock
    paths.go           runtime paths
  app/
    app.go             submit/status/pull policy and orchestration
    batch.go           multi-source submit/pull batching
  cli/
    cli.go             command routing and output
    daemon.go          daemon lifecycle commands
    operations.go      detached admission and worker loop
  store/
    migration.go       explicit stable-1 -> stable-2 migration
    operations.go      Operation persistence
  transfer/
    manager.go         atomic pull planning/installation
    rsync.go           resumable rsync backend
    sftp.go            resumable SFTP backend
```

`internal/app` remains the policy/service layer. It should depend on Store,
Remote, Scheduler, Transfer, and Clock interfaces rather than constructing
processes. The daemon injects implementations of those interfaces; clients
never construct remote adapters.

All routed application services run inside the daemon. Durable Operations can
be resumed after a daemon restart; a client process never becomes a second
controller.

## 22. Implementation phases

### Phase A: consistency foundation (implemented)

- finish submission-key persistence;
- implement remote-refresh guard for `--force-new`;
- add exact `retry TASK` semantics;
- add tests for uncertain, rejected, terminal, and concurrent admission.

### Phase B: daemon shell (implemented)

- runtime paths, lock, secret, IPC framing, handshake;
- `daemon start/run/status/stop/logs`;
- mandatory daemon CLI routing and `DAEMON_REQUIRED` handling;
- protocol mismatch and upgrade behavior.

### Phase C: durable operations (implemented foundation)

- explicit `stable-2` migration and backup workflow;
- Operation store and transfer retry state;
- dispatcher, leases, graceful shutdown, crash recovery;
- detached submit/pull execution and persisted stdout/stderr.

### Phase D: monitoring (implemented conservatively)

- single local poll worker, grouped status queries, and uncertain reconciliation;
- freshness fields, `--cached`, and `--refresh`;
- persisted cluster backoff and poll jitter remain future refinements.

### Phase E: connection and transfer optimization (implemented foundation)

- detected OpenSSH multiplexing;
- rsync ControlPath propagation;
- per-cluster transfer limits remain a future refinement;
- restartable staging; automatic staging cleanup remains future work.

### Phase F: optional automation (implemented foundation; real-HPC validation pending)

- `--auto-pull` policies;
- operation inspection/cancellation;
- final documentation, Skill, and installers; real-HPC acceptance is still a
  manual release gate.

## 23. Testing and acceptance

Automated tests use fake Store/Remote/Scheduler/Transfer objects where practical.
Current coverage includes:

- IPC framing, authentication, version mismatch, disconnect, and max size;
- Unix lock and Windows named-pipe/single-instance behavior;
- atomic Task plus Operation admission;
- two concurrent identical submissions yielding one Task and one `sbatch`;
- detached snapshot, lease expiry, and worker reclaim paths;
- `--force-new` rejection for stale, active, uncertain, or unreachable state;
- successful intentional rerun after remotely confirmed terminal state;
- explicit rejection retry and uncertain reconciliation;
- grouped status and freshness calculations;
- rsync/SFTP partial transfer recovery and atomic local installation;
- Project move during queued pull;
- detached config snapshots and relative config resolution;
- daemon/client upgrade mismatch without database mutation.

Progress streaming, persisted backoff/jitter, per-file transfer rows, and full
crash injection at every remote stage remain acceptance work rather than claims
about the current automated suite.

Real-HPC acceptance must additionally verify:

1. start the daemon and submit ten Tasks while the CLI disconnects mid-command;
2. observe no more than one submit and one status session per cluster at once;
3. kill the daemon immediately before and after `sbatch` and recover without a
   duplicate Slurm job;
4. disconnect the network during upload, status, and pull, then restore it;
5. restart the computer with queued and running Operations;
6. validate OpenSSH multiplexing where supported and fallback where not;
7. repeat the suite on Linux/rsync and native Windows/SFTP;
8. confirm no automatic pull, overwrite, remote deletion, or resubmission occurs
   without the corresponding explicit policy.

## 24. Frozen design decisions

The first daemon implementation adopts these decisions:

- local per-user daemon, never an HPC daemon;
- same `joyrun` binary, no separate installed `joyrund` executable;
- explicit `joyrun daemon start` startup (service integration is a later
  packaging concern, not a runtime fallback);
- daemon mandatory for routed commands and the sole SQLite writer;
- length-prefixed authenticated local JSON IPC;
- persistent Operations in the same SQLite database;
- explicit `stable-2` migration, never silent migration;
- remote state required for mutation safety decisions;
- at-most-once behavior for ambiguous scheduler submission;
- OpenSSH multiplexing as an optional optimization;
- automatic pull off by default and remote deletion unsupported;
- no scientific interpretation, restart logic, or workflow orchestration.
