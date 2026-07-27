package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/identity"
	"github.com/wxia529/joyrun/internal/model"
	"gopkg.in/yaml.v3"
)

const metadataPath = ".joyrun/project.yaml"

func Init(root string) (model.Project, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return model.Project{}, fault.Wrap("PROJECT_INIT_FAILED", "cannot resolve project path", false, err)
	}
	if info, err := os.Stat(absolute); err != nil || !info.IsDir() {
		return model.Project{}, fault.New("PROJECT_NOT_FOUND", "project directory does not exist", false)
	}
	path := filepath.Join(absolute, metadataPath)
	if _, err := os.Stat(path); err == nil {
		return Load(absolute)
	} else if !errors.Is(err, os.ErrNotExist) {
		return model.Project{}, fault.Wrap("PROJECT_INIT_FAILED", "cannot inspect project metadata", false, err)
	}
	id, err := identity.New("pj_")
	if err != nil {
		return model.Project{}, fault.Wrap("PROJECT_INIT_FAILED", "cannot allocate project ID", false, err)
	}
	p := model.Project{Version: 1, ProjectID: id, Root: absolute}
	data, err := yaml.Marshal(p)
	if err != nil {
		return model.Project{}, fault.Wrap("PROJECT_INIT_FAILED", "cannot encode project metadata", false, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return model.Project{}, fault.Wrap("PROJECT_INIT_FAILED", "cannot create .joyrun directory", false, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return model.Project{}, fault.Wrap("PROJECT_INIT_FAILED", "cannot write project metadata", false, err)
	}
	return p, nil
}

func Discover(start string) (model.Project, error) {
	current, err := filepath.Abs(start)
	if err != nil {
		return model.Project{}, fault.Wrap("PROJECT_NOT_FOUND", "cannot resolve current directory", false, err)
	}
	if info, err := os.Stat(current); err == nil && !info.IsDir() {
		current = filepath.Dir(current)
	}
	for {
		path := filepath.Join(current, metadataPath)
		if _, err := os.Stat(path); err == nil {
			return Load(current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return model.Project{}, fault.New("PROJECT_NOT_FOUND", "no .joyrun/project.yaml found; run 'joyrun init'", false)
		}
		current = parent
	}
}

func Load(root string) (model.Project, error) {
	path := filepath.Join(root, metadataPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return model.Project{}, fault.Wrap("PROJECT_NOT_FOUND", "cannot read project metadata", false, err)
	}
	var p model.Project
	if err := yaml.Unmarshal(data, &p); err != nil {
		return model.Project{}, fault.Wrap("PROJECT_INVALID", "invalid project metadata", false, err)
	}
	if p.Version != 1 || p.ProjectID == "" {
		return model.Project{}, fault.New("PROJECT_INVALID", fmt.Sprintf("unsupported or incomplete project metadata at %s", path), false)
	}
	p.Root, err = filepath.Abs(root)
	if err != nil {
		return model.Project{}, err
	}
	return p, nil
}
