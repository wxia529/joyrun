package config

import (
	"os"
	"path/filepath"
	"runtime"
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
	if info, err := os.Stat(path); err != nil ||
		(runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
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
    partitions:
      community: {cores_per_node: 32, memory_per_node: 240GiB}
targets:
  gibbs/orca:
    cluster: gibbs
    software: {name: orca, version: "6.1.1"}
    placement:
      default_partition: community
      allowed_partitions: [community]
    source:
      kind: file
      patterns: ["*.inp"]
    push: {mode: entry}
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
      #SBATCH -p {{ .Partition.Name }}
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
	if cfg.Targets["gibbs/orca"].Placement.DefaultPartition != "community" {
		t.Fatalf("unexpected target placement: %#v", cfg.Targets["gibbs/orca"].Placement)
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
	partition, source, err := ResolvePartition(
		cfg.Clusters["gibbs"], cfg.Targets["gibbs/orca"], "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if partition.Name != "community" || partition.CoresPerNode != 32 ||
		partition.MemoryPerNode != "240GiB" || source != "target_default" {
		t.Fatalf("unexpected partition resolution: %#v source=%q", partition, source)
	}
}

func TestLoadRejectsUnknownAllowedPartition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `version: 1
clusters:
  c: {host: c, scheduler: slurm, remote_root: /tmp/joyrun, partitions: {p: {cores_per_node: 1}}}
targets:
  c/run:
    cluster: c
    software: {name: run}
    placement: {default_partition: missing, allowed_partitions: [missing]}
    source: {kind: file}
    push: {mode: entry}
    script: "run {{ .Input }}"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown placement partition to be rejected")
	}
}

func TestLoadRejectsUnsafePartitionName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `version: 1
clusters:
  c:
    host: c
    scheduler: slurm
    remote_root: /tmp/joyrun
    partitions:
      "bad partition": {cores_per_node: 1}
targets: {}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unsafe partition name to be rejected")
	}
}

func TestLoadRejectsPartitionAsTargetParameter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `version: 1
clusters:
  c: {host: c, scheduler: slurm, remote_root: /tmp/joyrun, partitions: {p: {}}}
targets:
  c/run:
    cluster: c
    software: {name: run}
    placement: {default_partition: p, allowed_partitions: [p]}
    source: {kind: file}
    push: {mode: entry}
    params:
      partition: {type: string, default: p}
    script: "run {{ .Input }}"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected partition target parameter to be rejected")
	}
}

func TestLoadRequiresExplicitSourceContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `version: 1
clusters:
  c: {host: c, scheduler: slurm, remote_root: /tmp/joyrun, partitions: {p: {cores_per_node: 1}}}
targets:
  c/run:
    cluster: c
    software: {name: run}
    placement: {default_partition: p, allowed_partitions: [p]}
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
  c: {host: c, scheduler: slurm, remote_root: /tmp/joyrun, partitions: {p: {cores_per_node: 1}}}
targets:
  c/run:
    cluster: c
    software: {name: run}
    placement: {default_partition: p, allowed_partitions: [p]}
    source: {kind: directory}
    push: {mode: workdir}
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
  c: {host: c, scheduler: slurm, remote_root: /tmp/joyrun, partitions: {p: {cores_per_node: 1}}}
targets:
  c/run:
    cluster: c
    software: {name: run}
    placement: {default_partition: p, allowed_partitions: [p]}
    source: {kind: either}
    push: {mode: workdir}
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
  c: {host: c, scheduler: slurm, remote_root: /tmp/joyrun, partitions: {p: {cores_per_node: 1}}}
targets:
  c/run:
    cluster: c
    software: {name: run}
    placement: {default_partition: p, allowed_partitions: [p]}
    source: {kind: file}
    push: {mode: entry}
    script: '{{ if .Input }}run{{ end }}'
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected control-flow template to be rejected")
	}
}

func TestLoadRequiresSafePushMode(t *testing.T) {
	tests := []struct {
		name string
		push string
	}{
		{name: "missing", push: ""},
		{name: "entry with directory source", push: "push: {mode: entry}"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			content := `version: 1
clusters:
  c: {host: c, scheduler: slurm, remote_root: /tmp/joyrun, partitions: {p: {cores_per_node: 1}}}
targets:
  c/run:
    cluster: c
    software: {name: run}
    placement: {default_partition: p, allowed_partitions: [p]}
    source: {kind: directory}
    ` + test.push + `
    script: "true"
`
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("expected unsafe push policy %q to be rejected", test.name)
			}
		})
	}
}

func TestParseByteSize(t *testing.T) {
	tests := map[string]int64{
		"2GB":    2_000_000_000,
		"2GiB":   2 * (1 << 30),
		"2G":     2 * (1 << 30),
		"512MiB": 512 * (1 << 20),
	}
	for raw, expected := range tests {
		if got, err := ParseByteSize(raw); err != nil || got != expected {
			t.Fatalf("ParseByteSize(%q) = %d, %v; want %d", raw, got, err, expected)
		}
	}
	for _, raw := range []string{"", "0GB", "-1GB", "1.5GB", "huge"} {
		if _, err := ParseByteSize(raw); err == nil {
			t.Fatalf("expected invalid size %q to fail", raw)
		}
	}
}
