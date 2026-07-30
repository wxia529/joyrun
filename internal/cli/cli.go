package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
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

type submitOutput struct {
	BatchID  string             `json:"batch_id,omitempty"`
	Tasks    []model.Task       `json:"tasks"`
	Failures []app.BatchFailure `json:"failures"`
}

type submitPreviewOutput struct {
	Sources  []string      `json:"sources"`
	Previews []app.Preview `json:"previews"`
	Failures []any         `json:"failures"`
}

type pullOutput struct {
	PullID        string             `json:"pull_id,omitempty"`
	SourceBatchID string             `json:"source_batch_id,omitempty"`
	Tasks         []app.PullManyItem `json:"tasks"`
	Failures      []app.BatchFailure `json:"failures"`
	DryRun        bool               `json:"dry_run,omitempty"`
}

func Run(ctx context.Context, args []string, version string) int {
	c := &command{ctx: ctx, version: version, stdout: os.Stdout, stderr: os.Stderr, config: paths.ConfigFile()}
	args = c.extractGlobals(args)
	if len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h")) {
		c.usage()
		return 0
	}
	if args[0] == "help" {
		if len(args) != 2 {
			c.writeError(fault.New("INVALID_ARGUMENT", "usage: joyrun help <command>", false))
			return 1
		}
		if !c.commandUsage(args[1]) {
			c.writeError(fault.New("UNKNOWN_COMMAND", fmt.Sprintf("unknown command %q", args[1]), false))
			return 1
		}
		return 0
	}
	if len(args) >= 2 && (containsFlag(args[1:], "--help") || containsFlag(args[1:], "-h")) {
		if !c.commandUsage(args[0]) {
			c.writeError(fault.New("UNKNOWN_COMMAND", fmt.Sprintf("unknown command %q", args[0]), false))
			return 1
		}
		return 0
	}
	if args[0] == "version" || args[0] == "--version" {
		c.write(map[string]any{"version": version}, "joyrun "+version+"\n")
		return 0
	}
	if !knownCommand(args[0]) {
		c.writeError(fault.New("UNKNOWN_COMMAND", fmt.Sprintf("unknown command %q", args[0]), false))
		return 1
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
	if name == "config" {
		return c.configure(args)
	}
	if name == "init" {
		db, err := store.Open(paths.DatabaseFile())
		if err != nil {
			return err
		}
		defer db.Close()
		return c.init(db, args)
	}
	cfg, err := config.Load(c.config)
	if err != nil {
		return err
	}
	runner := remote.SSH{Stderr: c.stderr}
	application := &app.App{
		Config: cfg, Runner: runner,
		Transfer: transfer.Manager{Stderr: c.stderr, Runner: runner},
		Progress: c.stderr,
	}
	if name == "target" {
		return c.target(application, args)
	}
	if name == "doctor" {
		return c.doctor(application, args)
	}
	if name == "submit" && containsFlag(args, "--dry-run") {
		return c.submit(application, args)
	}
	if name == "recover" && containsFlag(args, "--scan") {
		return c.recover(application, args)
	}
	db, err := store.Open(paths.DatabaseFile())
	if err != nil {
		return err
	}
	defer db.Close()
	application.Store = db
	switch name {
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
	case "files":
		return c.files(application, args)
	case "cancel":
		return c.cancel(application, args)
	case "pull":
		return c.pull(application, args)
	case "recover":
		return c.recover(application, args)
	default:
		return fault.New("UNKNOWN_COMMAND", fmt.Sprintf("unknown command %q", name), false)
	}
}

