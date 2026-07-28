package config

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/model"
	jtemplate "github.com/wxia529/joyrun/internal/template"
	"gopkg.in/yaml.v3"
)

var paramName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
var byteSize = regexp.MustCompile(`(?i)^([1-9][0-9]*)(B|KB|MB|GB|TB|KIB|MIB|GIB|TIB)?$`)

const starter = `# JoyRun user configuration.
# Add clusters and execution targets, then run:
#   joyrun config validate
#   joyrun doctor <target>
#
# Example:
# clusters:
#   mycluster:
#     host: mycluster
#     scheduler: slurm
#     remote_root: /scratch/your-user/joyrun
#     transfer: auto
#
# targets:
#   mycluster/program:
#     cluster: mycluster
#     source:
#       kind: file
#       patterns: ["*.inp"]
#     push:
#       mode: entry
#       limits:
#         max_files: 20
#         max_total_size: 2GiB
#     status:
#       partition: community
#     script: |
#       #!/bin/bash
#       #SBATCH --job-name={{ .Stem }}
#       program {{ .Input }} > {{ .Stem }}.out
#     pull:
#       default: ["*.out"]
version: 1

clusters: {}
targets: {}
`

func Init(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fault.Wrap("CONFIG_INIT_FAILED", "cannot create JoyRun configuration directory", false, err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fault.New("CONFIG_EXISTS", fmt.Sprintf("configuration already exists at %s", path), false).
			WithAction("edit the existing file or select another path with --config")
	}
	if err != nil {
		return fault.Wrap("CONFIG_INIT_FAILED", fmt.Sprintf("cannot create configuration %s", path), false, err)
	}
	if _, err := file.WriteString(starter); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fault.Wrap("CONFIG_INIT_FAILED", "cannot write starter configuration", false, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fault.Wrap("CONFIG_INIT_FAILED", "cannot finalize starter configuration", false, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return fault.Wrap("CONFIG_INIT_FAILED", "cannot close starter configuration", false, err)
	}
	return nil
}

func Load(path string) (model.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return model.Config{}, fault.Wrap("CONFIG_NOT_FOUND", fmt.Sprintf("cannot read config %s", path), false, err).
			WithAction("run `joyrun config init` or select a configuration with --config")
	}
	var cfg model.Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return model.Config{}, fault.Wrap("CONFIG_INVALID", "invalid JoyRun configuration", false, err)
	}
	if cfg.Version != 1 {
		return model.Config{}, fault.New("CONFIG_INVALID", "config version must be 1", false)
	}
	if cfg.Clusters == nil {
		cfg.Clusters = map[string]model.Cluster{}
	}
	if cfg.Targets == nil {
		cfg.Targets = map[string]model.Target{}
	}
	for name, cluster := range cfg.Clusters {
		if cluster.Host == "" || cluster.RemoteRoot == "" {
			return model.Config{}, fault.New("CONFIG_INVALID", fmt.Sprintf("cluster %q requires host and remote_root", name), false)
		}
		if !strings.HasPrefix(cluster.RemoteRoot, "/") {
			return model.Config{}, fault.New("CONFIG_INVALID", fmt.Sprintf("cluster %q remote_root must be an absolute POSIX path", name), false)
		}
		if cluster.Scheduler != "slurm" {
			return model.Config{}, fault.New("CONFIG_INVALID", fmt.Sprintf("cluster %q: v0.1 only supports scheduler slurm", name), false)
		}
		if cluster.Transfer == "" {
			cluster.Transfer = "auto"
		}
		switch cluster.Transfer {
		case "auto", "rsync", "sftp":
		default:
			return model.Config{}, fault.New("CONFIG_INVALID", fmt.Sprintf("cluster %q has unsupported transfer %q", name, cluster.Transfer), false)
		}
		cfg.Clusters[name] = cluster
	}
	for name, target := range cfg.Targets {
		if _, ok := cfg.Clusters[target.Cluster]; !ok {
			return model.Config{}, fault.New("CONFIG_INVALID", fmt.Sprintf("target %q refers to unknown cluster %q", name, target.Cluster), false)
		}
		if target.Script == "" {
			return model.Config{}, fault.New("CONFIG_INVALID", fmt.Sprintf("target %q requires script", name), false)
		}
		for key, spec := range target.Params {
			if !paramName.MatchString(key) {
				return model.Config{}, fault.New("CONFIG_INVALID", fmt.Sprintf("parameter %q must use lowercase_snake_case", key), false)
			}
			if err := validateSpec(key, spec); err != nil {
				return model.Config{}, err
			}
		}
		if err := jtemplate.Validate(target.Script, target.Params); err != nil {
			return model.Config{}, fault.Wrap("CONFIG_INVALID", fmt.Sprintf("target %q has an invalid script", name), false, err)
		}
		if target.Status.Partition != "" {
			if strings.TrimSpace(target.Status.Partition) == "" {
				return model.Config{}, fault.New("CONFIG_INVALID",
					fmt.Sprintf("target %q status.partition cannot be blank", name), false)
			}
			if err := jtemplate.ValidateParamsOnly(target.Status.Partition, target.Params); err != nil {
				return model.Config{}, fault.Wrap("CONFIG_INVALID",
					fmt.Sprintf("target %q has an invalid status.partition template", name), false, err)
			}
		}
		for _, logPath := range target.Logs {
			if err := jtemplate.Validate(logPath, target.Params); err != nil {
				return model.Config{}, fault.Wrap("CONFIG_INVALID", fmt.Sprintf("target %q has an invalid log template", name), false, err)
			}
		}
		usesEntry := jtemplate.UsesEntry(target.Script)
		for _, logPath := range target.Logs {
			usesEntry = usesEntry || jtemplate.UsesEntry(logPath)
		}
		if target.Source.Kind == "" {
			return model.Config{}, fault.New("CONFIG_INVALID",
				fmt.Sprintf("target %q requires an explicit source.kind (file, directory, or either)", name), false)
		}
		switch target.Source.Kind {
		case "file", "directory", "either":
		default:
			return model.Config{}, fault.New("CONFIG_INVALID",
				fmt.Sprintf("target %q source.kind must be file, directory, or either", name), false)
		}
		if usesEntry && target.Source.Kind != "file" {
			return model.Config{}, fault.New("CONFIG_INVALID",
				fmt.Sprintf("target %q uses .Input or .Stem and therefore requires source.kind file", name), false)
		}
		if target.Source.Kind == "directory" && len(target.Source.Patterns) > 0 {
			return model.Config{}, fault.New("CONFIG_INVALID",
				fmt.Sprintf("target %q cannot use source.patterns with source.kind directory", name), false)
		}
		for _, pattern := range target.Source.Patterns {
			if pattern == "" {
				return model.Config{}, fault.New("CONFIG_INVALID",
					fmt.Sprintf("target %q has an empty source pattern", name), false)
			}
			if _, err := pathpkg.Match(pattern, "entry"); err != nil {
				return model.Config{}, fault.Wrap("CONFIG_INVALID",
					fmt.Sprintf("target %q has invalid source pattern %q", name, pattern), false, err)
			}
		}
		switch target.Push.Mode {
		case "entry":
			if target.Source.Kind != "file" {
				return model.Config{}, fault.New("CONFIG_INVALID",
					fmt.Sprintf("target %q push.mode entry requires source.kind file", name), false)
			}
		case "workdir":
		default:
			return model.Config{}, fault.New("CONFIG_INVALID",
				fmt.Sprintf("target %q requires push.mode entry or workdir", name), false)
		}
		for _, pattern := range append(append([]string{}, target.Push.Include...), target.Push.Exclude...) {
			if pattern == "" {
				return model.Config{}, fault.New("CONFIG_INVALID",
					fmt.Sprintf("target %q has an empty push pattern", name), false)
			}
			trimmed := strings.TrimSuffix(strings.TrimPrefix(filepath.ToSlash(pattern), "/"), "/")
			if _, err := pathpkg.Match(trimmed, "entry"); err != nil {
				return model.Config{}, fault.Wrap("CONFIG_INVALID",
					fmt.Sprintf("target %q has invalid push pattern %q", name, pattern), false, err)
			}
		}
		if target.Push.Limits.MaxFiles < 0 {
			return model.Config{}, fault.New("CONFIG_INVALID",
				fmt.Sprintf("target %q push.limits.max_files cannot be negative", name), false)
		}
		if target.Push.Limits.MaxTotalSize != "" {
			if _, err := ParseByteSize(target.Push.Limits.MaxTotalSize); err != nil {
				return model.Config{}, fault.Wrap("CONFIG_INVALID",
					fmt.Sprintf("target %q has invalid push.limits.max_total_size", name), false, err)
			}
		}
		cfg.Targets[name] = target
	}
	return cfg, nil
}

