package transfer

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/wxia529/joyrun/internal/model"
)

func TestAutoBackendSelection(t *testing.T) {
	found := func(name string) (string, error) { return "/bin/" + name, nil }
	missingRsync := func(name string) (string, error) {
		if name == "rsync" {
			return "", errors.New("missing")
		}
		return "/bin/" + name, nil
	}
	tests := []struct {
		name    string
		manager Manager
		cluster model.Cluster
		want    string
	}{
		{"windows always uses sftp automatically", Manager{GOOS: "windows", LookPath: found}, model.Cluster{Transfer: "auto"}, "sftp"},
		{"unix prefers rsync", Manager{GOOS: "linux", LookPath: found}, model.Cluster{Transfer: "auto"}, "rsync"},
		{"unix falls back to sftp", Manager{GOOS: "linux", LookPath: missingRsync}, model.Cluster{Transfer: "auto"}, "sftp"},
		{"explicit sftp", Manager{GOOS: "linux", LookPath: found}, model.Cluster{Transfer: "sftp"}, "sftp"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.manager.backend(context.Background(), test.cluster)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("want %s, got %s", test.want, got)
			}
		})
	}
}

func TestExplicitMissingBackendFails(t *testing.T) {
	manager := Manager{
		GOOS: "linux",
		LookPath: func(string) (string, error) {
			return "", errors.New("missing")
		},
	}
	if _, err := manager.backend(context.Background(), model.Cluster{Transfer: "rsync"}); err == nil {
		t.Fatal("expected missing explicitly selected rsync to fail")
	}
	if _, err := manager.backend(context.Background(), model.Cluster{Transfer: "sftp"}); err == nil {
		t.Fatal("expected missing OpenSSH to fail")
	}
}

func TestAutoSelectionDoesNotProbeRemoteRsync(t *testing.T) {
	manager := Manager{
		GOOS:     "linux",
		LookPath: func(name string) (string, error) { return "/bin/" + name, nil },
		Runner:   staticCommandRunner{stdout: "no"},
	}
	got, err := manager.backend(context.Background(), model.Cluster{Host: "cluster", Transfer: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "rsync" {
		t.Fatalf("want rsync without a remote probe, got %s", got)
	}
}

type staticCommandRunner struct {
	stdout string
	err    error
}

func (r staticCommandRunner) Exec(context.Context, string, string, io.Reader) (string, string, error) {
	return r.stdout, "", r.err
}
