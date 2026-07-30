package localfs

import (
	"io"
	"os"
	"path/filepath"

	"github.com/wxia529/joyrun/internal/fault"
)

// InstallStagedFile atomically replaces a destination with a downloaded
// staging file while remaining safe on Windows.
func InstallStagedFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return fault.Wrap("LOCAL_INSTALL_FAILED", "cannot open staged result", false, err)
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return fault.Wrap("LOCAL_INSTALL_FAILED", "cannot inspect staged result", false, err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fault.Wrap("LOCAL_INSTALL_FAILED", "cannot create result directory", false, err)
	}
	output, err := os.CreateTemp(filepath.Dir(destination),
		"."+filepath.Base(destination)+".joyrun-*")
	if err != nil {
		return fault.Wrap("LOCAL_INSTALL_FAILED", "cannot create result temporary file", false, err)
	}
	tempPath := output.Name()
	keep := true
	defer func() {
		_ = output.Close()
		if keep {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return fault.Wrap("LOCAL_INSTALL_FAILED", "cannot copy staged result", false, err)
	}
	if err := output.Sync(); err != nil {
		return fault.Wrap("LOCAL_INSTALL_FAILED", "cannot sync staged result", false, err)
	}
	if err := output.Chmod(info.Mode().Perm()); err != nil {
		return fault.Wrap("LOCAL_INSTALL_FAILED", "cannot preserve result permissions", false, err)
	}
	if err := output.Close(); err != nil {
		return fault.Wrap("LOCAL_INSTALL_FAILED", "cannot close staged result", false, err)
	}
	if err := replaceInstalledFile(tempPath, destination); err != nil {
		return fault.Wrap("LOCAL_INSTALL_FAILED", "cannot replace local result", false, err)
	}
	keep = false
	return nil
}
