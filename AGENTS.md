# Repository Guidelines

## Project Structure & Module Organization

JoyRun is a Go 1.24 CLI. The executable entry point is
`cmd/joyrun/main.go`; application orchestration lives in `internal/app`, and
command parsing/output lives in `internal/cli`. Domain types are in
`internal/model`. Keep infrastructure behind the focused packages:
`config`, `store`, `scheduler`, `remote`, `transfer`, `template`, and
`manifest`. Tests sit beside implementation files as `*_test.go`.

User-facing examples belong in `examples/`, architecture notes in `docs/`, and
operational behavior for coding agents in `SKILL.md`. Do not treat
`user-temp/` as production source.

## Build, Test, and Development Commands

```bash
make build                 # build bin/joyrun
make test                  # run all Go tests
make check                 # run tests and go vet
go test -race ./...        # detect data races
go run ./cmd/joyrun version
GOOS=windows GOARCH=amd64 go build -o joyrun.exe ./cmd/joyrun
```

Before handing off a change, run `gofmt` on edited Go files,
`go test ./...`, `go vet ./...`, and `git diff --check`. Exercise Windows
cross-compilation when modifying paths, transfer code, or platform-specific
files.

## Coding Style & Naming Conventions

Follow standard Go formatting and package conventions. Use short lowercase
package names, exported `PascalCase` identifiers, and descriptive sentinel
error codes such as `SOURCE_KIND_MISMATCH`. Pass `context.Context` through
remote, scheduler, transfer, and persistence operations. Keep CLI commands
non-interactive. Under `--json`, stdout must contain exactly one JSON document;
send progress and diagnostics to stderr.

## Testing Guidelines

Use Go's `testing` package and name tests `TestBehaviorBeingVerified`.
Prefer table-driven tests for validation and state mappings. Unit-test local
logic without HPC access; inject fake remote, scheduler, or transfer
implementations for pipelines. Add regression tests for every bug fix. There
is no numeric coverage gate, but changed behavior must be covered.

## Commit & Pull Request Guidelines

The current history uses Conventional Commit style, for example
`feat: implement JoyRun HPC task runner`. Use focused subjects such as
`fix: protect input files during pull`; avoid mixing unrelated cleanup.

Pull requests should explain the user-visible behavior, design tradeoffs, and
verification commands. Link relevant issues and include representative CLI or
JSON output when interfaces change. Update `README.md`, `docs/design.md`,
examples, and `SKILL.md` when their contracts are affected.

## Security & Compatibility

Never store SSH credentials; rely on OpenSSH configuration and host-key
verification. Preserve unrelated working-tree changes. The first public
SQLite schema is `stable/stable-1`. Future stable schema changes require an
explicit, tested migration; never add silent or destructive migrations.
