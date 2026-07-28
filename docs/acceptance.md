# Real HPC Acceptance Checklist

Run this checklist before publishing a JoyRun release. Unit tests do not
replace validation against a real OpenSSH server and Slurm installation.

## Prepare a smoke target

Create a file target whose script requests the cluster's smallest practical
allocation and performs only:

```bash
sleep 10
cp {{ .Input }} result.txt
```

Set `pull.default` to `["result.txt", "joyrun-slurm-*.log"]`. Use a dedicated
test project and run `joyrun doctor TARGET` before the scenarios below.

## Required scenarios

- Preview reports the correct entry, rendered script, included/ignored files,
  push mode, file count, and total bytes.
- An `entry` target uploads its selected input and declared dependencies but
  not sibling input/output files.
- Project-root submission fails without `--allow-project-root`; upload file
  count and total-size limits fail locally before SSH.
- Submit returns immediately with a `jr_...` ID; status reaches queued,
  running, and completed.
- `files` reports remote sizes and marks the submitted input.
- `pull --dry-run` changes no pull state; normal pull retrieves `result.txt`.
- A deliberately failing command records failed, the raw Slurm state, exit
  code, reason, and readable scheduler log.
- Timeout and cancellation remain distinct terminal outcomes.
- Interrupt upload and pull, retry the failed stage, and confirm that no
  partial local file is exposed.
- Disconnect SSH immediately after `sbatch`; status recovers the Slurm ID from
  the remote marker or `joyrun:<task-id>` comment.
- Submit the same source twice and confirm isolated remote directories.
- Move the local project and confirm source lookup still works.
- Delete or redirect the SQLite database, run `recover --scan`, recover one
  candidate, and refresh its status.

## Platform matrix

Run the full checklist on Linux with `transfer: rsync`. Run preview, submit,
status, files, pull, and recovery on native Windows with `transfer: sftp`.
Record JoyRun, OpenSSH, rsync, Slurm, and operating-system versions with the
release notes.
