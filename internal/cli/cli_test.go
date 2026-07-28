package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wxia529/joyrun/internal/app"
	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/model"
	"github.com/wxia529/joyrun/internal/project"
)

func TestInterspersedCanonicalSubmitSyntax(t *testing.T) {
	got := interspersed(
		[]string{"task01/eg.inp", "-t", "gibbs/orca", "--set", "cpus=64", "--dry-run"},
		map[string]bool{"--target": true, "-t": true, "--set": true},
		map[string]bool{"--dry-run": true},
	)
	want := []string{"-t", "gibbs/orca", "--set", "cpus=64", "--dry-run", "task01/eg.inp"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestUsageExposesCompressedCommandSurface(t *testing.T) {
	var output bytes.Buffer
	c := &command{stdout: &output}
	c.usage()
	help := output.String()
	for _, expected := range []string{
		"joyrun target list",
		"joyrun list [source]",
		"joyrun status --all",
		"joyrun inspect <source|task-id> --events",
	} {
		if !strings.Contains(help, expected) {
			t.Fatalf("help is missing %q:\n%s", expected, help)
		}
	}
	for _, removed := range []string{
		"joyrun targets",
		"joyrun history",
		"joyrun trace",
		"joyrun reconcile",
		"target <show|params>",
	} {
		if strings.Contains(help, removed) {
			t.Fatalf("help still exposes removed command %q:\n%s", removed, help)
		}
	}
}

func TestCommandHelpDoesNotNeedConfiguration(t *testing.T) {
	var stdout, stderr bytes.Buffer
	c := &command{stdout: &stdout, stderr: &stderr}
	if !c.commandUsage("submit") {
		t.Fatal("submit help was not recognized")
	}
	if !strings.Contains(stdout.String(), "joyrun submit") {
		t.Fatalf("unexpected command help: %q", stdout.String())
	}
}

func TestHumanErrorShowsRecoveryContext(t *testing.T) {
	var stderr bytes.Buffer
	c := &command{stderr: &stderr}
	c.writeError(fault.New("PULL_FAILED", "connection reset", true).
		WithTask("pull", "joyrun pull jr_test", "completed", "failed"))
	output := stderr.String()
	for _, expected := range []string{
		"Error [PULL_FAILED]", "Stage: pull", "compute=completed",
		"Retryable: yes", "Next: joyrun pull jr_test",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("human error is missing %q:\n%s", expected, output)
		}
	}
}

func TestFormatPreviewShowsSizesAndIgnoredPaths(t *testing.T) {
	output := formatPreview(app.Preview{
		InputManifest: []model.ManifestEntry{{Path: "input.dat", Size: 2048}},
		Ignored:       []string{"old.out"},
		Params:        map[string]any{},
	})
	for _, expected := range []string{"1 file(s), 2.0 KiB", "input.dat", "Ignored: 1 path(s)", "old.out"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("preview is missing %q:\n%s", expected, output)
		}
	}
}

type doctorRunner struct{}

func (doctorRunner) Exec(_ context.Context, _, command string, _ io.Reader) (string, string, error) {
	if command != "true" && len(command) >= 5 && command[:5] == "root=" {
		return "not_writable", "", nil
	}
	return "", "", nil
}

type doctorTransfer struct{}

func (doctorTransfer) Push(context.Context, model.Cluster, string, string, []string) error {
	return nil
}
func (doctorTransfer) Pull(context.Context, model.Cluster, string, string, []string) error {
	return nil
}
func (doctorTransfer) Check(context.Context, model.Cluster) (string, error) {
	return "fake", nil
}

func TestDoctorSetsNonzeroExitWithoutReturningOutputError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	c := &command{ctx: context.Background(), stdout: &stdout, stderr: &stderr}
	application := &app.App{
		Config: model.Config{
			Clusters: map[string]model.Cluster{"c": {
				Host: "c", Scheduler: "slurm", RemoteRoot: "/shared/joyrun",
			}},
			Targets: map[string]model.Target{"c/run": {Cluster: "c", Script: "run"}},
		},
		Runner: doctorRunner{}, Transfer: doctorTransfer{},
	}
	if err := c.doctor(application, []string{"c/run"}); err != nil {
		t.Fatal(err)
	}
	if c.exitCode != 1 || !bytes.Contains(stdout.Bytes(), []byte("Suggested action:")) {
		t.Fatalf("unexpected doctor result: exit=%d stdout=%q", c.exitCode, stdout.String())
	}
}

func TestTargetListDoesNotRequireTaskDatabase(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(`
version: 1
clusters:
  c:
    host: c
    scheduler: slurm
    remote_root: /tmp/joyrun
targets:
  c/run:
    cluster: c
    source:
      kind: directory
    script: "true"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JOYRUN_DB", t.TempDir())
	var stdout, stderr bytes.Buffer
	c := &command{
		ctx: context.Background(), config: configPath, json: true,
		stdout: &stdout, stderr: &stderr,
	}
	if err := c.execute("target", []string{"list"}); err != nil {
		t.Fatalf("target list unexpectedly required the task database: %v", err)
	}
}

func TestDryRunDoesNotRequireTaskDatabase(t *testing.T) {
	root := t.TempDir()
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "input.dat"), []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(`
version: 1
clusters:
  c:
    host: c
    scheduler: slurm
    remote_root: /tmp/joyrun
targets:
  c/run:
    cluster: c
    source:
      kind: file
    script: "cp {{ .Input }} result.dat"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	t.Setenv("JOYRUN_DB", t.TempDir())
	var stdout, stderr bytes.Buffer
	c := &command{
		ctx: context.Background(), config: configPath,
		stdout: &stdout, stderr: &stderr,
	}
	if err := c.execute("submit", []string{"input.dat", "-t", "c/run", "--dry-run"}); err != nil {
		t.Fatalf("dry-run unexpectedly required the task database: %v", err)
	}
}
