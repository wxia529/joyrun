package scheduler

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/remote"
)

type Slurm struct {
	Runner remote.Runner
}

func (s Slurm) Submit(ctx context.Context, host, workDir string) (string, error) {
	command := "cd " + remote.Quote(workDir) +
		" && jobid=$(sbatch --parsable joyrun-job.sh) && jobid=${jobid%%;*}" +
		" && printf '%s\\n' \"$jobid\" > ../scheduler_id.tmp" +
		" && mv ../scheduler_id.tmp ../scheduler_id && printf '%s\\n' \"$jobid\""
	stdout, stderr, err := s.Runner.Exec(ctx, host, command, nil)
	if err != nil {
		return "", fault.Wrap("SUBMIT_FAILED", message("Slurm submission failed", stderr), true, err)
	}
	id := strings.TrimSpace(stdout)
	if head, _, ok := strings.Cut(id, ";"); ok {
		id = head
	}
	if id == "" {
		return "", fault.New("SUBMIT_FAILED", "sbatch returned an empty scheduler ID", true)
	}
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return "", fault.Wrap("SUBMIT_FAILED", fmt.Sprintf("unexpected sbatch output %q", id), false, err)
	}
	return id, nil
}

func (s Slurm) Status(ctx context.Context, host, id string) (string, string, error) {
	command := fmt.Sprintf(
		"squeue_output=$(squeue -h -j %s -o %%T 2>&1); squeue_status=$?; "+
			"if [ \"$squeue_status\" -eq 0 ]; then state=$(printf '%%s\\n' \"$squeue_output\" | head -n1); fi; "+
			"if [ -z \"$state\" ]; then "+
			"sacct_output=$(sacct -n -X -j %s --format=State --parsable2 2>&1); sacct_status=$?; "+
			"if [ \"$sacct_status\" -ne 0 ]; then "+
			"printf 'squeue: %%s\\nsacct: %%s\\n' \"$squeue_output\" \"$sacct_output\" >&2; exit 1; fi; "+
			"state=$(printf '%%s\\n' \"$sacct_output\" | head -n1 | cut -d'|' -f1); fi; "+
			"printf '%%s' \"$state\"",
		remote.Quote(id), remote.Quote(id))
	stdout, stderr, err := s.Runner.Exec(ctx, host, command, nil)
	if err != nil {
		return "", "", fault.Wrap("STATUS_FAILED", message("cannot query Slurm job", stderr), true, err)
	}
	raw := strings.TrimSpace(strings.TrimSuffix(stdout, "+"))
	if raw == "" {
		return "unknown", "", nil
	}
	return normalize(raw), raw, nil
}

func (s Slurm) Cancel(ctx context.Context, host, id string) error {
	_, stderr, err := s.Runner.Exec(ctx, host, "scancel "+remote.Quote(id), nil)
	if err != nil {
		return fault.Wrap("CANCEL_FAILED", message("cannot cancel Slurm job", stderr), true, err)
	}
	return nil
}

func normalize(raw string) string {
	switch strings.ToUpper(strings.Fields(raw)[0]) {
	case "PENDING", "CONFIGURING", "SUSPENDED":
		return "queued"
	case "RUNNING", "COMPLETING", "RESIZING":
		return "running"
	case "COMPLETED":
		return "completed"
	case "CANCELLED", "CANCELLED+":
		return "cancelled"
	case "FAILED", "TIMEOUT", "NODE_FAIL", "OUT_OF_MEMORY", "PREEMPTED", "BOOT_FAIL", "DEADLINE":
		return "failed"
	default:
		return "unknown"
	}
}

func message(prefix, stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return prefix
	}
	return prefix + ": " + stderr
}
