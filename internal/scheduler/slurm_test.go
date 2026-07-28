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
	"time"
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
	id, err := (Slurm{Runner: runner}).Submit(
		context.Background(), "unused", "/remote/work", "jr_test", "community")
	if err != nil {
		t.Fatal(err)
	}
	if id != "12345" ||
		!strings.Contains(runner.command, "--output=joyrun-slurm-%j.log") ||
		!strings.Contains(runner.command, "--comment='joyrun:jr_test'") ||
		!strings.Contains(runner.command, "--partition='community'") {
		t.Fatalf("unexpected submit command: %q", runner.command)
	}
}

type reconciliationRunner struct {
	outputs []string
}

func (r *reconciliationRunner) Exec(_ context.Context, _, _ string, _ io.Reader) (string, string, error) {
	output := r.outputs[0]
	r.outputs = r.outputs[1:]
	return output, "", nil
}

func TestFindByTaskIDFallsBackToAccounting(t *testing.T) {
	runner := &reconciliationRunner{outputs: []string{
		"999|other\n",
		"12345|joyrun:jr_test|\n",
	}}
	id, err := (Slurm{Runner: runner}).FindByTaskID(
		context.Background(), "unused", "jr_test", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if id != "12345" {
		t.Fatalf("unexpected reconciled ID %q", id)
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
	_, err := (Slurm{Runner: localShellRunner{path: bin}}).Status(context.Background(), "unused", "123")
	if err == nil {
		t.Fatal("expected scheduler command failure")
	}
}

func TestStatusFallsBackToAccounting(t *testing.T) {
	bin := t.TempDir()
	writeCommand(t, bin, "squeue", "#!/bin/sh\nexit 0\n")
	writeCommand(t, bin, "sacct", "#!/bin/sh\nprintf 'COMPLETED|01:02:03|0:0|None|2026-07-28T10:00:00|2026-07-28T11:02:03|\\n'\n")
	status, err := (Slurm{Runner: localShellRunner{path: bin}}).Status(context.Background(), "unused", "123")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "completed" || status.RawState != "COMPLETED" ||
		status.Elapsed != "01:02:03" || status.ExitCode != "0:0" || status.Reason != "None" {
		t.Fatalf("unexpected status: %#v", status)
	}
	if status.Start != "2026-07-28T10:00:00" || status.End != "2026-07-28T11:02:03" {
		t.Fatalf("missing scheduler timestamps: %#v", status)
	}
}

func TestNormalizeExtendedSlurmStates(t *testing.T) {
	tests := map[string]string{
		"REQUEUED":     "queued",
		"SIGNALING":    "running",
		"STAGE_OUT":    "running",
		"REVOKED":      "failed",
		"SPECIAL_EXIT": "failed",
	}
	for raw, want := range tests {
		if got := normalize(raw); got != want {
			t.Fatalf("normalize(%q) = %q, want %q", raw, got, want)
		}
	}
}

func writeCommand(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
