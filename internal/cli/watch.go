package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/wxia529/joyrun/internal/daemon"
	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/model"
	"github.com/wxia529/joyrun/internal/store"
)

const watchRefreshInterval = 5 * time.Second

type watchOutput struct {
	Tasks       []model.TaskSummary `json:"tasks"`
	Total       int                 `json:"total"`
	Hidden      int                 `json:"hidden"`
	GeneratedAt time.Time           `json:"generated_at"`
	ProjectID   string              `json:"project_id,omitempty"`
	Target      string              `json:"target,omitempty"`
	State       string              `json:"state,omitempty"`
	Attention   bool                `json:"attention_only,omitempty"`
}

// watchClient owns the continuous presentation loop. Every iteration is a
// cheap daemon IPC/cache read; the daemon's reconciliation worker owns all
// remote scheduler polling. --json and --once intentionally make one request.
func (c *command) watchClient(args []string) int {
	once := containsFlag(args, "--once") || c.json
	requestArgs := append([]string(nil), args...)
	if !containsFlag(requestArgs, "--once") {
		requestArgs = append(requestArgs, "--once")
	}
	if c.json && !containsFlag(requestArgs, "--json") {
		requestArgs = append(requestArgs, "--json")
	}
	first := true
	for {
		response, err := c.callDaemon(requestArgs)
		if err != nil {
			c.writeError(err)
			return 1
		}
		if response.Stdout != "" {
			if !first && !c.json && isTerminalWriter(c.stdout) {
				_, _ = io.WriteString(c.stdout, "\033[H\033[2J")
			}
			_, _ = io.WriteString(c.stdout, response.Stdout)
		}
		if response.Stderr != "" {
			_, _ = io.WriteString(c.stderr, response.Stderr)
		}
		if response.ExitCode != 0 {
			return response.ExitCode
		}
		if once {
			return 0
		}
		first = false
		timer := time.NewTimer(watchRefreshInterval)
		select {
		case <-c.ctx.Done():
			timer.Stop()
			return 0
		case <-timer.C:
		}
	}
}

func (c *command) callDaemon(args []string) (daemon.Response, error) {
	runtime, err := daemon.DefaultPaths()
	if err != nil {
		return daemon.Response{}, fault.Wrap("DAEMON_UNAVAILABLE", "cannot determine daemon runtime paths", true, err)
	}
	response, err := daemon.Call(c.ctx, runtime, c.version, args)
	if err != nil {
		if fault.As(err).Code == "DAEMON_UNAVAILABLE" {
			return daemon.Response{}, fault.New("DAEMON_REQUIRED",
				"this command requires the JoyRun daemon; run `joyrun daemon start`", true)
		}
		return daemon.Response{}, err
	}
	return response, nil
}

func (c *command) watch(db *store.Store, args []string) error {
	flags := newFlags("watch", c.stderr)
	var once, attention bool
	var projectID, target, state string
	var limit int
	flags.BoolVar(&once, "once", false, "render one cache snapshot")
	flags.StringVar(&projectID, "project", "", "filter by Project ID")
	flags.StringVar(&target, "target", "", "filter by target")
	flags.StringVar(&state, "state", "", "filter by compute state")
	flags.BoolVar(&attention, "attention", false, "show only tasks needing attention")
	flags.IntVar(&limit, "limit", 100, "maximum visible tasks")
	if err := flags.Parse(interspersed(args,
		map[string]bool{"--project": true, "--target": true, "--state": true, "--limit": true},
		map[string]bool{"--once": true, "--attention": true})); err != nil {
		return fault.Wrap("INVALID_ARGUMENT", "invalid watch arguments", false, err)
	}
	if flags.NArg() != 0 {
		return fault.New("INVALID_ARGUMENT", "usage: joyrun watch [--once] [--project ID] [--target TARGET] [--state STATE] [--attention] [--limit N]", false)
	}
	if limit < 1 || limit > 1000 {
		return fault.New("INVALID_ARGUMENT", "watch --limit must be between 1 and 1000", false)
	}
	if !once {
		return fault.New("INVALID_ARGUMENT", "watch is controlled by the daemon client; use `joyrun watch` without --once", false)
	}
	rows, total, err := db.ListWatchTasks(c.ctx, limit, store.WatchFilter{
		ProjectID: projectID, Target: target, State: state, Attention: attention,
	})
	if err != nil {
		return err
	}
	output := watchOutput{
		Tasks: rows, Total: total, Hidden: total - len(rows), GeneratedAt: time.Now().UTC(),
		ProjectID: projectID, Target: target, State: state, Attention: attention,
	}
	if c.json {
		c.write(output, "")
		return nil
	}
	fmt.Fprint(c.stdout, formatWatch(output))
	return nil
}

func formatWatch(output watchOutput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "JoyRun watch  %s\n", output.GeneratedAt.Local().Format("2006-01-02 15:04:05"))
	if output.ProjectID != "" {
		fmt.Fprintf(&b, "Project: %s  ", output.ProjectID)
	}
	if output.Target != "" {
		fmt.Fprintf(&b, "Target: %s  ", output.Target)
	}
	if output.State != "" {
		fmt.Fprintf(&b, "State: %s  ", output.State)
	}
	if output.Attention {
		b.WriteString("Attention only  ")
	}
	fmt.Fprintf(&b, "Tasks: %d", output.Total)
	if output.Hidden > 0 {
		fmt.Fprintf(&b, " (%d hidden)", output.Hidden)
	}
	b.WriteString("\n\n")
	if len(output.Tasks) == 0 {
		b.WriteString("No tasks.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "%-22s %-12s %-24s %-10s %-10s %-8s\n", "TASK", "PROJECT", "SOURCE", "STATE", "PULL", "AGE")
	for _, task := range output.Tasks {
		state := task.ComputeState
		if isWatchAttention(task) {
			state = "!" + state
		}
		fmt.Fprintf(&b, "%-22s %-12s %-24s %-10s %-10s %-8s\n",
			shortID(task.ID, 21), shortID(task.ProjectID, 11), shortText(task.SourcePath, 23),
			shortText(state, 9), shortText(task.PullState, 9), watchAge(task.UpdatedAt))
	}
	return b.String()
}

func isWatchAttention(task model.TaskSummary) bool {
	return task.ComputeState == model.ComputeSubmissionFailed ||
		task.ComputeState == model.ComputeSubmissionUncertain ||
		task.ComputeState == model.ComputeFailed ||
		task.PullState == model.PullFailed || task.PullState == model.PullPartial
}

func watchAge(updated time.Time) string {
	if updated.IsZero() {
		return "-"
	}
	d := time.Since(updated)
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

func shortID(value string, width int) string {
	if len(value) <= width {
		return value
	}
	return value[:width-1] + "~"
}

func shortText(value string, width int) string {
	value = strings.ReplaceAll(value, "\n", " ")
	return shortID(value, width)
}

func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
