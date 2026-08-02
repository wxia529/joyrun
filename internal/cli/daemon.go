package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/wxia529/joyrun/internal/daemon"
	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/paths"
	"github.com/wxia529/joyrun/internal/store"
)

func shouldUseDaemon(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "config", "database", "version", "--version", "help", "--help", "-h", "daemon":
		return false
	case "target":
		// Listing and inspecting local configuration do not need the daemon;
		// node observations do because they use SSH.
		return len(args) < 2 || (args[1] != "list" && args[1] != "show" && args[1] != "params")
	default:
		return true
	}
}

func (c *command) forwardToDaemon(args []string) int {
	runtime, err := daemon.DefaultPaths()
	if err != nil {
		c.writeError(fault.Wrap("DAEMON_UNAVAILABLE", "cannot determine daemon runtime paths", true, err))
		return 1
	}
	response, err := daemon.Call(c.ctx, runtime, c.version, args)
	if err == nil {
		if response.Stdout != "" {
			_, _ = io.WriteString(c.stdout, response.Stdout)
		}
		if response.Stderr != "" {
			_, _ = io.WriteString(c.stderr, response.Stderr)
		}
		return response.ExitCode
	}
	if fault.As(err).Code == "DAEMON_UNAVAILABLE" {
		err = fault.New("DAEMON_REQUIRED",
			"this command requires the JoyRun daemon; run `joyrun daemon start`", true)
	}
	c.writeError(err)
	return 1
}

func (c *command) daemonCommand(args []string) int {
	if len(args) == 0 {
		c.writeError(fault.New("INVALID_ARGUMENT", "usage: joyrun daemon <start|run|status|stop|logs>", false))
		return 1
	}
	runtime, err := daemon.DefaultPaths()
	if err != nil {
		c.writeError(fault.Wrap("DAEMON_START_FAILED", "cannot determine daemon runtime paths", false, err))
		return 1
	}
	switch args[0] {
	case "run":
		if len(args) != 1 && !(len(args) == 2 && args[1] == "--foreground") {
			c.writeError(fault.New("INVALID_ARGUMENT", "usage: joyrun daemon run --foreground", false))
			return 1
		}
		db, dbErr := store.Open(paths.DatabaseFile())
		if dbErr != nil {
			c.writeError(dbErr)
			return 1
		}
		_ = db.Close()
		server := daemon.NewServer(runtime, c.version, store.SchemaLabel,
			func(ctx context.Context, commandArgs []string, cwd string) (int, string, string) {
				var stdout, stderr bytes.Buffer
				ctx = context.WithValue(ctx, daemonExecutionKey{}, true)
				code := run(ctx, commandArgs, c.version, &stdout, &stderr, cwd, true)
				return code, stdout.String(), stderr.String()
			})
		server.Worker = func(ctx context.Context) {
			go c.pollWorker(ctx, c.version)
			c.operationWorker(ctx, c.version)
		}
		if err := server.Run(c.ctx); err != nil {
			c.writeError(err)
			return 1
		}
		return 0
	case "start":
		return c.startDaemon(runtime)
	case "status":
		return c.daemonStatus(runtime)
	case "stop":
		return c.daemonStop(runtime)
	case "logs":
		return c.daemonLogs(runtime, args[1:])
	default:
		c.writeError(fault.New("INVALID_ARGUMENT", "usage: joyrun daemon <start|run|status|stop|logs>", false))
		return 1
	}
}

