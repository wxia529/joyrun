package manifest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wxia529/joyrun/internal/fault"
)

func TestEntrySelectionUploadsOnlyEntryAndExplicitIncludes(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"job.inp":   "input",
		"coord.xyz": "coordinates",
		"other.inp": "wrong task",
		"old.gbw":   "old result",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	staging, entries, ignored, cleanup, err := Snapshot(root, Selection{
		Mode: "entry", Entry: "job.inp", Include: []string{"*.xyz"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(entries) != 2 || entries[0].Path != "coord.xyz" || entries[1].Path != "job.inp" {
		t.Fatalf("unexpected selected entries: %#v", entries)
	}
	if _, err := os.Stat(filepath.Join(staging, "other.inp")); !os.IsNotExist(err) {
		t.Fatalf("unselected input reached snapshot: %v", err)
	}
	if len(ignored) != 2 {
		t.Fatalf("expected two unselected files in preview, got %#v", ignored)
	}
}

func TestEntrySelectionRejectsExcludedInputAndLimits(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "job.inp"), []byte("input"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Build(root, Selection{
		Mode: "entry", Entry: "job.inp", Exclude: []string{"*.inp"},
	}); fault.As(err).Code != "SOURCE_ENTRY_EXCLUDED" {
		t.Fatalf("expected excluded entry rejection, got %v", err)
	}
	if _, _, err := Build(root, Selection{
		Mode: "entry", Entry: "job.inp", MaxTotalBytes: 1,
	}); fault.As(err).Code != "UPLOAD_POLICY_EXCEEDED" {
		t.Fatalf("expected size limit rejection, got %v", err)
	}
}

func TestBuiltInMetadataDirectoriesAreExcludedAtAnyDepth(t *testing.T) {
	for _, candidate := range []string{".git/config", "nested/.git/config", ".joyrun/project.yaml"} {
		if !Excluded(candidate, false, []string{".git/", ".joyrun/"}) {
			t.Fatalf("expected %q to be excluded", candidate)
		}
	}
}

func TestSnapshotDereferencesInternalFileSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "input.dat"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("input.dat", filepath.Join(root, "alias.dat")); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	staging, entries, _, cleanup, err := Snapshot(root, Selection{Mode: "workdir"})
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
	if _, _, _, cleanup, err := Snapshot(root, Selection{Mode: "workdir"}); err == nil {
		cleanup()
		t.Fatal("expected external symbolic link to be rejected")
	}
}
