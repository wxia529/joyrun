# Changelog

All notable user-visible changes are documented here. JoyRun follows semantic
version tags, while the SQLite schema is versioned independently.

## [v0.2.3] - 2026-08-03

### Changed

- Persisted submit dry-runs as local audit Tasks marked `dry_run`, without
  opening SSH or creating Slurm jobs.
- Excluded dry-run audit Tasks from `joyrun watch` by default, with
  `--include-dry-run` available for explicit history review.
- Simplified human-readable `watch` output by showing Project ID only for
  mixed-Project results; JSON output remains unchanged.

## [v0.2.2] - 2026-08-02

### Fixed

- Fixed daemon-reserved submissions for nested single-file Sources. The
  detached snapshot now uploads the selected input at the remote `work/` root,
  matching `.Input` and `.Stem` in the rendered script.
- Added a regression test for nested single-file daemon snapshots.

### Database compatibility

This release does not change the SQLite schema. Existing `stable-2` databases
continue to work without migration.

## [v0.2.1] - 2026-08-02

### Changed

- Made `joyrun watch` a one-shot, cache-only squeue-style query. It no longer
  starts a resident client loop or accepts an interval flag.
- Limited the default watch view to active Tasks and failures from the last
  12 hours; newer Tasks supersede older failures for the same Source. Use
  `--attention` or an explicit `--state` filter for historical inspection.
- Clarified the watch contract in the CLI help, README, command guide, and
  Agent Skill, including the JSON usage for Agents.

### Database compatibility

This release does not change the SQLite schema. Existing `stable-2` databases
continue to work without migration. Databases still marked `stable-1` must be
upgraded explicitly with `joyrun database upgrade --to stable-2` before use.

## [v0.2.0] - 2026-08-02

### Changed

- Made submission admission idempotent using a content-based submission key;
  retries after lost SSH responses reuse the existing Task instead of creating a
  second Slurm job. Added explicit `--force-new` for intentional reruns.
- Added the mandatory local daemon and authenticated IPC controller. Project,
  task, scheduler, and remote commands no longer fall back to direct SSH or
  SQLite access when the daemon is unavailable.
- Added durable submit/pull Operations, explicit stable-1 to stable-2 database
  upgrade, lease reclaim on restart, operation inspection/cancel/retry,
  conservative Task polling, and opt-in automatic pull.
- Added the cache-only global `joyrun watch` view with bounded attention-first
  task listing, filters, one-shot JSON output, and no remote polling from the
  client.
- Added adaptive scheduler freshness reporting, immutable detached input and
  configuration snapshots, resumable rsync/SFTP transfer staging, atomic pull
  installation, and OpenSSH ControlPath reuse in daemon mode.
- Fixed duplicate-admission fingerprints so changing pull or push selection
  cannot create a second compute Task, and protected detached source paths
  containing `=` during snapshot rewriting.
- Added a remote safety refresh before `--force-new`; active or uncertain
  identical Tasks now block intentional reruns until Slurm confirms a terminal
  state.

## [v0.1.11] - 2026-07-30

### Changed

- Added a hard Project ID safety boundary for operations on tasks, requiring
  repeated explicit confirmation before any out-of-project mutation.

## [v0.1.10] - 2026-07-30

### Changed

- Refined Agent resource guidance to separate reasoned node counts from
  generous use of available per-node memory.

## [v0.1.9] - 2026-07-30

### Changed

- Updated the Agent Skill guidance for autonomous CPU and memory selection.

## [v0.1.8] - 2026-07-30

### Removed

- Removed the project-wide `pull --finished` selector. Pull now requires exact
  Task/Source selectors or the ID of one known submission batch.

## [v0.1.7] - 2026-07-30

### Changed

- Clarified across CLI help, user documentation, and the Agent Skill that
  listing multiple Source paths directly is the primary batch-submission
  interface; `--glob` and `--from` are optional convenience selectors.

## [v0.1.6] - 2026-07-30

### Changed

- Batched `status --all` into one Slurm query per cluster and stopped probing
  legacy records without scheduler IDs during bulk refresh.