func (c *command) startDaemon(runtime daemon.Paths) int {
	if response, err := daemon.Call(c.ctx, runtime, c.version, []string{"daemon", "status"}); err == nil && response.ExitCode == 0 {
		return c.emitDaemonJSONOrText(response.Stdout, "Daemon already running.\n")
	}
	if err := os.MkdirAll(filepathDir(runtime.Log), 0o700); err != nil {
		c.writeError(fault.Wrap("DAEMON_START_FAILED", "cannot create daemon log directory", false, err))
		return 1
	}
	logFile, err := os.OpenFile(runtime.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		c.writeError(fault.Wrap("DAEMON_START_FAILED", "cannot open daemon log", false, err))
		return 1
	}
	defer logFile.Close()
	executable, err := os.Executable()
	if err != nil {
		c.writeError(fault.Wrap("DAEMON_START_FAILED", "cannot locate joyrun executable", false, err))
		return 1
	}
	child := exec.Command(executable, "daemon", "run", "--foreground")
	child.Stdout, child.Stderr = logFile, logFile
	if err := child.Start(); err != nil {
		c.writeError(fault.Wrap("DAEMON_START_FAILED", "cannot start daemon process", false, err))
		return 1
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		response, callErr := daemon.Call(c.ctx, runtime, c.version, []string{"daemon", "status"})
		if callErr == nil && response.ExitCode == 0 {
			return c.emitDaemonJSONOrText(response.Stdout, "Daemon started.\n")
		}
		select {
		case <-c.ctx.Done():
			c.writeError(fault.Wrap("DAEMON_START_FAILED", "daemon startup cancelled", true, c.ctx.Err()))
			return 1
		case <-time.After(100 * time.Millisecond):
		}
	}
	_ = child.Process.Kill()
	_ = os.Remove(runtime.Secret)
	_ = os.Remove(runtime.Endpoint)
	c.writeError(fault.New("DAEMON_START_FAILED", "daemon did not become ready within 15 seconds", true))
	return 1
}

func (c *command) daemonStatus(runtime daemon.Paths) int {
	response, err := daemon.Call(c.ctx, runtime, c.version, []string{"daemon", "status"})
	if err != nil {
		c.writeError(err)
		return 1
	}
	return c.emitDaemonJSONOrText(response.Stdout, "Daemon is running.\n")
}

func (c *command) daemonStop(runtime daemon.Paths) int {
	response, err := daemon.Call(c.ctx, runtime, c.version, []string{"daemon", "stop"})
	if err != nil {
		c.writeError(err)
		return 1
	}
	if c.json {
		c.write(map[string]any{"stopped": true}, "")
	} else {
		fmt.Fprintln(c.stdout, "Daemon stopped.")
	}
	return response.ExitCode
}

func (c *command) daemonLogs(runtime daemon.Paths, args []string) int {
	lines := 100
	if len(args) == 2 && args[0] == "--lines" {
		value, err := strconv.Atoi(args[1])
		if err != nil || value < 1 {
			c.writeError(fault.New("INVALID_ARGUMENT", "usage: joyrun daemon logs [--lines N]", false))
			return 1
		}
		lines = value
	} else if len(args) != 0 {
		c.writeError(fault.New("INVALID_ARGUMENT", "usage: joyrun daemon logs [--lines N]", false))
		return 1
	}
	data, err := os.ReadFile(runtime.Log)
	if err != nil {
		c.writeError(fault.Wrap("DAEMON_LOG_FAILED", "cannot read daemon log", false, err))
		return 1
	}
	content := tailLines(string(data), lines)
	if c.json {
		c.write(map[string]any{"path": runtime.Log, "content": content}, "")
	} else {
		_, _ = io.WriteString(c.stdout, content)
	}
	return 0
}

func (c *command) emitDaemonJSONOrText(raw, human string) int {
	if c.json {
		var value any
		if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &value); err != nil {
			c.writeError(fault.Wrap("DAEMON_PROTOCOL_FAILED", "daemon returned invalid status JSON", false, err))
			return 1
		}
		c.write(value, "")
		return 0
	}
	if raw != "" {
		var value struct {
			Version    string `json:"version"`
			PID        int    `json:"pid"`
			Schema     string `json:"schema"`
			InstanceID string `json:"instance_id"`
			StartedAt  string `json:"started_at"`
		}
		if json.Unmarshal([]byte(raw), &value) == nil {
			fmt.Fprintf(c.stdout, "Daemon running: pid=%d version=%s schema=%s started=%s instance=%s\n",
				value.PID, value.Version, value.Schema, value.StartedAt, value.InstanceID)
			return 0
		}
	}
	_, _ = io.WriteString(c.stdout, human)
	return 0
}

func tailLines(content string, lines int) string {
	parts := strings.Split(content, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n") + "\n"
}

func filepathDir(path string) string {
	index := strings.LastIndexAny(path, "/\\")
	if index < 0 {
		return "."
	}
	return path[:index]
}
