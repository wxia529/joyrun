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
