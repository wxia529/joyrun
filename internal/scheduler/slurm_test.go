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
	output  string
	calls   int
}

func (r *recordingRunner) Exec(_ context.Context, _, command string, _ io.Reader) (string, string, error) {
	r.command = command
	r.calls++
	if r.output != "" {
		return r.output, "", nil
	}
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
		!strings.Contains(runner.command, "chmod 700 joyrun-job.sh") ||
		!strings.Contains(runner.command, "--output=joyrun-slurm-%j.log") ||
		!strings.Contains(runner.command, "--comment='joyrun:jr_test'") ||
		!strings.Contains(runner.command, "--partition='community'") {
		t.Fatalf("unexpected submit command: %q", runner.command)
	}
}

func TestSubmitManyUsesOneRemoteInvocationAndParsesIndependentResults(t *testing.T) {
	runner := &recordingRunner{output: "OK\x00jr_one\x00101\x00ERR\x00jr_two\x00partition unavailable\x00"}
	result, err := (Slurm{Runner: runner}).SubmitMany(context.Background(), "unused", []BatchJob{
		{TaskID: "jr_one", WorkDir: "/remote/jr_one/work", Partition: "cpu"},
		{TaskID: "jr_two", WorkDir: "/remote/jr_two/work", Partition: "cpu"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 || result.SchedulerIDs["jr_one"] != "101" ||
		result.Failures["jr_two"] != "partition unavailable" {
		t.Fatalf("unexpected batch submission: calls=%d result=%#v", runner.calls, result)
	}
	if strings.Count(runner.command, "sbatch --parsable") != 2 ||
		!strings.Contains(runner.command, "/remote/jr_one/scheduler_id") {
		t.Fatalf("unexpected batch command: %q", runner.command)
	}
}

func TestSubmitManyRemoteShellWritesIndependentMarkers(t *testing.T) {
	root := t.TempDir()
	for _, task := range []string{"jr_one", "jr_two"} {
		work := filepath.Join(root, task, "work")
		if err := os.MkdirAll(work, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, "joyrun-job.sh"), []byte("#!/bin/sh\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	writeCommand(t, bin, "sbatch", "#!/bin/sh\ncase \"$*\" in *jr_one*) printf '201\\n';; *) printf '202\\n';; esac\n")
	result, err := (Slurm{Runner: localShellRunner{path: bin}}).SubmitMany(
		context.Background(), "unused", []BatchJob{
			{TaskID: "jr_one", WorkDir: filepath.ToSlash(filepath.Join(root, "jr_one", "work")), Partition: "cpu"},
			{TaskID: "jr_two", WorkDir: filepath.ToSlash(filepath.Join(root, "jr_two", "work")), Partition: "cpu"},
		})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []string{"jr_one", "jr_two"} {
		data, err := os.ReadFile(filepath.Join(root, task, "scheduler_id"))
		if err != nil || strings.TrimSpace(string(data)) == "" {
			t.Fatalf("missing scheduler marker for %s: %q, %v", task, data, err)
		}
		if result.SchedulerIDs[task] == "" {
			t.Fatalf("missing parsed scheduler ID for %s: %#v", task, result)
		}
	}
}

func TestSubmissionDefinitelyRejectedDistinguishesSSHTransportFailure(t *testing.T) {
	rejected := exec.Command("sh", "-c", "exit 1").Run()
	transport := exec.Command("sh", "-c", "exit 255").Run()
	if !SubmissionDefinitelyRejected(rejected) {
		t.Fatal("remote command exit 1 should be a definitive rejection")
	}
	if SubmissionDefinitelyRejected(transport) {
		t.Fatal("SSH-style exit 255 must remain uncertain")
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

func TestStatusesQueriesJobsInOneRemoteInvocation(t *testing.T) {
	runner := &recordingRunner{output: strings.Join([]string{
		"A|123|COMPLETED|00:10:00|0:0|None|2026-07-28T10:00:00|2026-07-28T10:10:00",
		"A|456|PENDING|00:00:00|0:0|Priority|Unknown|Unknown",
		"Q|456|RUNNING|00:00:12||node01|2026-07-28T10:11:00|N/A",
	}, "\n")}
	statuses, err := (Slurm{Runner: runner}).Statuses(
		context.Background(), "unused", []string{"123", "456"})
	if err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 {
		t.Fatalf("batch status used %d remote calls, want 1", runner.calls)
	}
	if !strings.Contains(runner.command, "123,456") {
		t.Fatalf("batch command omitted job IDs: %q", runner.command)
	}
	if statuses["123"].State != "completed" || statuses["123"].ExitCode != "0:0" {
		t.Fatalf("unexpected accounting status: %#v", statuses["123"])
	}
	if statuses["456"].State != "running" || statuses["456"].RawState != "RUNNING" {
		t.Fatalf("queue row did not override accounting status: %#v", statuses["456"])
	}
}

func TestStatusesReturnsUnknownForPurgedJob(t *testing.T) {
	runner := &recordingRunner{output: "\n"}
	statuses, err := (Slurm{Runner: runner}).Statuses(
		context.Background(), "unused", []string{"123"})
	if err != nil {
		t.Fatal(err)
	}
	if statuses["123"].State != "unknown" {
		t.Fatalf("unexpected purged job status: %#v", statuses["123"])
	}
}

func TestStatusesRemoteShellProtocol(t *testing.T) {
	bin := t.TempDir()
	writeCommand(t, bin, "squeue", "#!/bin/sh\nprintf '456|RUNNING|00:00:12||node01|2026-07-29T10:00:00|N/A\\n'\n")
	writeCommand(t, bin, "sacct", "#!/bin/sh\nprintf '123|COMPLETED|00:10:00|0:0|None|2026-07-29T09:50:00|2026-07-29T10:00:00\\n'\n")
	statuses, err := (Slurm{Runner: localShellRunner{path: bin}}).Statuses(
		context.Background(), "unused", []string{"123", "456"})
	if err != nil {
		t.Fatal(err)
	}
	if statuses["123"].State != "completed" || statuses["456"].State != "running" {
		t.Fatalf("unexpected shell protocol statuses: %#v", statuses)
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