func (c *command) configure(args []string) error {
	if len(args) != 1 {
		return fault.New("INVALID_ARGUMENT", "usage: joyrun config <path|init|validate>", false)
	}
	switch args[0] {
	case "path":
		c.write(map[string]string{"path": c.config}, c.config+"\n")
		return nil
	case "init":
		if err := config.Init(c.config); err != nil {
			return err
		}
		c.write(map[string]string{"path": c.config},
			"Created JoyRun configuration at "+c.config+"\n")
		return nil
	case "validate":
		cfg, err := config.Load(c.config)
		if err != nil {
			return err
		}
		result := map[string]any{
			"path": c.config, "clusters": len(cfg.Clusters), "targets": len(cfg.Targets),
		}
		c.write(result, fmt.Sprintf("Valid JoyRun configuration: %s (%d cluster(s), %d target(s))\n",
			c.config, len(cfg.Clusters), len(cfg.Targets)))
		return nil
	default:
		return fault.New("INVALID_ARGUMENT", "usage: joyrun config <path|init|validate>", false)
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
		if len(names) == 0 {
			fmt.Fprintln(c.stdout, "No targets configured.")
			return nil
		}
		fmt.Fprintf(c.stdout, "%-28s %s\n", "TARGET", "CLUSTER")
		for _, name := range names {
			fmt.Fprintf(c.stdout, "%-28s %s\n", name, application.Config.Targets[name].Cluster)
		}
		return nil
	}
	if len(args) >= 1 && args[0] == "nodes" {
		flags := newFlags("target nodes", c.stderr)
		var partition string
		flags.StringVar(&partition, "partition", "", "allowed target partition")
		if err := flags.Parse(interspersed(
			args[1:], map[string]bool{"--partition": true}, map[string]bool{},
		)); err != nil {
			return fault.Wrap("INVALID_ARGUMENT", "invalid target nodes arguments", false, err)
		}
		if flags.NArg() != 1 {
			return fault.New("INVALID_ARGUMENT",
				"usage: joyrun target nodes <target> [--partition name]", false)
		}
		result, err := application.TargetNodes(c.ctx, flags.Arg(0), partition)
		if err != nil {
			return err
		}
		c.write(result, formatTargetNodes(result))
		return nil
	}
	if len(args) != 2 || (args[0] != "show" && args[0] != "params") {
		return fault.New("INVALID_ARGUMENT",
			"usage: joyrun target <list|show TARGET|params TARGET|nodes TARGET [--partition name]>", false)
	}
	target, ok := application.Config.Targets[args[1]]
	if !ok {
		return fault.New("TARGET_NOT_FOUND", fmt.Sprintf("target %q not found", args[1]), false)
	}
	if args[0] == "params" {
		c.write(map[string]any{"name": args[1], "params": target.Params}, formatParams(target))
		return nil
	}
	c.write(map[string]any{"name": args[1], "target": target},
		fmt.Sprintf("Target: %s\nCluster: %s\nSoftware: %s %s\nSource: %s\nDefault partition: %s\nAllowed partitions: %s\nPush mode: %s\nPush include: %s\nPull: %s\n\nParameters:\n%s",
			args[1], target.Cluster, target.Software.Name, target.Software.Version, target.Source.Kind,
			target.Placement.DefaultPartition, displayList(target.Placement.AllowedPartitions),
			target.Push.Mode, displayList(target.Push.Include),
			strings.Join(target.Pull.Default, ", "), formatParams(target)))
	return nil
}

