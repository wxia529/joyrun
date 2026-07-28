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

func Build(root, projectRoot string, excludes []string) ([]model.ManifestEntry, []string, error) {
	patterns := []string{".joyrun/"}
	patterns = append(patterns, excludes...)
	patterns = append(patterns, projectIgnorePatterns(projectRoot, root)...)
	var entries []model.ManifestEntry
	var ignored []string
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
		if matches(rel, entry.IsDir(), patterns) {
			ignored = append(ignored, rel)
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
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
		hash, err := hashFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, model.ManifestEntry{Path: rel, Size: info.Size(), SHA256: hash})
		return nil
	})
	if err != nil {
		return nil, nil, fault.Wrap("SOURCE_SCAN_FAILED", "cannot build input manifest", false, err)
	}
	return entries, ignored, nil
}

// Snapshot copies the included source files into an immutable staging directory
// and computes the manifest from the bytes written there. The returned cleanup
// function must be called by the caller.
func Snapshot(root string, excludes []string) (string, []model.ManifestEntry, []string, func(), error) {
	staging, err := os.MkdirTemp("", "joyrun-snapshot-*")
	if err != nil {
		return "", nil, nil, func() {}, fault.Wrap("SOURCE_SNAPSHOT_FAILED", "cannot create input snapshot", false, err)
	}
	cleanup := func() { _ = os.RemoveAll(staging) }
	patterns := []string{".joyrun/"}
	patterns = append(patterns, excludes...)
	var entries []model.ManifestEntry
	var ignored []string
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
		if matches(rel, entry.IsDir(), patterns) {
			ignored = append(ignored, rel)
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		destination := filepath.Join(staging, filepath.FromSlash(rel))
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(destination, info.Mode().Perm())
		}
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
			size, hash, err := copyAndHash(target, destination, targetInfo.Mode().Perm())
			if err != nil {
				return err
			}
			entries = append(entries, model.ManifestEntry{Path: rel, Size: size, SHA256: hash})
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported special file %s (%s)", rel, info.Mode().Type())
		}
		size, hash, err := copyAndHash(sourcePath, destination, info.Mode().Perm())
		if err != nil {
			return err
		}
		entries = append(entries, model.ManifestEntry{Path: rel, Size: size, SHA256: hash})
		return nil
	})
	if err != nil {
		cleanup()
		return "", nil, nil, func() {}, fault.Wrap("SOURCE_SNAPSHOT_FAILED", "cannot create input snapshot", false, err)
	}
	return staging, entries, ignored, cleanup, nil
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
	result := []string{".joyrun/"}
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
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
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
