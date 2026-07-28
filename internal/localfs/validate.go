package localfs

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/wxia529/joyrun/internal/fault"
)

func ValidatePullPaths(files []string) error {
	return validatePullPaths(files, runtime.GOOS)
}

func ValidatePullDestination(root string, files []string) error {
	if err := ValidatePullPaths(files); err != nil {
		return err
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return fault.Wrap("LOCAL_PATH_UNSUPPORTED", "cannot resolve pull destination", false, err)
	}
	if info, err := os.Lstat(absolute); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fault.New("LOCAL_PATH_UNSUPPORTED",
				fmt.Sprintf("pull destination %q is a symbolic link", absolute), false)
		}
		if !info.IsDir() {
			return fault.New("LOCAL_PATH_UNSUPPORTED",
				fmt.Sprintf("pull destination %q is not a directory", absolute), false)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fault.Wrap("LOCAL_PATH_UNSUPPORTED", "cannot inspect pull destination", false, err)
	}
	for _, file := range files {
		current := absolute
		components := strings.Split(filepath.FromSlash(file), string(filepath.Separator))
		for _, component := range components[:len(components)-1] {
			current = filepath.Join(current, component)
			info, err := os.Lstat(current)
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil {
				return fault.Wrap("LOCAL_PATH_UNSUPPORTED",
					"cannot inspect pull destination "+current, false, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fault.New("LOCAL_PATH_UNSUPPORTED",
					fmt.Sprintf("pull destination parent %q is a symbolic link", current), false)
			}
			if !info.IsDir() {
				return fault.New("LOCAL_PATH_UNSUPPORTED",
					fmt.Sprintf("pull destination parent %q is not a directory", current), false)
			}
		}
	}
	return nil
}

func validatePullPaths(files []string, goos string) error {
	seen := make(map[string]string, len(files))
	for _, file := range files {
		cleaned := path.Clean(file)
		if file == "" || !utf8.ValidString(file) || strings.ContainsRune(file, '\x00') || path.IsAbs(file) ||
			cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned != file {
			return fault.New("LOCAL_PATH_UNSUPPORTED", fmt.Sprintf("remote file path %q is not a safe relative path", file), false)
		}
		key := cleaned
		if goos == "windows" {
			if err := validateWindowsPath(cleaned); err != nil {
				return fault.New("LOCAL_PATH_UNSUPPORTED", err.Error(), false)
			}
			key = strings.ToLower(cleaned)
		}
		if previous, ok := seen[key]; ok && previous != cleaned {
			return fault.New("LOCAL_PATH_COLLISION", fmt.Sprintf("remote files %q and %q map to the same local path", previous, cleaned), false)
		}
		seen[key] = cleaned
	}
	return nil
}

func validateWindowsPath(value string) error {
	for _, component := range strings.Split(value, "/") {
		if component == "" || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return fmt.Errorf("remote file path %q cannot be represented safely on Windows", value)
		}
		original := component
		for len(component) > 0 {
			r, size := utf8.DecodeRuneInString(component)
			if r < 32 || strings.ContainsRune(`<>:"\|?*`, r) {
				return fmt.Errorf("remote file path %q contains a character unsupported by Windows", value)
			}
			component = component[size:]
		}
		name := strings.ToUpper(strings.SplitN(original, ".", 2)[0])
		if windowsReservedName(name) {
			return fmt.Errorf("remote file path %q uses a reserved Windows filename", value)
		}
	}
	return nil
}

func windowsReservedName(name string) bool {
	switch name {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(name) == 4 && (strings.HasPrefix(name, "COM") || strings.HasPrefix(name, "LPT")) {
		return name[3] >= '1' && name[3] <= '9'
	}
	return false
}
