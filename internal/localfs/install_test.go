package localfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallStagedFileReplacesExistingResult(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "staging", "result.out")
	destination := filepath.Join(root, "task", "result.out")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InstallStagedFile(source, destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("unexpected installed content %q", data)
	}
}