func ParseByteSize(raw string) (int64, error) {
	match := byteSize.FindStringSubmatch(strings.TrimSpace(raw))
	if match == nil {
		return 0, fmt.Errorf("size %q must be a positive integer followed by B, KB, MB, GB, TB, KiB, MiB, GiB, or TiB", raw)
	}
	value, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, err
	}
	multipliers := map[string]int64{
		"": 1, "B": 1,
		"KB": 1000, "MB": 1000 * 1000, "GB": 1000 * 1000 * 1000, "TB": 1000 * 1000 * 1000 * 1000,
		"KIB": 1 << 10, "MIB": 1 << 20, "GIB": 1 << 30, "TIB": 1 << 40,
	}
	multiplier := multipliers[strings.ToUpper(match[2])]
	if value > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("size %q exceeds the supported range", raw)
	}
	return value * multiplier, nil
}

func validateSpec(name string, spec model.ParamSpec) error {
	switch spec.Type {
	case "string", "int", "float", "bool":
	default:
		return fault.New("CONFIG_INVALID", fmt.Sprintf("parameter %q has unsupported type %q", name, spec.Type), false)
	}
	if spec.Default == nil && !spec.Required {
		return fault.New("CONFIG_INVALID", fmt.Sprintf("parameter %q requires default or required: true", name), false)
	}
	if spec.Default != nil {
		value, err := convert(spec.Type, fmt.Sprint(spec.Default))
		if err != nil {
			return fault.Wrap("CONFIG_INVALID", fmt.Sprintf("parameter %q has invalid default", name), false, err)
		}
		if len(spec.Choices) > 0 && !choiceAllowed(value, spec.Choices, spec.Type) {
			return fault.New("CONFIG_INVALID", fmt.Sprintf("parameter %q default is not in choices", name), false)
		}
	}
	for _, choice := range spec.Choices {
		if _, err := convert(spec.Type, fmt.Sprint(choice)); err != nil {
			return fault.Wrap("CONFIG_INVALID", fmt.Sprintf("parameter %q has an invalid choice", name), false, err)
		}
	}
	return nil
}