func (c *command) submit(application *app.App, args []string) error {
	flags := newFlags("submit", c.stderr)
	var target, partition string
	var sets, includes, globs, files stringList
	var dryRun, allowProjectRoot bool
	flags.StringVar(&target, "target", "", "execution target")
	flags.StringVar(&target, "t", "", "execution target")
	flags.Var(&sets, "set", "target parameter key=value")
	flags.Var(&includes, "include", "additional input dependency glob (repeatable; entry-mode targets only)")
	flags.Var(&globs, "glob", "source glob expanded by JoyRun")
	flags.Var(&files, "from", "file containing one source per line")
	flags.StringVar(&partition, "partition", "", "allowed target partition")
	flags.BoolVar(&dryRun, "dry-run", false, "preview without remote changes")
	flags.BoolVar(&allowProjectRoot, "allow-project-root", false, "explicitly allow uploading from the project root")
	if err := flags.Parse(interspersed(args,
		map[string]bool{
			"--target": true, "-t": true, "--set": true, "--include": true,
			"--glob": true, "--from": true, "--partition": true,
		},
		map[string]bool{"--dry-run": true, "--allow-project-root": true})); err != nil {
		return fault.Wrap("INVALID_ARGUMENT", "invalid submit arguments", false, err)
	}
	if target == "" {
		return fault.New("INVALID_ARGUMENT",
			"usage: joyrun submit SOURCE... -t TARGET [--glob PATTERN] [--from FILE]", false)
	}
	sources, err := collectBatchValues(flags.Args(), globs, files)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return fault.New("INVALID_ARGUMENT", "submit requires at least one source", false)
	}
	if len(sources) > app.MaxBatchTasks {
		return fault.New("BATCH_TOO_LARGE",
			fmt.Sprintf("submit accepts at most %d sources", app.MaxBatchTasks), false)
	}
	cwd, _ := os.Getwd()
	if dryRun {
		previews := make([]app.Preview, 0, len(sources))
		for _, source := range sources {
			preview, _, _, err := application.Preview(
				c.ctx, cwd, source, target, sets, includes, partition, allowProjectRoot)
			if err != nil {
				return err
			}
			previews = append(previews, preview)
		}
		if c.json {
			c.write(submitPreviewOutput{
				Sources: sources, Previews: previews, Failures: []any{},
			}, "")
			return nil
		}
		if len(previews) == 1 {
			fmt.Fprint(c.stdout, formatPreview(previews[0]))
			return nil
		}
		fmt.Fprintf(c.stdout, "Batch preview: %d task(s)\n", len(previews))
		for _, preview := range previews {
			fmt.Fprintf(c.stdout, "  %s  %s  %d file(s)\n",
				preview.TaskID, preview.Source.RelativePath, len(preview.InputManifest))
		}
		return nil
	}
	if len(sources) == 1 {
		fmt.Fprintln(c.stderr, "Preparing immutable input snapshot and uploading task...")
		result, err := application.Submit(
			c.ctx, cwd, sources[0], target, sets, includes, partition, allowProjectRoot)
		if err != nil {
			return err
		}
		output := submitOutput{Tasks: []model.Task{result.Task}, Failures: []app.BatchFailure{}}
		c.write(output, "Submitted "+formatTask(result.Task))
		return nil
	}
	fmt.Fprintf(c.stderr, "Preparing and submitting %d independent task(s)...\n", len(sources))
	result, err := application.SubmitMany(
		c.ctx, cwd, sources, target, sets, includes, partition, allowProjectRoot)
	if err != nil {
		return err
	}
	output := submitOutput{
		BatchID: result.BatchID, Tasks: result.Tasks, Failures: result.Failures,
	}
	if c.json {
		c.write(output, "")
	} else {
		fmt.Fprintf(c.stdout, "Batch %s\n", result.BatchID)
		for _, task := range result.Tasks {
			fmt.Fprint(c.stdout, formatTask(task))
		}
		for _, failure := range result.Failures {
			fmt.Fprintf(c.stderr, "%s (%s): %s\n",
				failure.TaskID, failure.Source, failure.Error.Error())
		}
	}
	if len(result.Failures) > 0 {
		c.exitCode = 1
	}
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
			if len(result.Tasks) == 0 && len(result.Failures) == 0 {
				fmt.Fprintln(c.stdout, "No tasks.")
			} else if len(result.Tasks) > 0 {
				fmt.Fprint(c.stdout, taskHeader())
			}
			for _, task := range result.Tasks {
				fmt.Fprint(c.stdout, formatTask(task))
			}
			for _, failure := range result.Failures {
				fmt.Fprintf(c.stderr, "Task %s:\n", failure.TaskID)
				c.writeError(failure.Error)
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
	if len(tasks) == 0 {
		fmt.Fprintln(c.stdout, "No tasks.")
		return nil
	}
	fmt.Fprint(c.stdout, taskHeader())
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
	var file string
	flags.IntVar(&lines, "lines", 100, "number of lines")
	flags.StringVar(&file, "file", "", "specific remote log path")
	if err := flags.Parse(interspersed(args,
		map[string]bool{"--lines": true, "--file": true}, map[string]bool{})); err != nil {
		return fault.Wrap("INVALID_ARGUMENT", "invalid logs arguments", false, err)
	}
	if flags.NArg() != 1 || lines < 1 {
		return fault.New("INVALID_ARGUMENT",
			"usage: joyrun logs <source|task-id> [--lines N] [--file PATH]", false)
	}
	cwd, _ := os.Getwd()
	result, err := application.Logs(c.ctx, cwd, flags.Arg(0), lines, file)
	if err != nil {
		return err
	}
	c.write(result, result.Content)
	return nil
}

