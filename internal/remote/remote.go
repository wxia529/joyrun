package remote

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/wxia529/joyrun/internal/fault"
)

type Runner interface {
	Exec(ctx context.Context, host, command string, stdin io.Reader) (string, string, error)
}

type SSH struct {
	Stderr      io.Writer
	Timeout     time.Duration
	ControlPath string
}

func (s SSH) Exec(ctx context.Context, host, command string, stdin io.Reader) (string, string, error) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	args := append(OpenSSHOptionsFor(s.ControlPath), host, command)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.MultiWriter(&stderr, writerOrDiscard(s.Stderr))
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func OpenSSHOptions() []string {
	return OpenSSHOptionsFor("")
}

func OpenSSHOptionsFor(controlPath string) []string {
	options := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=15",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	}
	if controlPath != "" {
		options = append(options, "-o", "ControlMaster=auto", "-o", "ControlPersist=300", "-o", "ControlPath="+controlPath)
	}
	return options
}

func OpenSSHShell() string {
	return OpenSSHShellFor("")
}

func OpenSSHShellFor(controlPath string) string {
	options := OpenSSHOptionsFor(controlPath)
	parts := make([]string, 0, len(options)/2+1)
	parts = append(parts, "ssh")
	for index := 0; index+1 < len(options); index += 2 {
		parts = append(parts, Quote(options[index]), Quote(options[index+1]))
	}
	return strings.Join(parts, " ")
}

func Check(ctx context.Context, runner Runner, host string) error {
	_, stderr, err := runner.Exec(ctx, host, "true", nil)
	if err != nil {
		return fault.Wrap("SSH_FAILED", detail("cannot connect to "+host, stderr), true, err)
	}
	return nil
}

func WriteFile(ctx context.Context, runner Runner, host, path string, content []byte, mode string) error {
	tempPrefix := path + ".joyrun-tmp"
	command := fmt.Sprintf(
		"umask 077 && mkdir -p %s && tmp=%s.$$ && "+
			"trap 'rm -f \"$tmp\"' EXIT HUP INT TERM && "+
			"cat > \"$tmp\" && chmod %s \"$tmp\" && mv -f \"$tmp\" %s && trap - EXIT HUP INT TERM",
		Quote(Dir(path)), Quote(tempPrefix), Quote(mode), Quote(path))
	_, stderr, err := runner.Exec(ctx, host, command, bytes.NewReader(content))
	if err != nil {
		return fault.Wrap("REMOTE_WRITE_FAILED", detail("cannot write remote file", stderr), true, err)
	}
	return nil
}

func ReadFile(ctx context.Context, runner Runner, host, path string) ([]byte, error) {
	stdout, stderr, err := runner.Exec(ctx, host, "cat "+Quote(path), nil)
	if err != nil {
		return nil, fault.Wrap("REMOTE_READ_FAILED", detail("cannot read remote file", stderr), true, err)
	}
	return []byte(stdout), nil
}

func Quote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func Dir(path string) string {
	index := strings.LastIndex(path, "/")
	if index <= 0 {
		return "."
	}
	return path[:index]
}

func writerOrDiscard(writer io.Writer) io.Writer {
	if writer == nil {
		return io.Discard
	}
	return writer
}

func detail(message, stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return message
	}
	return message + ": " + stderr
}
