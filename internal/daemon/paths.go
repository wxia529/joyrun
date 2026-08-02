package daemon

import (
	"os"
	"path/filepath"
	"runtime"
)

func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	base := os.Getenv("XDG_STATE_HOME")
	if runtime.GOOS == "windows" {
		base = os.Getenv("LOCALAPPDATA")
	}
	if base == "" {
		base = filepath.Join(home, ".local", "share")
	}
	runDir := filepath.Join(base, "joyrun", "run")
	return Paths{
		Endpoint: endpointPath(runDir),
		Lock:     filepath.Join(runDir, "daemon.lock"),
		Secret:   filepath.Join(runDir, "daemon.secret"),
		Log:      filepath.Join(base, "joyrun", "daemon.log"),
	}, nil
}

func prepareRuntime(paths Paths) error {
	if err := os.MkdirAll(filepath.Dir(paths.Lock), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(paths.Log), 0o700); err != nil {
		return err
	}
	return nil
}

func endpointPath(runDir string) string {
	if runtime.GOOS == "windows" {
		return `\\.\pipe\joyrun-daemon`
	}
	return filepath.Join(runDir, "daemon.sock")
}
