package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/wxia529/joyrun/internal/app"
	"github.com/wxia529/joyrun/internal/config"
	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/model"
	"github.com/wxia529/joyrun/internal/paths"
	"github.com/wxia529/joyrun/internal/project"
	"github.com/wxia529/joyrun/internal/remote"
	"github.com/wxia529/joyrun/internal/store"
	"github.com/wxia529/joyrun/internal/transfer"
)

type command struct {
	ctx      context.Context
	version  string
	stdout   io.Writer
	stderr   io.Writer
	json     bool
	config   string
	exitCode int
}

func Run(ctx context.Context, args []string, version string) int {
	c := &command{ctx: ctx, version: version, stdout: os.Stdout, stderr: os.Stderr, config: paths.ConfigFile()}
	args = c.extractGlobals(args)
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		c.usage()
		return 0
	}
	if args[0] == "version" || args[0] == "--version" {
		c.write(map[string]any{"version": version}, "joyrun "+version+"\n")
		return 0
	}
	if err := c.execute(args[0], args[1:]); err != nil {
		c.writeError(err)
		return 1
	}
	return c.exitCode
}

func (c *command) extractGlobals(args []string) []string {
	result := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			c.json = true
		case "--config":
			if i+1 < len(args) {
				i++
				c.config = args[i]
			} else {
				result = append(result, args[i])
			}
		default:
			if strings.HasPrefix(args[i], "--config=") {
				c.config = strings.TrimPrefix(args[i], "--config=")
			} else {
				result = append(result, args[i])
			}
		}
	}
	return result
}

func (c *command) execute(name string, args []string) error {
	db, err := store.Open(paths.DatabaseFile())
	if err != nil {
		return err
	}
	defer db.Close()
	if name == "init" {
		return c.init(db, args)
	}
	cfg, err := config.Load(c.config)
	if err != nil {
		return err
	}
	runner := remote.SSH{Stderr: c.stderr}
	application := &app.App{
		Config: cfg, Store: db, Runner: runner,
		Transfer: transfer.Manager{Stderr: c.stderr, Runner: runner},
	}
	switch name {
	case "target":
		return c.target(application, args)
	case "submit":
		return c.submit(application, args)
	case "status":
		return c.status(application, args)
	case "list":
		return c.list(application, args)
	case "inspect":
		return c.inspect(application, args)
	case "logs":
		return c.logs(application, args)
	case "cancel":
		return c.cancel(application, args)
	case "pull":
		return c.pull(application, args)
	case "doctor":
		return c.doctor(application, args)
	case "recover":
		return c.recover(application, args)
	default:
		return fault.New("UNKNOWN_COMMAND", fmt.Sprintf("unknown command %q", name), false)
	}
}

func (c *command) init(db *store.Store, args []string) error {
	flags := newFlags("init", c.stderr)
	if err := flags.Parse(interspersed(args, map[string]bool{}, map[string]bool{})); err != nil {
		return fault.Wrap("INVALID_ARGUMENT", "invalid init arguments", false, err)
	}
	root := "."
	if flags.NArg() > 1 {
		return fault.New("INVALID_ARGUMENT", "usage: joyrun init [directory]", false)
	}
	if flags.NArg() == 1 {
		root = flags.Arg(0)
	}
	p, err := project.Init(root)
	if err != nil {
		return err
	}
	if err := db.BindProject(c.ctx, p); err != nil {
		return err
	}
	c.write(p, fmt.Sprintf("Initialized JoyRun project %s in %s\n", p.ProjectID, p.Root))
	return nil
}

