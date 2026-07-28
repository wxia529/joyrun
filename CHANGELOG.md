# Changelog

All notable user-visible changes are documented here. JoyRun follows semantic
version tags, while the SQLite schema is versioned independently.

## [Unreleased]

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

[Unreleased]: https://github.com/wxia529/joyrun/compare/v0.1.4...HEAD
[v0.1.4]: https://github.com/wxia529/joyrun/compare/v0.1.3...v0.1.4
[v0.1.3]: https://github.com/wxia529/joyrun/compare/v0.1.2...v0.1.3
[v0.1.2]: https://github.com/wxia529/joyrun/compare/v0.1.1...v0.1.2
[v0.1.1]: https://github.com/wxia529/joyrun/compare/v0.1.0...v0.1.1
[v0.1.0]: https://github.com/wxia529/joyrun/releases/tag/v0.1.0
