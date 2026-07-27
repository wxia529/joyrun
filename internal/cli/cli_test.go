package cli

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/wxia529/joyrun/internal/app"
	"github.com/wxia529/joyrun/internal/model"
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
