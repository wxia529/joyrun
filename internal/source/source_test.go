package source

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/model"
)

func TestResolveFileAndRejectOutsideProject(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "task01")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(work, "job.inp")
	if err := os.WriteFile(input, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	project := model.Project{ProjectID: "pj_test", Root: root}
	source, gotWork, err := Resolve(project, input)
	if err != nil {
		t.Fatal(err)
	}
	if source.RelativePath != "task01/job.inp" || source.WorkDir != "task01" ||
		source.Entry == nil || *source.Entry != "job.inp" || gotWork != work {
		t.Fatalf("unexpected source: %#v, work=%q", source, gotWork)
	}
	outside := filepath.Join(t.TempDir(), "outside.inp")
	if err := os.WriteFile(outside, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Resolve(project, outside); fault.As(err).Code != "SOURCE_OUTSIDE_PROJECT" {
		t.Fatalf("expected outside source rejection, got %v", err)
	}
}
