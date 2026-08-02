# Agent Configuration Guide

Use this guide only when creating or modifying JoyRun clusters and targets.
Configuration work prepares execution; it does not authorize submitting a job.

## Workflow

1. Locate the active configuration:

   ```bash
   joyrun config path --json
   ```

2. If it does not exist, create the starter:

   ```bash
   joyrun config init --json
   ```

   `config init` refuses to overwrite an existing file. When a configuration
   exists, read and preserve its unrelated clusters and targets.

3. Inspect the user's existing Slurm script, input files, and any documented
   cluster instructions. Treat them as the source of truth.

4. Ask only for required facts that cannot be established from those sources:

   - OpenSSH host alias;
   - absolute remote work root;
   - account, partition, resource requests, and environment setup;
   - executable path or module commands;
   - files required as input dependencies;
   - default results and application logs to retrieve.

5. Add or edit the smallest applicable cluster and target entries.

6. Complete every validation step in [Validate](#validate). Do not submit a
   real task unless the user separately requests submission.

Never request or store SSH passwords, private keys, or tokens. JoyRun uses the
user's OpenSSH configuration. Do not guess cluster-specific values or copy
values from an unrelated cluster.

## Define a cluster

JoyRun v0.2 supports Slurm:

```yaml
version: 1

clusters:
  para-amd:
    host: para-amd
    scheduler: slurm
    remote_root: /scratch/username/joyrun
    transfer: auto
    partitions:
      normal:
        cores_per_node: 64
        memory_per_node: 256GiB
      highio:
        cores_per_node: 64
```

- `host` must be an OpenSSH host or alias that the user intends to use.
- `remote_root` must be an absolute POSIX path owned or writable by the user.
- `transfer` is `auto`, `rsync`, or `sftp`. Prefer `auto` unless the user has a
  confirmed reason to force a backend.
- `partitions` records verified hardware facts. `cores_per_node` and
  `memory_per_node` are optional; omit unknown facts instead of estimating.
  Partition names may contain letters, digits, `.`, `_`, and `-`.
  Capacity accepts decimal `GB`, explicit binary `GiB`, and HPC-style binary
  suffixes such as `180G`.
- Reuse an existing cluster entry instead of duplicating connection details.

Do not put usernames, ports, keys, `ProxyJump`, or authentication material in
JoyRun configuration; those belong in OpenSSH configuration.

## Identify software and constrain placement

Every Target must identify its software and declare where it may run:

```yaml
software:
  name: orca
  version: "6.1.1"
placement:
  default_partition: normal
  allowed_partitions: [normal, highio]
```

`software` is opaque descriptive data for users and agents; it does not enable
JoyRun parsing or validation. Every placement name must exist under the
Target's cluster. Keep application parameters such as executable variant,
nodes, or memory request under `params`, but keep the Slurm partition in
`placement`. Use `--partition APPROVED_NAME` to override the default. JoyRun
passes the resolved value directly to `sbatch --partition`; using
`.Partition.Name` in the script remains useful for transparent dry-run output
but is not the enforcement mechanism. The parameter name `partition` is
reserved and rejected to prevent two conflicting sources of truth.

## Choose the source and upload boundary

Choose from the actual program workflow, not only the filename extension:

| Workflow | `source.kind` | `push.mode` | Submission |
|---|---|---|---|
| One entry file plus known dependencies | `file` | `entry` | Submit the concrete file |
| A calculation consumes a complete working directory | `directory` | `workdir` | Submit the directory |
| Both forms are truly supported | `either` | `workdir` | Submit either form |

Use `source.patterns` to constrain file entries, for example `["*.inp"]`.
Directory sources cannot declare patterns. Any target using `{{ .Input }}` or
`{{ .Stem }}` must use `source.kind: file`.

For `push.mode: entry`, JoyRun uploads the selected entry plus explicit
dependencies. Declare `push.include` only for sibling files required by every
run:

```yaml
source:
  kind: file
  patterns: ["*.inp"]
push:
  mode: entry
  include: ["shared.basis"]
  exclude: ["*.out", "*.tmp"]
  limits:
    max_files: 30
    max_total_size: 2GiB
```

Do not make optional coordinates, wavefunctions, checkpoints, or restart files
target defaults. Select them for the individual task, preferably by exact
filename:

```bash
joyrun submit molecule.inp -t cluster/orca \
  --include molecule.xyz \
  --include molecule.gbw \
  --dry-run --json
```

`--include` is repeatable and valid only for `push.mode: entry`. Every
requested pattern must match an uploaded file; exclusions and upload limits
still take precedence.

For `push.mode: workdir`, every non-excluded file in the work directory is an
input snapshot. Inspect it carefully and set realistic limits:

```yaml
source:
  kind: directory
push:
  mode: workdir
  exclude: ["*.out", "*.log", "restart-old/"]
  limits:
    max_files: 500
    max_total_size: 20GiB
```

Exclusions, `.joyrunignore`, and built-in `.git/` and `.joyrun/` exclusions
take precedence over inclusion. Do not use `--allow-project-root` to compensate
for an incorrectly selected source.

## Adapt the job script

Keep the user's working Slurm script intact unless a change is required for
JoyRun's isolated remote work directory. Replace only task-specific values:

- `{{ .Input }}` — selected entry filename;
- `{{ .Stem }}` — entry filename without its final extension;
- `{{ .Name }}` — source work-directory name;
- `{{ .TaskID }}` — JoyRun task ID;
- `{{ .WorkDir }}` — absolute remote work directory;
- `{{ .Partition.Name }}` — resolved allowed Slurm partition;
- `{{ .Partition.CoresPerNode }}` — configured core fact, or zero if unknown;
- `{{ .Partition.MemoryPerNode }}` — configured memory fact, or empty if unknown;
- `{{ .Params.name }}` — resolved target parameter.

JoyRun shell-quotes substitutions. Templates allow direct substitutions only;
do not introduce template functions, pipelines, conditions, or loops.

Example adaptation:

```yaml
targets:
  para-amd/gaussian:
    cluster: para-amd
    software:
      name: gaussian
    placement:
      default_partition: normal
      allowed_partitions: [normal, highio]
    source:
      kind: file
      patterns: ["*.gjf", "*.com"]
    script: |
      #!/bin/bash
      #SBATCH --partition={{ .Partition.Name }}
      #SBATCH --job-name={{ .Stem }}

      module load gaussian
      g16 < {{ .Input }} > {{ .Stem }}.log
    push:
      mode: entry
      exclude: ["*.log"]
      limits:
        max_files: 10
        max_total_size: 1GiB
    pull:
      default: ["*.log", "*.chk", "*.fchk"]
    logs: ["{{ .Stem }}.log"]
```

Record verified cluster facts once and constrain each Target separately:

```yaml
clusters:
  para-amd:
    host: para-amd
    scheduler: slurm
    remote_root: /scratch/user/joyrun
    partitions:
      normal:
        cores_per_node: 64
        memory_per_node: 256GiB
      highio:
        cores_per_node: 64
```

Omit unknown fields rather than estimating them. `software.name` identifies
what the Target runs; it does not activate a built-in software parser.
`placement.allowed_partitions` is authoritative, and
`placement.default_partition` must be a member. JoyRun never parses `#SBATCH`
directives or scientific inputs to infer these facts.

Submit this target with a concrete file, not its containing directory:

```bash
joyrun submit benzene/benzene_opt.gjf \
  -t para-amd/gaussian \
  --dry-run --json
```

Expose only values the user may reasonably vary as parameters. Parameter names
must be `lowercase_snake_case`. Supported types are `string`, `int`, `float`,
and `bool`; specifications may use `default`, `required`, `choices`, and
`description`. Do not invent a default when the correct value is unknown.

## Define outputs and logs

`pull.default` should contain useful generated outputs, not every remote file.
Large restart files should be included only when the user's normal workflow
requires them; users can request additional files with `pull --include` or
`pull --all`.

Define `logs` using paths the script actually creates. A log using
`{{ .Stem }}` requires a file source. Prefer a stable scheduler or application
output name when possible. Do not list an expected output merely because it is
conventional for the software—confirm it from the script or user.

## Validate

Run these in order:

```bash
joyrun config validate --json
joyrun target show TARGET --json
joyrun target params TARGET --json
joyrun doctor TARGET --json
joyrun submit SOURCE -t TARGET --dry-run --json
```

When the user asks about current capacity, query the Target without submitting
a job:

```bash
joyrun target nodes TARGET --partition APPROVED_PARTITION --json
```

Only use `placement.allowed_partitions`. Treat the result as a timestamped
Slurm observation, not a prediction that a job will start immediately.

`doctor` performs connectivity and capability checks but does not submit a
job. Treat failed checks as blocking; report actionable warnings without
silently changing cluster settings.

In the dry-run result, verify:

- source kind, entry, and work directory;
- no empty `Input` or `Stem` substitutions;
- exact included and ignored files;
- file count and total size;
- resolved parameters and rendered script;
- software identity, selected partition, known hardware facts, and their
  relationship to the rendered resource request;
- target, cluster, and planned remote directory;
- pull and log paths derived from the intended input.

Stop if the manifest contains unrelated inputs, outputs, credentials, the
project root, or unexpectedly large files. Stop if the script contains empty
filenames or differs materially from the proven Slurm workflow. Present the
validated target and dry-run result to the user; submit only upon a separate,
explicit request.
