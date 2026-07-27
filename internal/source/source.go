package source

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/model"
)

func Resolve(project model.Project, path string) (model.Source, string, error) {
	absolute := path
	if !filepath.IsAbs(absolute) {
		var err error
		absolute, err = filepath.Abs(path)
		if err != nil {
			return model.Source{}, "", fault.Wrap("SOURCE_NOT_FOUND", "cannot resolve source path", false, err)
		}
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return model.Source{}, "", fault.Wrap("SOURCE_NOT_FOUND", "source path does not exist", false, err)
	}
	relative, err := filepath.Rel(project.Root, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return model.Source{}, "", fault.New("SOURCE_OUTSIDE_PROJECT", "source must be inside the JoyRun project", false)
	}
	relative = filepath.ToSlash(filepath.Clean(relative))
	var workDir string
	var entry *string
	workDirAbsolute := absolute
	if info.IsDir() {
		workDir = relative
	} else {
		workDir = filepath.ToSlash(filepath.Dir(relative))
		workDirAbsolute = filepath.Dir(absolute)
		name := filepath.Base(relative)
		entry = &name
	}
	if workDir == "." {
		workDir = ""
	}
	return model.Source{
		ProjectID:    project.ProjectID,
		RelativePath: relative,
		WorkDir:      workDir,
		Entry:        entry,
	}, workDirAbsolute, nil
}
