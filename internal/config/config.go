package config

import (
	"bytes"
	"fmt"
	"os"
	pathpkg "path"
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

func Load(path string) (model.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return model.Config{}, fault.Wrap("CONFIG_NOT_FOUND", fmt.Sprintf("cannot read config %s", path), false, err)
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
		cfg.Targets[name] = target
	}
	return cfg, nil
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
		return strconv.ParseFloat(raw, 64)
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
