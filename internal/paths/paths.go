package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

func ConfigFile() string {
	if p := os.Getenv("JOYRUN_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return configFile(runtime.GOOS, os.Getenv, home)
}

func configFile(goos string, getenv func(string) string, home string) string {
	base := getenv("XDG_CONFIG_HOME")
	if base == "" {
		if goos == "windows" {
			base = getenv("APPDATA")
		}
		if base == "" {
			base = filepath.Join(home, ".config")
		}
	}
	return filepath.Join(base, "joyrun", "config.yaml")
}

func DatabaseFile() string {
	if p := os.Getenv("JOYRUN_DB"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return databaseFile(runtime.GOOS, os.Getenv, home)
}

// OperationDataDir is private local state used for durable daemon operation
// snapshots. It intentionally follows the database's per-user data root.
func OperationDataDir() string {
	database := DatabaseFile()
	return filepath.Join(filepath.Dir(database), "operations")
}

func databaseFile(goos string, getenv func(string) string, home string) string {
	base := getenv("XDG_DATA_HOME")
	if base == "" {
		if goos == "windows" {
			base = getenv("LOCALAPPDATA")
		}
		if base == "" {
			base = filepath.Join(home, ".local", "share")
		}
	}
	return filepath.Join(base, "joyrun", "joyrun.db")
}
