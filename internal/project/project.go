package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/identity"
	"github.com/wxia529/joyrun/internal/model"
	"gopkg.in/yaml.v3"
)

const metadataPath = ".joyrun/project.yaml"
const ignorePath = ".joyrun/.gitignore"

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
		project, err := Load(absolute)
		if err != nil {
			return model.Project{}, err
		}
		if err := ensureProjectIgnore(absolute); err != nil {
			return model.Project{}, err
		}
		return project, nil
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
	if err := writeNewFileAtomic(path, data, 0o644); err != nil {
		return model.Project{}, fault.Wrap("PROJECT_INIT_FAILED", "cannot write project metadata", false, err)
	}
	if err := ensureProjectIgnore(absolute); err != nil {
		return model.Project{}, err
	}
	return p, nil
}

func ensureProjectIgnore(root string) error {
	ignore := filepath.Join(root, ignorePath)
	data, err := os.ReadFile(ignore)
	if errors.Is(err, os.ErrNotExist) {
		if err := writeNewFileAtomic(ignore, []byte("project.yaml\n"), 0o644); err != nil {
			return fault.Wrap("PROJECT_INIT_FAILED", "cannot write .joyrun/.gitignore", false, err)
		}
		return nil
	}
	if err != nil {
		return fault.Wrap("PROJECT_INIT_FAILED", "cannot read .joyrun/.gitignore", false, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "project.yaml" {
			return nil
		}
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	data = append(data, []byte("project.yaml\n")...)
	if err := writeNewFileAtomic(ignore, data, 0o644); err != nil {
		return fault.Wrap("PROJECT_INIT_FAILED", "cannot update .joyrun/.gitignore", false, err)
	}
	return nil
}

func writeNewFileAtomic(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	keep := true
	defer func() {
		_ = file.Close()
		if keep {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	keep = false
	return nil
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
