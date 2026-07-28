package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wxia529/joyrun/internal/fault"
)

func TestInitCreatesValidStarterAndRefusesOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clusters) != 0 || len(cfg.Targets) != 0 {
		t.Fatalf("starter configuration is not empty: %#v", cfg)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected starter permissions: info=%v err=%v", info, err)
	}
	if err := Init(path); fault.As(err).Code != "CONFIG_EXISTS" {
		t.Fatalf("expected existing configuration rejection, got %v", err)
	}
}

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
    source:
      kind: file
      patterns: ["*.inp"]
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
	if cfg.Targets["gibbs/orca"].Source.Kind != "file" {
		t.Fatalf("unexpected source policy: %#v", cfg.Targets["gibbs/orca"].Source)
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

func TestLoadRequiresExplicitSourceContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `version: 1
clusters:
  c: {host: c, scheduler: slurm, remote_root: /tmp/joyrun}
targets:
  c/run:
    cluster: c
    script: "run {{ .Input }} > {{ .Stem }}.out"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected missing source.kind to be rejected")
	}
}

func TestLoadRejectsDirectoryTargetUsingEntryVariables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `version: 1
clusters:
  c: {host: c, scheduler: slurm, remote_root: /tmp/joyrun}
targets:
  c/run:
    cluster: c
    source: {kind: directory}
    script: "run {{ .Input }}"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected directory target using .Input to be rejected")
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
    source: {kind: either}
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

func TestFloatParamsMustBeFinite(t *testing.T) {
	for _, value := range []string{"NaN", "+Inf", "-Inf"} {
		if _, err := convert("float", value); err == nil {
			t.Fatalf("expected non-finite float %q to be rejected", value)
		}
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
