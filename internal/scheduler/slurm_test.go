//go:build !windows

package scheduler

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type localShellRunner struct {
	path string
}

type recordingRunner struct {
	command string
}

func (r *recordingRunner) Exec(_ context.Context, _, command string, _ io.Reader) (string, string, error) {
	r.command = command
	return "12345\n", "", nil
}

func TestSubmitForcesJoyRunSchedulerLog(t *testing.T) {
	runner := &recordingRunner{}
	id, err := (Slurm{Runner: runner}).Submit(context.Background(), "unused", "/remote/work")
	if err != nil {
		t.Fatal(err)
	}
	if id != "12345" || !strings.Contains(runner.command, "--output=joyrun-slurm-%j.log") {
		t.Fatalf("unexpected submit command: %q", runner.command)
	}
}

func (r localShellRunner) Exec(ctx context.Context, _, command string, stdin io.Reader) (string, string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = append(os.Environ(), "PATH="+r.path+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func TestStatusPropagatesSchedulerCommandFailure(t *testing.T) {
	bin := t.TempDir()
	writeCommand(t, bin, "squeue", "#!/bin/sh\necho queue unavailable >&2\nexit 1\n")
	writeCommand(t, bin, "sacct", "#!/bin/sh\necho accounting unavailable >&2\nexit 1\n")
	_, _, err := (Slurm{Runner: localShellRunner{path: bin}}).Status(context.Background(), "unused", "123")
	if err == nil {
		t.Fatal("expected scheduler command failure")
	}
}

func TestStatusFallsBackToAccounting(t *testing.T) {
	bin := t.TempDir()
	writeCommand(t, bin, "squeue", "#!/bin/sh\nexit 0\n")
	writeCommand(t, bin, "sacct", "#!/bin/sh\nprintf 'COMPLETED|\\n'\n")
	state, raw, err := (Slurm{Runner: localShellRunner{path: bin}}).Status(context.Background(), "unused", "123")
	if err != nil {
		t.Fatal(err)
	}
	if state != "completed" || raw != "COMPLETED" {
		t.Fatalf("unexpected state: %q, %q", state, raw)
	}
}

func writeCommand(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