func (c *command) target(application *app.App, args []string) error {
	if len(args) == 1 && args[0] == "list" {
		names := config.TargetNames(application.Config)
		if c.json {
			c.write(map[string]any{"targets": names}, "")
			return nil
		}
		for _, name := range names {
			fmt.Fprintf(c.stdout, "%-28s %s\n", name, application.Config.Targets[name].Cluster)
		}
		return nil
	}
	if len(args) != 2 || args[0] != "show" {
		return fault.New("INVALID_ARGUMENT", "usage: joyrun target <list|show TARGET>", false)
	}
	target, ok := application.Config.Targets[args[1]]
	if !ok {
		return fault.New("TARGET_NOT_FOUND", fmt.Sprintf("target %q not found", args[1]), false)
	}
	c.write(map[string]any{"name": args[1], "target": target},
		fmt.Sprintf("Target: %s\nCluster: %s\nSource: %s\nPull: %s\n\nParameters:\n%s",
			args[1], target.Cluster, target.Source.Kind,
			strings.Join(target.Pull.Default, ", "), formatParams(target)))
	return nil
}

func (c *command) submit(application *app.App, args []string) error {
	flags := newFlags("submit", c.stderr)
	var target string
	var sets stringList
	var dryRun bool
	flags.StringVar(&target, "target", "", "execution target")
	flags.StringVar(&target, "t", "", "execution target")
	flags.Var(&sets, "set", "target parameter key=value")
	flags.BoolVar(&dryRun, "dry-run", false, "preview without remote changes")
	if err := flags.Parse(interspersed(args,
		map[string]bool{"--target": true, "-t": true, "--set": true},
		map[string]bool{"--dry-run": true})); err != nil {
		return fault.Wrap("INVALID_ARGUMENT", "invalid submit arguments", false, err)
	}
	if flags.NArg() != 1 || target == "" {
		return fault.New("INVALID_ARGUMENT", "usage: joyrun submit <source> -t <target> [--set key=value] [--dry-run]", false)
	}
	cwd, _ := os.Getwd()
	if dryRun {
		preview, _, _, err := application.Preview(c.ctx, cwd, flags.Arg(0), target, sets)
		if err != nil {
			return err
		}
		c.write(preview, formatPreview(preview))
		return nil
	}
	result, err := application.Submit(c.ctx, cwd, flags.Arg(0), target, sets)
	if err != nil {
		return err
	}
	c.write(result, formatTask(result.Task))
	return nil
}

func (c *command) status(application *app.App, args []string) error {
	flags := newFlags("status", c.stderr)
	var all bool
	flags.BoolVar(&all, "all", false, "refresh all non-terminal tasks")
	if err := flags.Parse(interspersed(args, map[string]bool{}, map[string]bool{"--all": true})); err != nil {
		return fault.Wrap("INVALID_ARGUMENT", "invalid status arguments", false, err)
	}
	cwd, _ := os.Getwd()
	if all {
		if flags.NArg() != 0 {
			return fault.New("INVALID_ARGUMENT", "usage: joyrun status --all", false)
		}
		result := application.StatusAll(c.ctx, cwd)
		if c.json {
			failures := result.Failures
			if failures == nil {
				failures = []app.StatusFailure{}
			}
			c.write(map[string]any{
				"tasks":    summarizeTasks(result.Tasks),
				"failures": failures,
			}, "")
		} else {
			for _, task := range result.Tasks {
				fmt.Fprint(c.stdout, formatTask(task))
			}
			for _, failure := range result.Failures {
				fmt.Fprintf(c.stderr, "Error [%s] %s: %s\n",
					failure.Error.Code, failure.TaskID, failure.Error.Error())
			}
		}
		if len(result.Failures) > 0 {
			c.exitCode = 1
		}
		return nil
	}
	if flags.NArg() != 1 {
		return fault.New("INVALID_ARGUMENT", "usage: joyrun status <source|task-id> | joyrun status --all", false)
	}
	task, err := application.Status(c.ctx, cwd, flags.Arg(0))
	if err != nil {
		return err
	}
	c.write(model.SummarizeTask(task), formatTask(task))
	return nil
}

