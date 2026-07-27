package template

import (
	"strings"
	"testing"

	"github.com/wxia529/joyrun/internal/model"
)

func TestRenderShellQuotesBuiltinsAndStringParams(t *testing.T) {
	entry := "bad name'$(touch nope).inp"
	source := model.Source{Entry: &entry}
	target := model.Target{Script: "run {{ .Input }} --label={{ .Params.label }} > {{ .Stem }}.out"}
	data := Values(source, "jr_test", "/remote/work dir", "task name", map[string]any{
		"label": "x; echo injected",
	})
	rendered, err := Render(target, data)
	if err != nil {
		t.Fatal(err)
	}
	expected := "run 'bad name'\"'\"'$(touch nope).inp' --label='x; echo injected' > 'bad name'\"'\"'$(touch nope)'.out"
	if rendered != expected {
		t.Fatalf("unexpected shell-safe rendering:\nwant: %s\n got: %s", expected, rendered)
	}
	if strings.Contains(rendered, "--label=x;") {
		t.Fatalf("parameter was rendered as executable shell syntax: %s", rendered)
	}
}

func TestRenderStringKeepsPathValuesRaw(t *testing.T) {
	entry := "bad name.inp"
	data := Values(model.Source{Entry: &entry}, "jr_test", "/remote/work dir", "task name", nil)
	rendered, err := RenderString("{{ .Stem }}.out", data)
	if err != nil {
		t.Fatal(err)
	}
	if rendered != "bad name.out" {
		t.Fatalf("log path should not contain shell quoting: %q", rendered)
	}
}

func TestRenderRejectsLineBreaks(t *testing.T) {
	entry := "job.inp"
	data := Values(model.Source{Entry: &entry}, "jr_test", "/remote/work", "task", map[string]any{
		"partition": "normal\nmalicious-command",
	})
	_, err := Render(model.Target{Script: "#SBATCH -p {{ .Params.partition }}"}, data)
	if err == nil {
		t.Fatal("expected line-breaking parameter to be rejected")
	}
}
