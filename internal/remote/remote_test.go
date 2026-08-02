package remote

import (
	"context"
	"io"
	"strings"
	"testing"
)

type captureRunner struct {
	command string
	content string
}

func (r *captureRunner) Exec(_ context.Context, _, command string, stdin io.Reader) (string, string, error) {
	r.command = command
	data, _ := io.ReadAll(stdin)
	r.content = string(data)
	return "", "", nil
}

func TestWriteFileUsesAtomicReplacement(t *testing.T) {
	runner := &captureRunner{}
	if err := WriteFile(context.Background(), runner, "unused",
		"/remote/task/metadata.json", []byte("metadata"), "600"); err != nil {
		t.Fatal(err)
	}
	if runner.content != "metadata" ||
		!strings.Contains(runner.command, "metadata.json.joyrun-tmp") ||
		!strings.Contains(runner.command, "mv -f \"$tmp\" '/remote/task/metadata.json'") {
		t.Fatalf("remote file was not written atomically: %q", runner.command)
	}
}

func TestOpenSSHOptionsBoundConnectionStalls(t *testing.T) {
	options := strings.Join(OpenSSHOptions(), " ")
	for _, expected := range []string{
		"BatchMode=yes",
		"ConnectTimeout=15",
		"ServerAliveInterval=15",
		"ServerAliveCountMax=3",
	} {
		if !strings.Contains(options, expected) {
			t.Fatalf("missing OpenSSH safeguard %q in %q", expected, options)
		}
	}
}

func TestOpenSSHOptionsForControlPath(t *testing.T) {
	shell := OpenSSHShellFor("/tmp/joyrun/control-%C")
	for _, expected := range []string{"ControlMaster=auto", "ControlPersist=300", "ControlPath=/tmp/joyrun/control-%C"} {
		if !strings.Contains(shell, expected) {
			t.Fatalf("control option %q missing from %q", expected, shell)
		}
	}
}
