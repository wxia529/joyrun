package scheduler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/remote"
)

type Slurm struct {
	Runner remote.Runner
}

type JobStatus struct {
	State    string
	RawState string
	Elapsed  string
	ExitCode string
	Reason   string
	Start    string
	End      string
}

func (s Slurm) Submit(ctx context.Context, host, workDir, taskID string) (string, error) {
	command := "cd " + remote.Quote(workDir) +
		" && jobid=$(sbatch --parsable --comment=" + remote.Quote("joyrun:"+taskID) +
		" --output=joyrun-slurm-%j.log joyrun-job.sh) && jobid=${jobid%%;*}" +
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

func (s Slurm) FindByTaskID(ctx context.Context, host, taskID string, since time.Time) (string, error) {
	comment := "joyrun:" + taskID
	queries := []string{
		"squeue -h -o '%A|%k'",
		"sacct -n -X --starttime " + remote.Quote(since.UTC().Format("2006-01-02T15:04:05")) +
			" --format=JobIDRaw,Comment --parsable2",
	}
	matches := map[string]bool{}
	for _, command := range queries {
		stdout, stderr, err := s.Runner.Exec(ctx, host, command, nil)
		if err != nil {
			return "", fault.Wrap("STATUS_FAILED",
				message("cannot reconcile JoyRun task with Slurm", stderr), true, err)
		}
		for _, line := range strings.Split(stdout, "\n") {
			fields := strings.SplitN(strings.TrimSpace(line), "|", 3)
			if len(fields) < 2 || fields[1] != comment {
				continue
			}
			id := strings.TrimSpace(fields[0])
			if _, err := strconv.ParseUint(id, 10, 64); err == nil {
				matches[id] = true
			}
		}
		if len(matches) > 0 {
			break
		}
	}
	if len(matches) == 0 {
		return "", nil
	}
	if len(matches) > 1 {
		return "", fault.New("SUBMIT_AMBIGUOUS",
			fmt.Sprintf("multiple Slurm jobs carry the JoyRun task marker %q", comment), false)
	}
	for id := range matches {
		return id, nil
	}
	panic("unreachable")
}

func LogName(jobID string) string {
	return "joyrun-slurm-" + jobID + ".log"
}

func (s Slurm) Status(ctx context.Context, host, id string) (JobStatus, error) {
	command := fmt.Sprintf(
		"squeue_output=$(squeue -h -j %s -o '%%T|%%M||%%R|%%S|%%e' 2>&1); squeue_status=$?; "+
			"if [ \"$squeue_status\" -eq 0 ]; then row=$(printf '%%s\\n' \"$squeue_output\" | head -n1); fi; "+
			"if [ -z \"$row\" ]; then "+
			"sacct_output=$(sacct -n -X -j %s --format=State,Elapsed,ExitCode,Reason,Start,End --parsable2 2>&1); sacct_status=$?; "+
			"if [ \"$sacct_status\" -ne 0 ]; then "+
			"printf 'squeue: %%s\\nsacct: %%s\\n' \"$squeue_output\" \"$sacct_output\" >&2; exit 1; fi; "+
			"row=$(printf '%%s\\n' \"$sacct_output\" | head -n1); fi; "+
			"printf '%%s' \"$row\"",
		remote.Quote(id), remote.Quote(id))
	stdout, stderr, err := s.Runner.Exec(ctx, host, command, nil)
	if err != nil {
		return JobStatus{}, fault.Wrap("STATUS_FAILED", message("cannot query Slurm job", stderr), true, err)
	}
	fields := strings.SplitN(strings.TrimSpace(stdout), "|", 7)
	if len(fields) == 0 || strings.TrimSpace(fields[0]) == "" {
		return JobStatus{State: "unknown"}, nil
	}
	for len(fields) < 6 {
		fields = append(fields, "")
	}
	raw := strings.TrimSpace(strings.TrimSuffix(fields[0], "+"))
	return JobStatus{
		State: normalize(raw), RawState: raw,
		Elapsed:  strings.TrimSpace(fields[1]),
		ExitCode: strings.TrimSpace(fields[2]),
		Reason:   strings.TrimSpace(fields[3]),
		Start:    cleanTime(fields[4]),
		End:      cleanTime(fields[5]),
	}, nil
}

func cleanTime(value string) string {
	value = strings.TrimSpace(value)
	switch value {
	case "", "Unknown", "N/A", "None":
		return ""
	default:
		return value
	}
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
	case "PENDING", "CONFIGURING", "SUSPENDED", "REQUEUED", "REQUEUE_FED", "REQUEUE_HOLD":
		return "queued"
	case "RUNNING", "COMPLETING", "RESIZING", "SIGNALING", "STAGE_OUT":
		return "running"
	case "COMPLETED":
		return "completed"
	case "CANCELLED", "CANCELLED+":
		return "cancelled"
	case "FAILED", "TIMEOUT", "NODE_FAIL", "OUT_OF_MEMORY", "PREEMPTED", "BOOT_FAIL",
		"DEADLINE", "REVOKED", "SPECIAL_EXIT":
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
