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

type BatchJob struct {
	TaskID    string
	WorkDir   string
	Partition string
}

type BatchSubmitResult struct {
	SchedulerIDs map[string]string
	Failures     map[string]string
	Rejected     map[string]bool
}

type NodeInfo struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	CPUs      int    `json:"cpus"`
	MemoryMB  int64  `json:"memory_mb"`
	GRES      string `json:"gres,omitempty"`
	Partition string `json:"partition"`
}

type NodeSummary struct {
	Total       int `json:"total"`
	Idle        int `json:"idle"`
	Mixed       int `json:"mixed"`
	Allocated   int `json:"allocated"`
	Unavailable int `json:"unavailable"`
}

type NodesResult struct {
	Summary NodeSummary `json:"summary"`
	Nodes   []NodeInfo  `json:"nodes"`
}

func (s Slurm) Submit(ctx context.Context, host, workDir, taskID, partition string) (string, error) {
	command := "cd " + remote.Quote(workDir) +
		" && chmod 700 joyrun-job.sh" +
		" && printf '%s\\n' " + remote.Quote(taskID) + " > ../submit_started.tmp" +
		" && mv -f ../submit_started.tmp ../submit_started" +
		" && sbatch_output=$(sbatch --parsable --comment=" + remote.Quote("joyrun:"+taskID) +
		" --partition=" + remote.Quote(partition) +
		" --output=joyrun-slurm-%j.log joyrun-job.sh 2>../submit_stderr.tmp); sbatch_status=$?; " +
		"if [ \"$sbatch_status\" -ne 0 ]; then " +
		"printf 'JOYRUN_SUBMIT_REJECTED\\n' >&2; cat ../submit_stderr.tmp >&2; " +
		"rm -f ../submit_stderr.tmp; exit \"$sbatch_status\"; fi; " +
		"rm -f ../submit_stderr.tmp; jobid=${sbatch_output%%;*}; " +
		"if ! printf '%s' \"$jobid\" | grep -Eq '^[0-9]+$'; then " +
		"printf 'JOYRUN_SUBMIT_UNCERTAIN: invalid scheduler output\\n' >&2; exit 1; fi; " +
		"printf '%s\\n' \"$jobid\" > ../scheduler_id.tmp" +
		" && mv -f ../scheduler_id.tmp ../scheduler_id && printf '%s\\n' \"$jobid\""
	stdout, stderr, err := s.Runner.Exec(ctx, host, command, nil)
	if err != nil {
		code := "SUBMIT_FAILED"
		if strings.Contains(stderr, "JOYRUN_SUBMIT_REJECTED") {
			code = "SUBMIT_REJECTED"
		}
		return "", fault.Wrap(code, message("Slurm submission failed", stderr), code != "SUBMIT_REJECTED", err)
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

// SubmitMany submits independent Slurm jobs through one remote shell. Each
// accepted job receives its own atomic scheduler marker for reconciliation.
func (s Slurm) SubmitMany(ctx context.Context, host string, jobs []BatchJob) (BatchSubmitResult, error) {
	result := BatchSubmitResult{
		SchedulerIDs: map[string]string{},
		Failures:     map[string]string{},
		Rejected:     map[string]bool{},
	}
	if len(jobs) == 0 {
		return result, nil
	}
	var command strings.Builder
	for _, job := range jobs {
		command.WriteString("taskid=")
		command.WriteString(remote.Quote(job.TaskID))
		command.WriteString("; cd ")
		command.WriteString(remote.Quote(job.WorkDir))
		command.WriteString(" && chmod 700 joyrun-job.sh && printf '%s\\n' \"$taskid\" > ../submit_started.tmp && mv -f ../submit_started.tmp ../submit_started && output=$(sbatch --parsable --comment=")
		command.WriteString(remote.Quote("joyrun:" + job.TaskID))
		command.WriteString(" --partition=")
		command.WriteString(remote.Quote(job.Partition))
		command.WriteString(" --output=joyrun-slurm-%j.log joyrun-job.sh 2>&1); status=$?; ")
		command.WriteString("if [ \"$status\" -eq 0 ]; then jobid=${output%%;*}; ")
		command.WriteString("if printf '%s' \"$jobid\" | grep -Eq '^[0-9]+$'; then ")
		command.WriteString("printf '%s\\n' \"$jobid\" > ")
		command.WriteString(remote.Quote(pathJoinParent(job.WorkDir, "scheduler_id.tmp")))
		command.WriteString(" && mv ")
		command.WriteString(remote.Quote(pathJoinParent(job.WorkDir, "scheduler_id.tmp")))
		command.WriteString(" ")
		command.WriteString(remote.Quote(pathJoinParent(job.WorkDir, "scheduler_id")))
		command.WriteString("; printf 'OK\\0%s\\0%s\\0' \"$taskid\" \"$jobid\"; ")
		command.WriteString("else printf 'ERR\\0%s\\0%s\\0' \"$taskid\" ")
		command.WriteString("\"unexpected sbatch output: $output\"; fi; ")
		command.WriteString("else printf 'REJ\\0%s\\0%s\\0' \"$taskid\" \"$output\"; fi; ")
	}
	stdout, stderr, err := s.Runner.Exec(ctx, host, command.String(), nil)
	if err != nil {
		return result, fault.Wrap("SUBMIT_FAILED",
			message("batch Slurm submission connection failed", stderr), true, err)
	}
	records := strings.Split(stdout, "\x00")
	for index := 0; index+2 < len(records); index += 3 {
		kind, taskID, value := records[index], records[index+1], records[index+2]
		switch kind {
		case "OK":
			if _, err := strconv.ParseUint(value, 10, 64); err == nil {
				result.SchedulerIDs[taskID] = value
			} else {
				result.Failures[taskID] = "invalid scheduler ID: " + value
			}
		case "ERR":
			result.Failures[taskID] = strings.TrimSpace(value)
		case "REJ":
			result.Failures[taskID] = strings.TrimSpace(value)
			result.Rejected[taskID] = true
		}
	}
	return result, nil
}

func pathJoinParent(workDir, name string) string {
	workDir = strings.TrimSuffix(workDir, "/")
	index := strings.LastIndex(workDir, "/")
	if index < 0 {
		return "../" + name
	}
	return workDir[:index] + "/" + name
}

func SubmissionDefinitelyRejected(err error) bool {
	if err == nil {
		return false
	}
	if fault.As(err).Code == "SUBMIT_REJECTED" || strings.Contains(err.Error(), "JOYRUN_SUBMIT_REJECTED") {
		return true
	}
	return false
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

// Statuses queries multiple Slurm jobs in one remote shell invocation. Queue
// rows take precedence over accounting rows because they describe the current
// state of active jobs.
func (s Slurm) Statuses(ctx context.Context, host string, ids []string) (map[string]JobStatus, error) {
	result := make(map[string]JobStatus, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	seen := make(map[string]bool, len(ids))
	ordered := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		if _, err := strconv.ParseUint(id, 10, 64); err != nil {
			return nil, fault.Wrap("STATUS_FAILED", fmt.Sprintf("invalid Slurm job ID %q", id), false, err)
		}
		seen[id] = true
		ordered = append(ordered, id)
		result[id] = JobStatus{State: "unknown"}
	}
	if len(ordered) == 0 {
		return result, nil
	}
	idList := strings.Join(ordered, ",")
	command :=
		"ids=" + remote.Quote(idList) + "; " +
			"queue_output=$(squeue -h -j \"$ids\" -o '%A|%T|%M||%R|%S|%e' 2>&1); queue_status=$?; " +
			"account_output=$(sacct -n -X -j \"$ids\" --format=JobIDRaw,State,Elapsed,ExitCode,Reason,Start,End --parsable2 2>&1); account_status=$?; " +
			"if [ \"$queue_status\" -ne 0 ] && [ \"$account_status\" -ne 0 ]; then " +
			"printf 'squeue: %s\\nsacct: %s\\n' \"$queue_output\" \"$account_output\" >&2; exit 1; fi; " +
			"if [ \"$account_status\" -eq 0 ]; then printf '%s\\n' \"$account_output\" | sed '/^[[:space:]]*$/d;s/^/A|/'; fi; " +
			"if [ \"$queue_status\" -eq 0 ]; then printf '%s\\n' \"$queue_output\" | sed '/^[[:space:]]*$/d;s/^/Q|/'; fi"
	stdout, stderr, err := s.Runner.Exec(ctx, host, command, nil)
	if err != nil {
		return nil, fault.Wrap("STATUS_FAILED", message("cannot query Slurm jobs", stderr), true, err)
	}
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), "|", 9)
		if len(fields) < 3 {
			continue
		}
		source := fields[0]
		id := strings.TrimSpace(fields[1])
		if !seen[id] {
			continue
		}
		if source == "A" {
			result[id] = statusFromFields(fields[2:])
			continue
		}
		if source == "Q" {
			// Queue information is printed last and intentionally overrides a
			// possibly stale accounting row.
			result[id] = statusFromFields(fields[2:])
		}
	}
	return result, nil
}