func ResolveParams(target model.Target, sets []string) (map[string]any, map[string]string, error) {
	values := make(map[string]any, len(target.Params))
	sources := make(map[string]string, len(target.Params))
	for name, spec := range target.Params {
		if spec.Default != nil {
			value, err := convert(spec.Type, fmt.Sprint(spec.Default))
			if err != nil {
				return nil, nil, err
			}
			values[name], sources[name] = value, "target_default"
		}
	}
	for _, item := range sets {
		key, raw, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			return nil, nil, fault.New("INVALID_PARAMETER", fmt.Sprintf("--set %q must be key=value", item), false)
		}
		spec, ok := target.Params[key]
		if !ok {
			return nil, nil, fault.New("UNKNOWN_PARAMETER", fmt.Sprintf("target does not declare parameter %q", key), false)
		}
		value, err := convert(spec.Type, raw)
		if err != nil {
			return nil, nil, fault.Wrap("INVALID_PARAMETER", fmt.Sprintf("invalid value for parameter %q", key), false, err)
		}
		if len(spec.Choices) > 0 && !choiceAllowed(value, spec.Choices, spec.Type) {
			return nil, nil, fault.New("INVALID_PARAMETER", fmt.Sprintf("value for parameter %q is not in choices", key), false)
		}
		values[key], sources[key] = value, "cli"
	}
	for name, spec := range target.Params {
		if spec.Required {
			if _, ok := values[name]; !ok {
				return nil, nil, fault.New("MISSING_PARAMETER", fmt.Sprintf("required parameter %q is missing", name), false)
			}
		}
	}
	return values, sources, nil
}

func convert(kind, raw string) (any, error) {
	switch kind {
	case "string":
		return raw, nil
	case "int":
		return strconv.ParseInt(raw, 10, 64)
	case "float":
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, err
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("float must be finite")
		}
		return value, nil
	case "bool":
		return strconv.ParseBool(raw)
	default:
		return nil, fmt.Errorf("unsupported type %q", kind)
	}
}

func choiceAllowed(value any, choices []any, kind string) bool {
	for _, choice := range choices {
		converted, err := convert(kind, fmt.Sprint(choice))
		if err == nil && fmt.Sprint(converted) == fmt.Sprint(value) {
			return true
		}
	}
	return false
}

func TargetNames(cfg model.Config) []string {
	names := make([]string, 0, len(cfg.Targets))
	for name := range cfg.Targets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