func (c *command) files(application *app.App, args []string) error {
	if len(args) != 1 {
		return fault.New("INVALID_ARGUMENT", "usage: joyrun files <source|task-id>", false)
	}
	cwd, _ := os.Getwd()
	files, err := application.RemoteFiles(c.ctx, cwd, args[0])
	if err != nil {
		return err
	}
	if c.json {
		c.write(map[string]any{"files": files}, "")
		return nil
	}
	if len(files) == 0 {
		fmt.Fprintln(c.stdout, "No remote files.")
		return nil
	}
	var total int64
	fmt.Fprintf(c.stdout, "%10s  %-6s  %s\n", "SIZE", "INPUT", "PATH")
	for _, file := range files {
		total += file.Size
		input := ""
		if file.Input {
			input = "yes"
		}
		fmt.Fprintf(c.stdout, "%10s  %-6s  %s\n", humanBytes(file.Size), input, file.Path)
	}
	fmt.Fprintf(c.stdout, "\n%d file(s), %s total\n", len(files), humanBytes(total))
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
	var options app.PullManyOptions
	var batchID string
	var includes, globs, files stringList
	flags.BoolVar(&options.All, "all", false, "pull all generated files")
	flags.BoolVar(&options.OverwriteInputs, "overwrite-inputs", false, "allow submitted inputs to be overwritten")
	flags.BoolVar(&options.Live, "live", false, "pull files while tasks are not complete")
	flags.BoolVar(&options.DryRun, "dry-run", false, "preview selected files")
	flags.StringVar(&batchID, "batch", "", "select tasks from a submit batch ID")
	flags.Var(&includes, "include", "include glob (repeatable)")
	flags.Var(&globs, "glob", "source glob expanded by JoyRun")
	flags.Var(&files, "from", "file containing one task ID or source per line")
	if err := flags.Parse(interspersed(args,
		map[string]bool{
			"--include": true, "--glob": true, "--from": true, "--batch": true,
		},
		map[string]bool{
			"--all": true, "--overwrite-inputs": true, "--live": true,
			"--dry-run": true,
		})); err != nil {
		return fault.Wrap("INVALID_ARGUMENT", "invalid pull arguments", false, err)
	}
	if options.All && len(includes) > 0 {
		return fault.New("INVALID_ARGUMENT", "--all and --include are mutually exclusive", false)
	}
	options.Include = includes
	options.BatchID = batchID
	identifiers, err := collectBatchValues(flags.Args(), globs, files)
	if err != nil {
		return err
	}
	selectors := 0
	if options.BatchID != "" {
		selectors++
	}
	if len(identifiers) > 0 {
		selectors++
	}
	if selectors > 1 {
		return fault.New("INVALID_ARGUMENT",
			"--batch and explicit selectors are mutually exclusive", false)
	}
	if selectors == 0 {
		return fault.New("INVALID_ARGUMENT",
			"usage: joyrun pull TASK_OR_SOURCE... | --batch BATCH_ID", false)
	}
	cwd, _ := os.Getwd()
	if len(identifiers) == 1 && options.BatchID == "" {
		if options.DryRun {
			fmt.Fprintln(c.stderr, "Selecting remote files without downloading...")
		} else {
			fmt.Fprintln(c.stderr, "Selecting and pulling remote files...")
		}
		result, err := application.Pull(c.ctx, cwd, identifiers[0], options.PullOptions)
		if err != nil {
			return err
		}
		output := pullOutput{
			Tasks: []app.PullManyItem{{
				TaskID: result.Task.ID, Source: result.Task.SourcePath,
				Files: result.Files, Destination: result.Destination,
				TotalBytes: result.TotalBytes, PullState: result.Task.PullState,
			}},
			Failures: []app.BatchFailure{}, DryRun: result.DryRun,
		}
		if c.json {
			c.write(output, "")
			return nil
		}
		if result.DryRun {
			fmt.Fprintf(c.stdout, "Would pull %d file(s), %s, to %s:\n",
				len(result.Files), humanBytes(result.TotalBytes), result.Destination)
			for _, file := range result.Files {
				fmt.Fprintln(c.stdout, "  "+file)
			}
			return nil
		}
		fmt.Fprintf(c.stdout, "Pulled %d file(s) to %s\n",
			len(result.Files), result.Destination)
		return nil
	}
	result, err := application.PullMany(c.ctx, cwd, identifiers, options)
	if err != nil {
		return err
	}
	output := pullOutput{
		PullID: result.PullID, SourceBatchID: result.SourceBatchID,
		Tasks: result.Tasks, Failures: result.Failures, DryRun: result.DryRun,
	}
	if c.json {
		c.write(output, "")
	} else {
		verb := "Pulled"
		if result.DryRun {
			verb = "Would pull"
		}
		fmt.Fprintf(c.stdout, "%s operation %s:\n", verb, result.PullID)
		for _, item := range result.Tasks {
			fmt.Fprintf(c.stdout, "  %s  %d file(s), %s -> %s\n",
				item.TaskID, len(item.Files), humanBytes(item.TotalBytes), item.Destination)
		}
		for _, failure := range result.Failures {
			fmt.Fprintf(c.stderr, "%s (%s): %s\n",
				failure.TaskID, failure.Source, failure.Error.Error())
		}
	}
	if len(result.Failures) > 0 {
		c.exitCode = 1
	}
	return nil
}