func (c *command) list(application *app.App, args []string) error {
	if len(args) > 1 {
		return fault.New("INVALID_ARGUMENT", "usage: joyrun list [source]", false)
	}
	cwd, _ := os.Getwd()
	var tasks []model.Task
	var err error
	if len(args) == 1 {
		tasks, err = application.History(c.ctx, cwd, args[0])
	} else {
		tasks, err = application.List(c.ctx, cwd)
	}
	if err != nil {
		return err
	}
	if c.json {
		c.write(map[string]any{"tasks": summarizeTasks(tasks)}, "")
		return nil
	}
	for _, task := range tasks {
		fmt.Fprint(c.stdout, formatTask(task))
	}
	return nil
}

func (c *command) inspect(application *app.App, args []string) error {
	flags := newFlags("inspect", c.stderr)
	var events bool
	flags.BoolVar(&events, "events", false, "include append-only task events")
	if err := flags.Parse(interspersed(args, map[string]bool{}, map[string]bool{"--events": true})); err != nil {
		return fault.Wrap("INVALID_ARGUMENT", "invalid inspect arguments", false, err)
	}
	if flags.NArg() != 1 {
		return fault.New("INVALID_ARGUMENT", "usage: joyrun inspect <source|task-id> [--events]", false)
	}
	cwd, _ := os.Getwd()
	if events {
		result, err := application.Trace(c.ctx, cwd, flags.Arg(0))
		if err != nil {
			return err
		}
		if c.json {
			c.write(result, "")
			return nil
		}
		fmt.Fprint(c.stdout, formatInspect(result.Task))
		fmt.Fprintln(c.stdout, "\nEvents:")
		for _, event := range result.Events {
			fmt.Fprintf(c.stdout, "%s  %-24s %-10s %s\n",
				event.CreatedAt.Local().Format("2006-01-02 15:04:05"),
				event.Type, event.Stage, event.Message)
		}
		return nil
	}
	task, err := application.Inspect(c.ctx, cwd, flags.Arg(0))
	if err != nil {
		return err
	}
	c.write(task, formatInspect(task))
	return nil
}

func (c *command) logs(application *app.App, args []string) error {
	flags := newFlags("logs", c.stderr)
	lines := 100
	flags.IntVar(&lines, "lines", 100, "number of lines")
	if err := flags.Parse(interspersed(args,
		map[string]bool{"--lines": true}, map[string]bool{})); err != nil {
		return fault.Wrap("INVALID_ARGUMENT", "invalid logs arguments", false, err)
	}
	if flags.NArg() != 1 || lines < 1 {
		return fault.New("INVALID_ARGUMENT", "usage: joyrun logs <source|task-id> [--lines N]", false)
	}
	cwd, _ := os.Getwd()
	result, err := application.Logs(c.ctx, cwd, flags.Arg(0), lines)
	if err != nil {
		return err
	}
	c.write(result, result.Content)
	return nil
}

func (c *command) cancel(application *app.App, args []string) error {
	if len(args) != 1 {
		return fault.New("INVALID_ARGUMENT", "usage: joyrun cancel <task-id>", false)
	}
	cwd, _ := os.Getwd()
	task, err := application.Cancel(c.ctx, cwd, args[0])
	if err != nil {
		return err
	}
	c.write(task, formatTask(task))
	return nil
}

func (c *command) pull(application *app.App, args []string) error {
	flags := newFlags("pull", c.stderr)
	var options app.PullOptions
	var includes stringList
	flags.BoolVar(&options.All, "all", false, "pull all generated files")
	flags.BoolVar(&options.OverwriteInputs, "overwrite-inputs", false, "allow submitted inputs to be overwritten")
	flags.BoolVar(&options.Live, "live", false, "pull files while task is not complete")
	flags.Var(&includes, "include", "include glob (repeatable)")
	if err := flags.Parse(interspersed(args,
		map[string]bool{"--include": true},
		map[string]bool{"--all": true, "--overwrite-inputs": true, "--live": true})); err != nil {
		return fault.Wrap("INVALID_ARGUMENT", "invalid pull arguments", false, err)
	}
	if flags.NArg() != 1 {
		return fault.New("INVALID_ARGUMENT", "usage: joyrun pull <source|task-id> [--all|--include glob] [--live]", false)
	}
	if options.All && len(includes) > 0 {
		return fault.New("INVALID_ARGUMENT", "--all and --include are mutually exclusive", false)
	}
	options.Include = includes
	cwd, _ := os.Getwd()
	result, err := application.Pull(c.ctx, cwd, flags.Arg(0), options)
	if err != nil {
		return err
	}
	c.write(result, fmt.Sprintf("Pulled %d file(s) to %s\n", len(result.Files), result.Destination))
	return nil
}

