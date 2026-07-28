//go:build !windows

package manifest

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestBuildRejectsNamedPipeWithoutOpeningIt(t *testing.T) {
	root := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(root, "stream.fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Build(root, Selection{Mode: "workdir"}); err == nil {
		t.Fatal("expected named pipe to be rejected")
	}
}
