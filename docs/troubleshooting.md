# Troubleshooting

Start with JSON output when sharing diagnostics:

```bash
joyrun COMMAND --json
```

Report the error code, task ID, `compute_state`, `pull_state`, and suggested
action. Do not blindly repeat `submit`: Slurm may already have accepted it.

## Configuration and source selection

- `CONFIG_INVALID`: run `joyrun config validate`. Check required `software`,
  `placement`, source, push, and script fields. Old `status.partition` and a
  target parameter named `partition` are unsupported; use `placement`.
- `SOURCE_KIND_MISMATCH`: submit a concrete file to a file Target or a
  directory to a directory Target. Do not bypass it by broadening upload scope.
- `SOURCE_PATTERN_MISMATCH`: select an entry matching the Target's declared
  patterns or correct the Target contract.
- `PROJECT_ROOT_UPLOAD_FORBIDDEN`: choose a narrower work directory.
  `--allow-project-root` is only for an intentional whole-project snapshot.
- `UPLOAD_POLICY_EXCEEDED`: inspect the dry-run manifest, excludes, explicit
  dependencies, file count, and total size.

## Remote execution

- `SSH_FAILED`: verify `ssh HOST` using the same OpenSSH alias. JoyRun does not
  store credentials or bypass host-key verification.
- `SSH_TIMEOUT`, `UPLOAD_TIMEOUT`, or `PULL_TIMEOUT`: JoyRun bounded an
  unresponsive SSH or transfer operation. Inspect
  `joyrun inspect TASK --events`; retry the failed stage instead of creating
  another Task.
- `doctor` `WARN` for `remote_root`: the root is absent but a writable ancestor
  can create it. `FAIL` means no usable writable path was established.
- `SUBMIT_FAILED`: inspect the task before retrying. JoyRun attempts recovery
  through the remote scheduler marker and immutable Slurm comment.
- `SUBMISSION_UNCERTAIN`: Slurm may already have accepted the task. Run
  `joyrun status TASK` until the scheduler ID is recovered or investigate the
  immutable `joyrun:TASK` Slurm comment. Do not run `submit` again.
- `LOG_NOT_READY`: no configured application or scheduler log exists yet.
  Retry later or inspect the candidate paths reported by the error.

JoyRun applies OpenSSH connection and keepalive safeguards and a 90-second
rsync inactivity timeout. This is an inactivity limit, not a total transfer
duration, so large active transfers are not capped.

## Pull and recovery

- `NO_FILES_MATCHED`: preview remote files with `joyrun files TASK`; then
  adjust `--include` or deliberately use `--all`.
- `PULL_FAILED`: computation state is unchanged. Retry only `pull`; do not
  recompute.
- `LOCAL_CONFLICT`: inspect protected input paths. Use `--overwrite-inputs`
  only when replacement is explicitly intended.
- Lost local database: initialize or enter the original project, run
  `recover --scan -t TARGET`, then import one selected Task ID.