func (c *command) doctor(application *app.App, args []string) error {
	if len(args) != 1 {
		return fault.New("INVALID_ARGUMENT", "usage: joyrun doctor <target>", false)
	}
	result := application.Doctor(c.ctx, args[0])
	if c.json {
		c.write(result, "")
	} else {
		for _, check := range result.Checks {
			fmt.Fprintf(c.stdout, "%-5s %-16s %s\n",
				strings.ToUpper(check.Status), check.Name, check.Message)
			if check.SuggestedAction != "" {
				fmt.Fprintf(c.stdout, "      %-16s %s\n", "Suggested action:", check.SuggestedAction)
			}
		}
	}
	if !result.Ready {
		// The report has already been emitted, including in JSON mode. Use a
		// process exit status instead of returning an error and emitting twice.
		c.exitCode = 1
	}
	return nil
}

func (c *command) recover(application *app.App, args []string) error {
	flags := newFlags("recover", c.stderr)
	var target string
	flags.StringVar(&target, "target", "", "execution target")
	flags.StringVar(&target, "t", "", "execution target")
	if err := flags.Parse(interspersed(args,
		map[string]bool{"--target": true, "-t": true}, map[string]bool{})); err != nil {
		return fault.Wrap("INVALID_ARGUMENT", "invalid recover arguments", false, err)
	}
	if flags.NArg() != 1 || target == "" {
		return fault.New("INVALID_ARGUMENT", "usage: joyrun recover <task-id> -t <target>", false)
	}
	cwd, _ := os.Getwd()
	task, err := application.Recover(c.ctx, cwd, flags.Arg(0), target)
	if err != nil {
		return err
	}
	c.write(task, "Recovered "+formatTask(task))
	return nil
}

func (c *command) write(value any, human string) {
	if c.json {
		encoder := json.NewEncoder(c.stdout)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(map[string]any{"ok": true, "result": value})
		return
	}
	fmt.Fprint(c.stdout, human)
}

func (c *command) writeError(err error) {
	typed := fault.As(err)
	if c.json {
		_ = json.NewEncoder(c.stdout).Encode(map[string]any{"ok": false, "error": typed})
		return
	}
	fmt.Fprintf(c.stderr, "Error [%s]: %s\n", typed.Code, typed.Error())
}

func (c *command) usage() {
	fmt.Fprint(c.stdout, `JoyRun — a local-first remote task runner for HPC

Usage:
  joyrun init [directory]
  joyrun submit <source> -t <target> [--set key=value] [--dry-run]
  joyrun status <source|task-id>
  joyrun status --all
  joyrun list [source]
  joyrun inspect <source|task-id>
  joyrun inspect <source|task-id> --events
  joyrun logs <source|task-id> [--lines N]
  joyrun pull <source|task-id> [--all|--include glob] [--live]
  joyrun cancel <task-id>
  joyrun target list
  joyrun target show <target>
  joyrun doctor <target>
  joyrun recover <task-id> -t <target>

Global options:
  --json           emit machine-readable JSON only on stdout
  --config PATH    use a specific config file
`)
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	return flags
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func formatTask(task model.Task) string {
	scheduler := ""
	if task.SchedulerID != "" {
		scheduler = " slurm:" + task.SchedulerID
	}
	return fmt.Sprintf("%s  compute:%-17s pull:%-15s %s  %s%s\n",
		task.ID, strings.ToUpper(task.ComputeState), strings.ToUpper(task.PullState),
		task.TargetName, task.SourcePath, scheduler)
}

func formatInspect(task model.Task) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Task:          %s\nProject:       %s\nSource:        %s\nTarget:        %s\nCluster:       %s\n",
		task.ID, task.ProjectID, task.SourcePath, task.TargetName, task.ClusterName)
	fmt.Fprintf(&builder, "Compute state: %s\nPull state:    %s\nScheduler:     %s\nRemote:        %s\n",
		task.ComputeState, task.PullState, task.SchedulerID, task.RemoteDir)
	fmt.Fprintf(&builder, "Created:       %s\nUpdated:       %s\n\nRendered script:\n%s",
		task.CreatedAt.Local().Format(time.RFC3339), task.UpdatedAt.Local().Format(time.RFC3339),
		task.RenderedScript)
	if !strings.HasSuffix(task.RenderedScript, "\n") {
		builder.WriteByte('\n')
	}
	return builder.String()
}

