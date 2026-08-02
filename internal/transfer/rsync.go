package transfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/remote"
)

type Rsync struct {
	Stderr      io.Writer
	ControlPath string
}

func (r Rsync) Push(ctx context.Context, host, localDir, remoteDir string, excludes []string) error {
	args := baseArgs(r.ControlPath)
	args = append(args, "--rsync-path", "mkdir -p "+remote.Quote(remoteDir)+" && rsync")
	for _, pattern := range excludes {
		args = append(args, "--exclude", pattern)
	}
	args = append(args, filepath.Clean(localDir)+string(filepath.Separator), host+":"+strings.TrimSuffix(remoteDir, "/")+"/")
	return r.run(ctx, "UPLOAD_FAILED", "rsync upload failed", args, nil)
}

func (r Rsync) Pull(ctx context.Context, host, remoteDir, localDir string, files []string) error {
	if len(files) == 0 {
		return nil
	}
	temp, err := os.CreateTemp("", "joyrun-files-*")
	if err != nil {
		return fault.Wrap("PULL_FAILED", "cannot prepare pull file list", false, err)
	}
	name := temp.Name()
	defer os.Remove(name)
	for _, file := range files {
		if _, err := fmt.Fprint(temp, filepath.ToSlash(file), "\x00"); err != nil {
			temp.Close()
			return fault.Wrap("PULL_FAILED", "cannot write pull file list", false, err)
		}
	}
	if err := temp.Close(); err != nil {
		return fault.Wrap("PULL_FAILED", "cannot finalize pull file list", false, err)
	}
	args := append(baseArgs(r.ControlPath), "--from0", "--files-from", name,
		host+":"+strings.TrimSuffix(remoteDir, "/")+"/", filepath.Clean(localDir)+string(filepath.Separator))
	return r.run(ctx, "PULL_FAILED", "rsync pull failed", args, nil)
}

func baseArgs(controlPathArgs ...string) []string {
	controlPath := ""
	if len(controlPathArgs) > 0 {
		controlPath = controlPathArgs[0]
	}
	return []string{
		"-az",
		"--partial",
		"--protect-args",
		"--info=progress2",
		"--timeout=90",
		"-e", remote.OpenSSHShellFor(controlPath),
	}
}

func (r Rsync) run(ctx context.Context, code, message string, args []string, stdin io.Reader) error {
	cmd := exec.CommandContext(ctx, "rsync", args...)
	cmd.Stdin = stdin
	cmd.Stdout = r.Stderr
	var stderr bytes.Buffer
	if r.Stderr != nil {
		cmd.Stderr = io.MultiWriter(&stderr, r.Stderr)
	} else {
		cmd.Stderr = &stderr
	}
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			message += ": " + detail
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 30 {
			if code == "UPLOAD_FAILED" {
				code = "UPLOAD_TIMEOUT"
			} else if code == "PULL_FAILED" {
				code = "PULL_TIMEOUT"
			}
		}
		return fault.Wrap(code, message, true, err)
	}
	return nil
}
