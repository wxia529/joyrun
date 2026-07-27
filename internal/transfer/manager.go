package transfer

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"

	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/model"
	"github.com/wxia529/joyrun/internal/remote"
)

type Manager struct {
	Stderr   io.Writer
	GOOS     string
	LookPath func(string) (string, error)
	SFTP     *SFTP
	Runner   remote.Runner
}

func (m Manager) Push(ctx context.Context, cluster model.Cluster, localDir, remoteDir string, excludes []string) error {
	backend, err := m.backend(ctx, cluster)
	if err != nil {
		return err
	}
	switch backend {
	case "rsync":
		return (Rsync{Stderr: m.Stderr}).Push(ctx, cluster.Host, localDir, remoteDir, excludes)
	case "sftp":
		return m.sftp().Push(ctx, cluster.Host, localDir, remoteDir, excludes)
	default:
		panic("unreachable transfer backend")
	}
}

func (m Manager) Pull(ctx context.Context, cluster model.Cluster, remoteDir, localDir string, files []string) error {
	backend, err := m.backend(ctx, cluster)
	if err != nil {
		return err
	}
	switch backend {
	case "rsync":
		return (Rsync{Stderr: m.Stderr}).Pull(ctx, cluster.Host, remoteDir, localDir, files)
	case "sftp":
		return m.sftp().Pull(ctx, cluster.Host, remoteDir, localDir, files)
	default:
		panic("unreachable transfer backend")
	}
}

func (m Manager) Check(ctx context.Context, cluster model.Cluster) (string, error) {
	backend, err := m.backend(ctx, cluster)
	if err != nil {
		return backend, err
	}
	if backend == "sftp" {
		return backend, m.sftp().Check(ctx, cluster.Host)
	}
	if m.Runner != nil {
		if _, stderr, err := m.Runner.Exec(ctx, cluster.Host, "command -v rsync", nil); err != nil {
			return backend, fault.Wrap("TRANSFER_UNAVAILABLE", withDetail("rsync is not installed on the remote cluster", stderr), false, err)
		}
	}
	return backend, nil
}

func (m Manager) backend(ctx context.Context, cluster model.Cluster) (string, error) {
	requested := cluster.Transfer
	if requested == "" {
		requested = "auto"
	}
	switch requested {
	case "auto":
		if m.goos() != "windows" {
			if _, err := m.lookPath()("rsync"); err == nil {
				if m.Runner == nil {
					return "rsync", nil
				}
				stdout, stderr, err := m.Runner.Exec(ctx, cluster.Host,
					"if command -v rsync >/dev/null 2>&1; then printf yes; else printf no; fi", nil)
				if err != nil {
					return "", fault.Wrap("TRANSFER_DETECTION_FAILED", withDetail("cannot detect the remote transfer backend", stderr), true, err)
				}
				if strings.TrimSpace(stdout) == "yes" {
					return "rsync", nil
				}
			}
		}
		if _, err := m.lookPath()("ssh"); err != nil {
			return "sftp", fault.Wrap("TRANSFER_UNAVAILABLE", "neither a usable rsync backend nor OpenSSH was found", false, err)
		}
		return "sftp", nil
	case "rsync":
		if _, err := m.lookPath()("rsync"); err != nil {
			return "rsync", fault.Wrap("TRANSFER_UNAVAILABLE", "rsync was explicitly selected but is not installed", false, err)
		}
		return "rsync", nil
	case "sftp":
		if _, err := m.lookPath()("ssh"); err != nil {
			return "sftp", fault.Wrap("TRANSFER_UNAVAILABLE", "sftp requires the OpenSSH client", false, err)
		}
		return "sftp", nil
	default:
		return requested, fault.New("TRANSFER_UNAVAILABLE", fmt.Sprintf("unsupported transfer backend %q", requested), false)
	}
}

func (m Manager) goos() string {
	if m.GOOS != "" {
		return m.GOOS
	}
	return runtime.GOOS
}

func (m Manager) lookPath() func(string) (string, error) {
	if m.LookPath != nil {
		return m.LookPath
	}
	return exec.LookPath
}

func (m Manager) sftp() *SFTP {
	if m.SFTP != nil {
		return m.SFTP
	}
	return &SFTP{Stderr: m.Stderr}
}