func collectBatchValues(positional, globs, files []string) ([]string, error) {
	values := append([]string{}, positional...)
	for _, pattern := range globs {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fault.Wrap("INVALID_ARGUMENT",
				fmt.Sprintf("invalid batch glob %q", pattern), false, err)
		}
		if len(matches) == 0 {
			return nil, fault.New("NO_SOURCES_MATCHED",
				fmt.Sprintf("batch glob %q matched no paths", pattern), false)
		}
		values = append(values, matches...)
	}
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			return nil, fault.Wrap("INVALID_ARGUMENT",
				"cannot read batch selection file "+name, false, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			values = append(values, line)
		}
	}
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		value = filepath.Clean(value)
		absolute, err := filepath.Abs(value)
		if err != nil {
			return nil, fault.Wrap("INVALID_ARGUMENT", "cannot resolve batch selector "+value, false, err)
		}
		key := filepath.Clean(absolute)
		if !seen[key] {
			seen[key] = true
			result = append(result, value)
		}
	}
	return result, nil
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
	var scan bool
	flags.StringVar(&target, "target", "", "execution target")
	flags.StringVar(&target, "t", "", "execution target")
	flags.BoolVar(&scan, "scan", false, "list recoverable tasks for the current project")
	if err := flags.Parse(interspersed(args,
		map[string]bool{"--target": true, "-t": true}, map[string]bool{"--scan": true})); err != nil {
		return fault.Wrap("INVALID_ARGUMENT", "invalid recover arguments", false, err)
	}
	cwd, _ := os.Getwd()
	if scan {
		if flags.NArg() != 0 || target == "" {
			return fault.New("INVALID_ARGUMENT", "usage: joyrun recover --scan -t <target>", false)
		}
		candidates, err := application.RecoveryCandidates(c.ctx, cwd, target)
		if err != nil {
			return err
		}
		if c.json {
			c.write(map[string]any{"candidates": candidates}, "")
			return nil
		}
		if len(candidates) == 0 {
			fmt.Fprintln(c.stdout, "No recoverable tasks found for this project and target.")
			return nil
		}
		fmt.Fprintf(c.stdout, "%-29s %-18s %-20s %s\n", "TASK", "COMPUTE", "UPDATED", "SOURCE")
		for _, candidate := range candidates {
			fmt.Fprintf(c.stdout, "%-29s %-18s %-20s %s\n",
				candidate.TaskID, candidate.ComputeState,
				candidate.UpdatedAt.Local().Format("2006-01-02 15:04:05"),
				candidate.SourcePath)
		}
		return nil
	}
	if flags.NArg() != 1 || target == "" {
		return fault.New("INVALID_ARGUMENT",
			"usage: joyrun recover <task-id> -t <target> | joyrun recover --scan -t <target>", false)
	}
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
	if typed.Stage != "" {
		fmt.Fprintf(c.stderr, "Stage: %s\n", typed.Stage)
	}
	if typed.ComputeState != "" || typed.PullState != "" {
		fmt.Fprintf(c.stderr, "State: compute=%s pull=%s\n",
			displayEmpty(typed.ComputeState), displayEmpty(typed.PullState))
	}
	if typed.Retryable {
		fmt.Fprintln(c.stderr, "Retryable: yes")
	}
	if typed.SuggestedAction != "" {
		fmt.Fprintf(c.stderr, "Next: %s\n", typed.SuggestedAction)
	}
}

