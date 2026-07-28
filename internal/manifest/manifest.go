package manifest

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/model"
)

type Selection struct {
	Mode          string
	Entry         string
	Include       []string
	Exclude       []string
	MaxFiles      int
	MaxTotalBytes int64
}

func Build(root string, selection Selection) ([]model.ManifestEntry, []string, error) {
	if err := validateSelection(selection); err != nil {
		return nil, nil, err
	}
	var entries []model.ManifestEntry
	var ignored []string
	var totalBytes int64
	rootAbsolute, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, fault.Wrap("SOURCE_SCAN_FAILED", "cannot resolve manifest root", false, err)
	}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if matches(rel, entry.IsDir(), selection.Exclude) {
			ignored = append(ignored, rel)
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !selected(rel, selection) {
			ignored = append(ignored, rel)
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := validateFileSymlink(rootAbsolute, path, rel)
			if err != nil {
				return err
			}
			info, err = os.Stat(target)
			if err != nil {
				return err
			}
		} else if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported special file %s (%s)", rel, info.Mode().Type())
		}
		if err := enforceLimits(selection, len(entries)+1, totalBytes+info.Size()); err != nil {
			return err
		}
		hash, err := hashFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, model.ManifestEntry{Path: rel, Size: info.Size(), SHA256: hash})
		totalBytes += info.Size()
		return nil
	})
	if err != nil {
		if code := fault.As(err).Code; code == "UPLOAD_POLICY_EXCEEDED" {
			return nil, nil, err
		}
		return nil, nil, fault.Wrap("SOURCE_SCAN_FAILED", "cannot build input manifest", false, err)
	}
	return entries, ignored, nil
}

// Snapshot copies the included source files into an immutable staging directory
// and computes the manifest from the bytes written there. The returned cleanup
// function must be called by the caller.
func Snapshot(root string, selection Selection) (string, []model.ManifestEntry, []string, func(), error) {
	if err := validateSelection(selection); err != nil {
		return "", nil, nil, func() {}, err
	}
	staging, err := os.MkdirTemp("", "joyrun-snapshot-*")
	if err != nil {
		return "", nil, nil, func() {}, fault.Wrap("SOURCE_SNAPSHOT_FAILED", "cannot create input snapshot", false, err)
	}
	cleanup := func() { _ = os.RemoveAll(staging) }
	var entries []model.ManifestEntry
	var ignored []string
	var totalBytes int64
	rootAbsolute, err := filepath.Abs(root)
	if err != nil {
		cleanup()
		return "", nil, nil, func() {}, fault.Wrap("SOURCE_SNAPSHOT_FAILED", "cannot resolve snapshot root", false, err)
	}
	err = filepath.WalkDir(root, func(sourcePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if sourcePath == root {
			return nil
		}
		rel, err := filepath.Rel(root, sourcePath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if matches(rel, entry.IsDir(), selection.Exclude) {
			ignored = append(ignored, rel)
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !selected(rel, selection) {
			ignored = append(ignored, rel)
			return nil
		}
		destination := filepath.Join(staging, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := validateFileSymlink(rootAbsolute, sourcePath, rel)
			if err != nil {
				return err
			}
			targetInfo, err := os.Stat(target)
			if err != nil {
				return err
			}
			if !targetInfo.Mode().IsRegular() {
				return fmt.Errorf("symbolic link %s must point to a regular file", rel)
			}
			if err := enforceLimits(selection, len(entries)+1, totalBytes+targetInfo.Size()); err != nil {
				return err
			}
			size, hash, err := copyAndHash(target, destination, targetInfo.Mode().Perm())
			if err != nil {
				return err
			}
			entries = append(entries, model.ManifestEntry{Path: rel, Size: size, SHA256: hash})
			totalBytes += size
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported special file %s (%s)", rel, info.Mode().Type())
		}
		if err := enforceLimits(selection, len(entries)+1, totalBytes+info.Size()); err != nil {
			return err
		}
		size, hash, err := copyAndHash(sourcePath, destination, info.Mode().Perm())
		if err != nil {
			return err
		}
		entries = append(entries, model.ManifestEntry{Path: rel, Size: size, SHA256: hash})
		totalBytes += size
		return nil
	})
	if err != nil {
		cleanup()
		if code := fault.As(err).Code; code == "UPLOAD_POLICY_EXCEEDED" {
			return "", nil, nil, func() {}, err
		}
		return "", nil, nil, func() {}, fault.Wrap("SOURCE_SNAPSHOT_FAILED", "cannot create input snapshot", false, err)
	}
	return staging, entries, ignored, cleanup, nil
}

func validateSelection(selection Selection) error {
	if selection.Mode != "entry" {
		return nil
	}
	if selection.Entry == "" {
		return fault.New("SOURCE_ENTRY_REQUIRED", "entry upload mode requires a concrete source file", false)
	}
	if matches(selection.Entry, false, selection.Exclude) {
		return fault.New("SOURCE_ENTRY_EXCLUDED",
			fmt.Sprintf("selected input %q is excluded by the upload policy", selection.Entry), false).
			WithAction("remove the matching push.exclude or .joyrunignore rule")
	}
	return nil
}

func selected(rel string, selection Selection) bool {
	if selection.Mode == "workdir" {
		return true
	}
	return rel == selection.Entry || matches(rel, false, selection.Include)
}

func enforceLimits(selection Selection, files int, totalBytes int64) error {
	if selection.MaxFiles > 0 && files > selection.MaxFiles {
		return fault.New("UPLOAD_POLICY_EXCEEDED",
			fmt.Sprintf("upload snapshot selects at least %d files, exceeding max_files=%d",
				files, selection.MaxFiles), false).
			WithAction("review `joyrun submit ... --dry-run` and adjust target push.limits.max_files")
	}
	if selection.MaxTotalBytes > 0 && totalBytes > selection.MaxTotalBytes {
		return fault.New("UPLOAD_POLICY_EXCEEDED",
			fmt.Sprintf("upload snapshot selects at least %d bytes, exceeding max_total_size=%d bytes",
				totalBytes, selection.MaxTotalBytes), false).
			WithAction("review `joyrun submit ... --dry-run` and adjust target push.limits.max_total_size")
	}
	return nil
}

func validateFileSymlink(rootAbsolute, sourcePath, rel string) (string, error) {
	target, err := filepath.EvalSymlinks(sourcePath)
	if err != nil {
		return "", err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", err
	}
	targetRelative, err := filepath.Rel(rootAbsolute, target)
	if err != nil || targetRelative == ".." ||
		strings.HasPrefix(targetRelative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("symbolic link %s points outside the source work directory", rel)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	if !targetInfo.Mode().IsRegular() {
		return "", fmt.Errorf("symbolic link %s must point to a regular file", rel)
	}
	return target, nil
}

func ExcludePatterns(projectRoot, workDir string, targetPatterns []string) []string {
	result := []string{".joyrun/", ".git/"}
	result = append(result, targetPatterns...)
	return append(result, projectIgnorePatterns(projectRoot, workDir)...)
}

func projectIgnorePatterns(projectRoot, workDir string) []string {
	workRelative, err := filepath.Rel(projectRoot, workDir)
	if err != nil {
		return nil
	}
	workRelative = filepath.ToSlash(filepath.Clean(workRelative))
	if workRelative == "." {
		workRelative = ""
	}
	var result []string
	for _, raw := range readIgnore(filepath.Join(projectRoot, ".joyrunignore")) {
		pattern := strings.TrimPrefix(filepath.ToSlash(raw), "/")
		if workRelative == "" || !strings.Contains(pattern, "/") {
			result = append(result, pattern)
			continue
		}
		prefix := strings.TrimSuffix(workRelative, "/") + "/"
		if strings.HasPrefix(pattern, prefix) {
			result = append(result, strings.TrimPrefix(pattern, prefix))
		} else if strings.TrimSuffix(pattern, "/") == workRelative {
			result = append(result, "*")
		}
	}
	return result
}

func readIgnore(path string) []string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	var patterns []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			patterns = append(patterns, line)
		}
	}
	return patterns
}

