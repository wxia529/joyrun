package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotDereferencesInternalFileSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "input.dat"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("input.dat", filepath.Join(root, "alias.dat")); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	staging, entries, _, cleanup, err := Snapshot(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	info, err := os.Lstat(filepath.Join(staging, "alias.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("snapshot alias should be a regular file, got %s", info.Mode())
	}
	data, err := os.ReadFile(filepath.Join(staging, "alias.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "content" || len(entries) != 2 {
		t.Fatalf("unexpected snapshot: %q, %#v", data, entries)
	}
}

func TestSnapshotRejectsSymlinkOutsideWorkdir(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "work")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if _, _, _, cleanup, err := Snapshot(root, nil); err == nil {
		cleanup()
		t.Fatal("expected external symbolic link to be rejected")
	}
}