func (c *command) usage() {
	fmt.Fprint(c.stdout, `JoyRun — a local-first remote task runner for HPC

Usage:
  joyrun config <path|init|validate>
  joyrun init [directory]
  joyrun submit <source>... -t <target> [--glob pattern] [--from file] [--partition name] [--set key=value] [--include glob] [--dry-run]
  joyrun status <source|task-id>
  joyrun status --all
  joyrun list [source]
  joyrun inspect <source|task-id>
  joyrun inspect <source|task-id> --events
  joyrun logs <source|task-id> [--lines N] [--file PATH]
  joyrun files <source|task-id>
  joyrun pull <source|task-id>... [--glob pattern] [--from file] [--batch id] [--all|--include glob] [--live] [--dry-run]
  joyrun cancel <task-id>
  joyrun target list
  joyrun target show <target>
  joyrun target params <target>
  joyrun target nodes <target> [--partition name]
  joyrun doctor <target>
  joyrun recover <task-id> -t <target>
  joyrun recover --scan -t <target>

Global options:
  --json           emit machine-readable JSON only on stdout
  --config PATH    use a specific config file
`)
}

func (c *command) commandUsage(name string) bool {
	usage := map[string]string{
		"config": "Usage: joyrun config <path|init|validate>\n",
		"init":   "Usage: joyrun init [directory]\n",
		"submit": `Usage:
  joyrun submit <source> [<source>...] -t <target> [options]
  joyrun submit --glob <pattern> -t <target> [options]
  joyrun submit --from <file> -t <target> [options]

List multiple source paths directly; they do not need matching filenames.
Use --glob for a reliable shared pattern or --from for a reviewed source list.
`,
		"status":  "Usage: joyrun status <source|task-id> | joyrun status --all\n",
		"list":    "Usage: joyrun list [source]\n",
		"inspect": "Usage: joyrun inspect <source|task-id> [--events]\n",
		"logs":    "Usage: joyrun logs <source|task-id> [--lines N] [--file PATH]\n",
		"files":   "Usage: joyrun files <source|task-id>\n",
		"pull":    "Usage: joyrun pull <source|task-id>... [--glob pattern] [--from file] [--batch id] [--all|--include glob] [--live] [--dry-run]\n",
		"cancel":  "Usage: joyrun cancel <task-id>\n",
		"target":  "Usage: joyrun target <list|show TARGET|params TARGET|nodes TARGET [--partition name]>\n",
		"doctor":  "Usage: joyrun doctor <target>\n",
		"recover": "Usage: joyrun recover <task-id> -t <target> | joyrun recover --scan -t <target>\n",
		"version": "Usage: joyrun version\n",
	}
	text, ok := usage[name]
	if ok {
		fmt.Fprint(c.stdout, text)
	}
	return ok
}