func matches(path string, isDir bool, patterns []string) bool {
	base := filepath.Base(path)
	for _, raw := range patterns {
		pattern := filepath.ToSlash(strings.TrimSpace(raw))
		pattern = strings.TrimPrefix(pattern, "/")
		if strings.HasSuffix(pattern, "/") {
			prefix := strings.TrimSuffix(pattern, "/")
			if path == prefix || strings.HasPrefix(path, prefix+"/") ||
				(!strings.Contains(prefix, "/") && containsPathSegment(path, prefix)) {
				return true
			}
			continue
		}
		if ok, _ := pathpkg.Match(pattern, path); ok {
			return true
		}
		if !strings.Contains(pattern, "/") {
			if ok, _ := pathpkg.Match(pattern, base); ok {
				return true
			}
		}
		if isDir && (path == pattern || strings.HasPrefix(path, pattern+"/")) {
			return true
		}
	}
	return false
}

func containsPathSegment(value, segment string) bool {
	for _, candidate := range strings.Split(filepath.ToSlash(value), "/") {
		if candidate == segment {
			return true
		}
	}
	return false
}

func Match(path string, patterns []string) bool {
	return matches(filepath.ToSlash(path), false, patterns)
}

func Excluded(path string, isDir bool, patterns []string) bool {
	return matches(filepath.ToSlash(path), isDir, patterns)
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := file.WriteTo(hash); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyAndHash(source, destination string, mode os.FileMode) (int64, string, error) {
	input, err := os.Open(source)
	if err != nil {
		return 0, "", err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return 0, "", err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(output, hash), input)
	closeErr := output.Close()
	if copyErr != nil {
		return 0, "", copyErr
	}
	if closeErr != nil {
		return 0, "", closeErr
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}
