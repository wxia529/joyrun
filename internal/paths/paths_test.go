package paths

import (
	"path/filepath"
	"testing"
)

func TestWindowsPathsUseApplicationData(t *testing.T) {
	env := map[string]string{
		"APPDATA":      `C:\Users\test\AppData\Roaming`,
		"LOCALAPPDATA": `C:\Users\test\AppData\Local`,
	}
	getenv := func(key string) string { return env[key] }
	if got := configFile("windows", getenv, `C:\Users\test`); got != filepath.Join(env["APPDATA"], "joyrun", "config.yaml") {
		t.Fatalf("unexpected config path: %s", got)
	}
	if got := databaseFile("windows", getenv, `C:\Users\test`); got != filepath.Join(env["LOCALAPPDATA"], "joyrun", "joyrun.db") {
		t.Fatalf("unexpected database path: %s", got)
	}
}

func TestXDGPathsTakePriority(t *testing.T) {
	env := map[string]string{
		"XDG_CONFIG_HOME": "/config",
		"XDG_DATA_HOME":   "/data",
		"APPDATA":         `C:\ignored`,
		"LOCALAPPDATA":    `C:\ignored`,
	}
	getenv := func(key string) string { return env[key] }
	if got := configFile("windows", getenv, "/home/test"); got != filepath.Join("/config", "joyrun", "config.yaml") {
		t.Fatalf("unexpected config path: %s", got)
	}
	if got := databaseFile("windows", getenv, "/home/test"); got != filepath.Join("/data", "joyrun", "joyrun.db") {
		t.Fatalf("unexpected database path: %s", got)
	}
}