func statusFromFields(fields []string) JobStatus {
	for len(fields) < 6 {
		fields = append(fields, "")
	}
	raw := strings.TrimSpace(strings.TrimSuffix(fields[0], "+"))
	if raw == "" {
		return JobStatus{State: "unknown"}
	}
	return JobStatus{
		State: normalize(raw), RawState: raw,
		Elapsed:  strings.TrimSpace(fields[1]),
		ExitCode: strings.TrimSpace(fields[2]),
		Reason:   strings.TrimSpace(fields[3]),
		Start:    cleanTime(fields[4]),
		End:      cleanTime(fields[5]),
	}
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

func (s Slurm) Nodes(ctx context.Context, host, partition string) (NodesResult, error) {
	partition = strings.TrimSpace(partition)
	if partition == "" {
		return NodesResult{}, fault.New("TARGET_STATUS_INVALID", "resolved partition is empty", false)
	}
	command := "LC_ALL=C sinfo -N -h -p " + remote.Quote(partition) +
		" -o '%P|%N|%T|%c|%m|%G'"
	stdout, stderr, err := s.Runner.Exec(ctx, host, command, nil)
	if err != nil {
		return NodesResult{}, fault.Wrap("NODES_QUERY_FAILED",
			message("cannot query Slurm nodes for partition "+partition, stderr), true, err)
	}
	return parseNodes(stdout)
}

func parseNodes(output string) (NodesResult, error) {
	result := NodesResult{Nodes: []NodeInfo{}}
	seen := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "|", 6)
		if len(fields) != 6 {
			return NodesResult{}, fault.New("NODES_QUERY_FAILED",
				fmt.Sprintf("unexpected sinfo row %q", line), false)
		}
		name := strings.TrimSpace(fields[1])
		if name == "" {
			return NodesResult{}, fault.New("NODES_QUERY_FAILED",
				fmt.Sprintf("sinfo returned an empty node name in row %q", line), false)
		}
		if seen[name] {
			continue
		}
		cpus, err := strconv.Atoi(strings.TrimSpace(fields[3]))
		if err != nil {
			return NodesResult{}, fault.Wrap("NODES_QUERY_FAILED",
				fmt.Sprintf("invalid CPU count for node %s", name), false, err)
		}
		memory, err := strconv.ParseInt(strings.TrimSpace(fields[4]), 10, 64)
		if err != nil {
			return NodesResult{}, fault.Wrap("NODES_QUERY_FAILED",
				fmt.Sprintf("invalid memory for node %s", name), false, err)
		}
		state := normalizeNodeState(fields[2])
		node := NodeInfo{
			Name: name, State: state, CPUs: cpus, MemoryMB: memory,
			GRES:      strings.TrimSpace(fields[5]),
			Partition: strings.TrimSuffix(strings.TrimSpace(fields[0]), "*"),
		}
		seen[name] = true
		result.Nodes = append(result.Nodes, node)
		result.Summary.Total++
		switch nodeStateCategory(state) {
		case "idle":
			result.Summary.Idle++
		case "mixed":
			result.Summary.Mixed++
		case "allocated":
			result.Summary.Allocated++
		default:
			result.Summary.Unavailable++
		}
	}
	return result, nil
}

func normalizeNodeState(raw string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(raw), "*~#@%$!^"))
}

func nodeStateCategory(state string) string {
	switch state {
	case "idle":
		return "idle"
	case "mixed", "mix":
		return "mixed"
	case "allocated", "alloc", "completing", "comp":
		return "allocated"
	default:
		return "unavailable"
	}
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
