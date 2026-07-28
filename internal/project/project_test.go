package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitKeepsProjectIdentityOutOfGit(t *testing.T) {
	root := t.TempDir()
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ignorePath))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "project.yaml\n" {
		t.Fatalf("unexpected .joyrun/.gitignore: %q", data)
	}
}

func TestInitAddsProjectIdentityToExistingIgnore(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, ".joyrun")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	ignore := filepath.Join(directory, ".gitignore")
	if err := os.WriteFile(ignore, []byte("cache/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(ignore)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "cache/\nproject.yaml\n" {
		t.Fatalf("unexpected merged .joyrun/.gitignore: %q", data)
	}
}
