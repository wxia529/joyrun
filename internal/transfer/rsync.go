package transfer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wxia529/joyrun/internal/fault"
)

type Rsync struct {
	Stderr io.Writer
}

func (r Rsync) Push(ctx context.Context, host, localDir, remoteDir string, excludes []string) error {
	args := []string{"-az", "--partial", "--protect-args"}
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
	args := []string{"-az", "--partial", "--protect-args", "--from0", "--files-from", name,
		host + ":" + strings.TrimSuffix(remoteDir, "/") + "/", filepath.Clean(localDir) + string(filepath.Separator)}
	return r.run(ctx, "PULL_FAILED", "rsync pull failed", args, nil)
}

func (r Rsync) run(ctx context.Context, code, message string, args []string, stdin io.Reader) error {
	cmd := exec.CommandContext(ctx, "rsync", args...)
	cmd.Stdin = stdin
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
		return fault.Wrap(code, message, true, err)
	}
	return nil
}