func knownCommand(name string) bool {
	switch name {
	case "config", "init", "submit", "status", "list", "inspect", "logs",
		"files", "pull", "cancel", "target", "doctor", "recover":
		return true
	default:
		return false
	}
}

func containsFlag(args []string, name string) bool {
	for _, value := range args {
		if value == name {
			return true
		}
	}
	return false
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
	detail := ""
	if task.SchedulerState != "" && !strings.EqualFold(task.SchedulerState, task.ComputeState) {
		detail += " state:" + task.SchedulerState
	}
	if task.Elapsed != "" {
		detail += " elapsed:" + task.Elapsed
	}
	if task.ExitCode != "" {
		detail += " exit:" + task.ExitCode
	}
	if task.SchedulerReason != "" && task.SchedulerReason != "None" {
		detail += " reason:" + task.SchedulerReason
	}
	return fmt.Sprintf("%s  compute:%-17s pull:%-15s %s  %s%s%s\n",
		task.ID, strings.ToUpper(task.ComputeState), strings.ToUpper(task.PullState),
		task.TargetName, task.SourcePath, scheduler, detail)
}

func taskHeader() string {
	return fmt.Sprintf("%-29s %-19s %-17s %-24s %s\n",
		"TASK", "COMPUTE", "PULL", "TARGET", "SOURCE")
}

func formatInspect(task model.Task) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Task:          %s\nProject:       %s\nSource:        %s\nTarget:        %s\nCluster:       %s\n",
		task.ID, task.ProjectID, task.SourcePath, task.TargetName, task.ClusterName)
	fmt.Fprintf(&builder, "Compute state: %s\nPull state:    %s\nScheduler:     %s\nRemote:        %s\n",
		task.ComputeState, task.PullState, task.SchedulerID, task.RemoteDir)
	if task.Metadata["software_name"] != "" || task.Metadata["partition"] != "" {
		software := strings.TrimSpace(task.Metadata["software_name"] + " " + task.Metadata["software_version"])
		fmt.Fprintf(&builder,
			"Software:      %s\nPartition:     %s\nCores/node:    %s\nMemory/node:   %s\n",
			displayEmpty(software), displayEmpty(task.Metadata["partition"]),
			displayEmpty(task.Metadata["cores_per_node"]),
			displayEmpty(task.Metadata["memory_per_node"]))
	}
	if task.SchedulerState != "" || task.SchedulerReason != "" ||
		task.Elapsed != "" || task.ExitCode != "" ||
		task.SchedulerStart != "" || task.SchedulerEnd != "" {
		fmt.Fprintf(&builder, "Slurm state:   %s\nReason:        %s\nElapsed:       %s\nExit code:     %s\nStarted:       %s\nEnded:         %s\n",
			displayEmpty(task.SchedulerState), displayEmpty(task.SchedulerReason),
			displayEmpty(task.Elapsed), displayEmpty(task.ExitCode),
			displayEmpty(task.SchedulerStart), displayEmpty(task.SchedulerEnd))
	}
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
	if len(names) == 0 {
		return "<none>\n"
	}
	for _, name := range names {
		spec := target.Params[name]
		fmt.Fprintf(&builder, "%-20s %-8s default=%v", name, spec.Type, spec.Default)
		if spec.Required {
			builder.WriteString(" required")
		}
		if len(spec.Choices) > 0 {
			choices := make([]string, 0, len(spec.Choices))
			for _, choice := range spec.Choices {
				choices = append(choices, fmt.Sprint(choice))
			}
			builder.WriteString(" choices=" + strings.Join(choices, ","))
		}
		if spec.Description != "" {
			builder.WriteString("  " + spec.Description)
		}
		builder.WriteByte('\n')
	}
	return builder.String()
}

