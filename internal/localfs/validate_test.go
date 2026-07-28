package localfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsPullPathValidation(t *testing.T) {
	for _, files := range [][]string{
		{"CON"},
		{"results/NUL.txt"},
		{"bad:name.out"},
		{"trailing. "},
		{"A.out", "a.out"},
		{"../escape"},
	} {
		if err := validatePullPaths(files, "windows"); err == nil {
			t.Fatalf("expected Windows paths to be rejected: %#v", files)
		}
	}
	if err := validatePullPaths([]string{"结果/output 1.txt", "nested/A.out"}, "windows"); err != nil {
		t.Fatalf("valid Windows paths rejected: %v", err)
	}
}

func TestUnixAllowsCaseDistinctPaths(t *testing.T) {
	if err := validatePullPaths([]string{"A.out", "a.out", "name:result"}, "linux"); err != nil {
		t.Fatal(err)
	}
}

func TestPullDestinationRejectsSymlinkParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "results")); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if err := ValidatePullDestination(root, []string{"results/output.dat"}); err == nil {
		t.Fatal("expected symbolic-link parent to be rejected")
	}
}
