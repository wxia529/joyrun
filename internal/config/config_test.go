package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndResolveParams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `version: 1
clusters:
  gibbs:
    host: gibbs
    scheduler: slurm
    remote_root: /scratch/user/joyrun
targets:
  gibbs/orca:
    cluster: gibbs
    params:
      cpus:
        type: int
        default: 32
      executable:
        type: string
        default: std
        choices: [std, gam]
    script: |
      #SBATCH -c {{ .Params.cpus }}
      orca_{{ .Params.executable }} {{ .Input }}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Clusters["gibbs"].Transfer != "auto" {
		t.Fatalf("expected default auto transfer, got %#v", cfg.Clusters["gibbs"])
	}
	values, sources, err := ResolveParams(cfg.Targets["gibbs/orca"], []string{"cpus=64"})
	if err != nil {
		t.Fatal(err)
	}
	if values["cpus"] != int64(64) || values["executable"] != "std" {
		t.Fatalf("unexpected values: %#v", values)
	}
	if sources["cpus"] != "cli" || sources["executable"] != "target_default" {
		t.Fatalf("unexpected sources: %#v", sources)
	}
}

func TestLoadRejectsUnknownTransferBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `version: 1
clusters:
  c: {host: c, scheduler: slurm, remote_root: /tmp/joyrun, transfer: magic}
targets: {}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown transfer backend to be rejected")
	}
}

func TestResolveParamsRejectsUnknownAndInvalidChoice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `version: 1
clusters:
  c: {host: c, scheduler: slurm, remote_root: /tmp/joyrun}
targets:
  c/run:
    cluster: c
    params:
      mode: {type: string, default: a, choices: [a, b]}
    script: "echo {{ .Params.mode }}"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveParams(cfg.Targets["c/run"], []string{"typo=x"}); err == nil {
		t.Fatal("expected unknown parameter error")
	}
	if _, _, err := ResolveParams(cfg.Targets["c/run"], []string{"mode=x"}); err == nil {
		t.Fatal("expected invalid choice error")
	}
}

func TestLoadRejectsTemplateControlFlow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `version: 1
clusters:
  c: {host: c, scheduler: slurm, remote_root: /tmp/joyrun}
targets:
  c/run:
    cluster: c
    script: '{{ if .Input }}run{{ end }}'
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected control-flow template to be rejected")
	}
}
