# JoyRun v0.1 design

JoyRun models one execution as:

```text
Project + Source + Target + resolved Params -> immutable Task
```

## Persistence and recovery

The SQLite database under the platform data directory (XDG data on Unix,
LocalAppData on Windows) is the primary index and state store. A project is
addressed by the ID in `.joyrun/project.yaml`, not by its absolute path. Each
command rebinds that ID to the current path, so a project can be moved without
losing source-path lookup.

Every remote task directory also contains `metadata.json`. It is updated after
submission and status changes. If the local database is lost, a known task can
be imported with:

```bash
joyrun recover jr_TASK_ID -t cluster/target
```

The target identifies the cluster from which metadata must be read.

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
runs `sbatch`. It lets the client recover the Slurm job ID if SSH disconnects
after submission but before stdout reaches the local process.

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
- `auto` selects SFTP on Windows. On Linux and macOS it selects rsync only when
  the executable is available locally and remotely, otherwise it selects SFTP.

SFTP writes uploads and downloads to same-directory temporary files before
renaming them. Windows-specific pull validation rejects paths that NTFS cannot
represent safely.

## v0.1 boundaries

- OpenSSH command-line client
- rsync and OpenSSH/SFTP transfer backends
- Slurm only
- no daemon, workflow DAG, software auto-detection, or automatic restart
- no tar/zstd packing; a future transfer strategy can add it without changing
  the task model