- Collapsed application/scheduler log fallback, recovery metadata scans, and
  rsync-based doctor checks into single remote invocations.
- Reused cached terminal compute state during pull and removed routine remote
  metadata writes from status and pull bookkeeping.
- Reduced a successful submission to three remote operations by relying on the
  immutable recovery metadata plus the atomically written scheduler marker.

### Added

- Extended `submit` to accept one or many Sources with cross-platform glob
  expansion, manifest files, one batch upload, and one remote Slurm session.
- Extended `pull` to accept one or many Tasks with per-cluster transfer,
  selecting the single-task or batch path from the resolved Task count.
- Added batch pull with project-local
  staging, per-Task results, destination-conflict protection, and selection
  by the `jb_...` ID returned from multi-source `submit`.

## [v0.1.5] - 2026-07-29

### Fixed

- Bounded stalled OpenSSH commands and idle rsync transfers with explicit
  connection and keepalive safeguards.
- Persisted submission failures after cancellation and distinguished
  uncertain Slurm acceptance from confirmed submission failure.
- Recorded precise upload stages and reduced normal submit SSH round trips by
  uploading the rendered script with the immutable input snapshot.

## [v0.1.4] - 2026-07-28

### Changed

- Clarified that failed and cancelled tasks may be pulled normally, while
  their outputs can be incomplete.
- Required a fresh official installer for version checks and upgrades.
- Added safeguards for replacing same-named local outputs when pulling
  historical tasks.

## [v0.1.3] - 2026-07-28

### Changed

- Streamlined the README around JoyRun's execution-layer boundary and primary
  task workflow.
- Added focused command, troubleshooting, release, and smoke-configuration
  documentation.
- Published version-matched Agent Skill and configuration-guide assets with
  each release.
- Strengthened release validation for archive contents and Skill version
  substitution.

## [v0.1.2] - 2026-07-28

### Added

- Cluster partition facts, opaque software identity, and Target placement
  policy.
- Explicit `--partition` selection for submission and node queries.
- Agent resource-consistency review guidance.
- HPC-style binary capacity suffixes such as `180G`.

### Changed

- The resolved partition is passed directly to `sbatch --partition`.
- Release assets now include the version-matched JoyRun skill and Agent
  configuration guide.

### Configuration compatibility

This active-development release intentionally replaces `status.partition` and
Target parameter-based partition selection. Update configurations to:

```yaml
clusters:
  cluster:
    partitions:
      compute:
        cores_per_node: 64
        memory_per_node: 240G
targets:
  cluster/software:
    software: {name: software}
    placement:
      default_partition: compute
      allowed_partitions: [compute]
```

Use `.Partition.Name` in script templates and `--partition NAME` on the CLI.
The SQLite schema remains `stable/stable-1`.

## [v0.1.1] - 2026-07-28

- Added explicit per-submission dependencies and Target-scoped Slurm node
  observations.
- Strengthened safe source selection for file-oriented software.

## [v0.1.0] - 2026-07-28

- First public release with asynchronous Slurm submission, status, logs,
  bounded pull, global SQLite state, Project identity, and remote recovery.

[v0.2.2]: https://github.com/wxia529/joyrun/compare/v0.2.1...v0.2.2
[v0.2.3]: https://github.com/wxia529/joyrun/compare/v0.2.2...v0.2.3
[v0.2.1]: https://github.com/wxia529/joyrun/compare/v0.2.0...v0.2.1
[v0.2.0]: https://github.com/wxia529/joyrun/compare/v0.1.11...v0.2.0
[v0.1.5]: https://github.com/wxia529/joyrun/compare/v0.1.4...v0.1.5
[v0.1.4]: https://github.com/wxia529/joyrun/compare/v0.1.3...v0.1.4
[v0.1.3]: https://github.com/wxia529/joyrun/compare/v0.1.2...v0.1.3
[v0.1.2]: https://github.com/wxia529/joyrun/compare/v0.1.1...v0.1.2
[v0.1.1]: https://github.com/wxia529/joyrun/compare/v0.1.0...v0.1.1
[v0.1.0]: https://github.com/wxia529/joyrun/releases/tag/v0.1.0
