package cli

import (
	"reflect"
	"testing"
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