func summarizeTasks(tasks []model.Task) []model.TaskSummary {
	result := make([]model.TaskSummary, 0, len(tasks))
	for _, task := range tasks {
		result = append(result, model.SummarizeTask(task))
	}
	return result
}

func formatParams(target model.Target) string {
	var builder strings.Builder
	names := make([]string, 0, len(target.Params))
	for name := range target.Params {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		spec := target.Params[name]
		fmt.Fprintf(&builder, "%-20s %-8s default=%v", name, spec.Type, spec.Default)
		if spec.Required {
			builder.WriteString(" required")
		}
		if spec.Description != "" {
			builder.WriteString("  " + spec.Description)
		}
		builder.WriteByte('\n')
	}
	return builder.String()
}

func formatPreview(preview app.Preview) string {
	var builder strings.Builder
	entry := "<none>"
	if preview.Source.Entry != nil {
		entry = *preview.Source.Entry
	}
	fmt.Fprintf(&builder, "Task: %s\nTarget: %s\nCluster: %s\nRemote: %s\nScheduler log: %s\n\nSource:\n  Path:     %s\n  Kind:     %s\n  WorkDir:  %s\n  Entry:    %s\n\nTemplate values:\n  Input:    %s\n  Stem:     %s\n  Name:     %s\n  WorkDir:  %s\n\nParameters:\n",
		preview.TaskID, preview.Target, preview.Cluster, preview.RemoteDir,
		preview.SchedulerLog,
		preview.Source.RelativePath, preview.Source.Kind, preview.Source.WorkDir, entry,
		displayEmpty(preview.TemplateValues.Input), displayEmpty(preview.TemplateValues.Stem),
		displayEmpty(preview.TemplateValues.Name), displayEmpty(preview.TemplateValues.WorkDir))
	names := make([]string, 0, len(preview.Params))
	for name := range preview.Params {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		fmt.Fprintf(&builder, "  %-20s %v (%s)\n", name, preview.Params[name], preview.ParamSources[name])
	}
	builder.WriteString("\nUpload snapshot:\n")
	for _, file := range preview.Files {
		builder.WriteString("  " + file + "\n")
	}
	builder.WriteString("\nRendered script:\n")
	builder.WriteString(preview.RenderedScript)
	if !strings.HasSuffix(preview.RenderedScript, "\n") {
		builder.WriteByte('\n')
	}
	return builder.String()
}

func displayEmpty(value string) string {
	if value == "" {
		return "<empty>"
	}
	return value
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func interspersed(args []string, valueFlags, boolFlags map[string]bool) []string {
	var options, positional []string
	for index := 0; index < len(args); index++ {
		value := args[index]
		name := value
		if before, _, ok := strings.Cut(value, "="); ok {
			name = before
		}
		switch {
		case boolFlags[name]:
			options = append(options, value)
		case valueFlags[name]:
			options = append(options, value)
			if !strings.Contains(value, "=") && index+1 < len(args) {
				index++
				options = append(options, args[index])
			}
		case strings.HasPrefix(value, "-"):
			options = append(options, value)
		default:
			positional = append(positional, value)
		}
	}
	return append(options, positional...)
}
