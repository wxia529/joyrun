package localfs

import "testing"

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