func formatTargetNodes(result app.TargetNodesResult) string {
	var builder strings.Builder
	fmt.Fprintf(&builder,
		"Target: %s\nCluster: %s\nSoftware: %s %s\nPartition: %s (%s)\n"+
			"Cores/node: %s\nMemory/node: %s\nObserved: %s\n\n"+
			"TOTAL  IDLE  MIXED  ALLOCATED  UNAVAILABLE\n"+
			"%-6d %-5d %-6d %-11d %d\n",
		result.Target, result.Cluster, result.Software.Name, result.Software.Version,
		result.Partition.Name, result.PartitionSource,
		displayPositive(result.Partition.CoresPerNode),
		displayEmpty(result.Partition.MemoryPerNode),
		result.ObservedAt.Local().Format(time.RFC3339),
		result.Summary.Total, result.Summary.Idle, result.Summary.Mixed,
		result.Summary.Allocated, result.Summary.Unavailable,
	)
	if len(result.Nodes) == 0 {
		return builder.String()
	}
	builder.WriteString("\nNODE                     STATE          CPUS  MEMORY_MB  GRES\n")
	for _, node := range result.Nodes {
		fmt.Fprintf(&builder, "%-24s %-14s %-5d %-10d %s\n",
			node.Name, node.State, node.CPUs, node.MemoryMB, node.GRES)
	}
	return builder.String()
}

func formatPreview(preview app.Preview) string {
	var builder strings.Builder
	entry := "<none>"
	if preview.Source.Entry != nil {
		entry = *preview.Source.Entry
	}
	fmt.Fprintf(&builder, "Task: %s\nTarget: %s\nCluster: %s\nSoftware: %s %s\nPartition: %s (%s)\nCores/node: %s\nMemory/node: %s\nRemote: %s\nScheduler log: %s\n\nSource:\n  Path:     %s\n  Kind:     %s\n  WorkDir:  %s\n  Entry:    %s\n\nUpload policy:\n  Mode:           %s\n  Include:        %s\n  Max files:      %s\n  Max total size: %s\n\nTemplate values:\n  Input:    %s\n  Stem:     %s\n  Name:     %s\n  WorkDir:  %s\n\nParameters:\n",
		preview.TaskID, preview.Target, preview.Cluster,
		preview.Software.Name, preview.Software.Version,
		displayEmpty(preview.Partition.Name), displayEmpty(preview.PartitionSource),
		displayPositive(preview.Partition.CoresPerNode), displayEmpty(preview.Partition.MemoryPerNode),
		preview.RemoteDir, preview.SchedulerLog,
		preview.Source.RelativePath, preview.Source.Kind, preview.Source.WorkDir, entry,
		preview.Push.Mode, displayList(preview.Push.Include),
		displayLimit(preview.Push.Limits.MaxFiles), displayEmpty(preview.Push.Limits.MaxTotalSize),
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
	var total int64
	for _, file := range preview.InputManifest {
		total += file.Size
	}
	fmt.Fprintf(&builder, "\nUpload snapshot: %d file(s), %s\n",
		len(preview.InputManifest), humanBytes(total))
	for _, file := range preview.InputManifest {
		fmt.Fprintf(&builder, "  %10s  %s\n", humanBytes(file.Size), file.Path)
	}
	if len(preview.Ignored) > 0 {
		fmt.Fprintf(&builder, "\nIgnored: %d path(s)\n", len(preview.Ignored))
		for _, file := range preview.Ignored {
			builder.WriteString("  " + file + "\n")
		}
	}
	builder.WriteString("\nRendered script:\n")
	builder.WriteString(preview.RenderedScript)
	if !strings.HasSuffix(preview.RenderedScript, "\n") {
		builder.WriteByte('\n')
	}
	return builder.String()
}

func displayList(values []string) string {
	if len(values) == 0 {
		return "<none>"
	}
	return strings.Join(values, ", ")
}

func displayLimit(value int) string {
	if value == 0 {
		return "<unlimited>"
	}
	return strconv.Itoa(value)
}

func displayPositive(value int) string {
	if value <= 0 {
		return "<unknown>"
	}
	return strconv.Itoa(value)
}

func humanBytes(size int64) string {
	const unit = int64(1024)
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	for _, name := range units {
		value /= 1024
		if value < 1024 || name == units[len(units)-1] {
			return fmt.Sprintf("%.1f %s", value, name)
		}
	}
	panic("unreachable")
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
