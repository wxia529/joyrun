package transfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"github.com/wxia529/joyrun/internal/fault"
	"github.com/wxia529/joyrun/internal/manifest"
	"github.com/wxia529/joyrun/internal/remote"
)

type sftpConnect func(context.Context, string, io.Writer) (*sftp.Client, func() error, error)

type SFTP struct {
	Stderr      io.Writer
	ControlPath string
	connect     sftpConnect
}

func (s *SFTP) Check(ctx context.Context, host string) error {
	client, closeSession, err := s.open(ctx, host)
	if err != nil {
		return err
	}
	if _, err := client.Getwd(); err != nil {
		_ = closeSession()
		return fault.Wrap("SFTP_FAILED", "cannot query the remote SFTP working directory", true, err)
	}
	if err := closeSession(); err != nil {
		return fault.Wrap("SFTP_FAILED", "cannot close the SFTP session cleanly", true, err)
	}
	return nil
}

func (s *SFTP) Push(ctx context.Context, host, localDir, remoteDir string, excludes []string) error {
	client, closeSession, err := s.open(ctx, host)
	if err != nil {
		return err
	}
	operationErr := s.push(ctx, client, localDir, remoteDir, excludes)
	closeErr := closeSession()
	if operationErr != nil {
		return operationErr
	}
	if closeErr != nil {
		return fault.Wrap("UPLOAD_FAILED", "SFTP session ended unexpectedly", true, closeErr)
	}
	return nil
}

func (s *SFTP) Pull(ctx context.Context, host, remoteDir, localDir string, files []string) error {
	if len(files) == 0 {
		return nil
	}
	client, closeSession, err := s.open(ctx, host)
	if err != nil {
		return err
	}
	operationErr := s.pull(ctx, client, remoteDir, localDir, files)
	closeErr := closeSession()
	if operationErr != nil {
		return operationErr
	}
	if closeErr != nil {
		return fault.Wrap("PULL_FAILED", "SFTP session ended unexpectedly", true, closeErr)
	}
	return nil
}

func (s *SFTP) push(ctx context.Context, client *sftp.Client, localDir, remoteDir string, excludes []string) error {
	if err := client.MkdirAll(remoteDir); err != nil {
		return fault.Wrap("UPLOAD_FAILED", "cannot create remote SFTP directory", true, err)
	}
	type remoteDirectory struct {
		path string
		mode os.FileMode
	}
	var directories []remoteDirectory
	err := filepath.WalkDir(localDir, func(localPath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if localPath == localDir {
			return nil
		}
		rel, err := filepath.Rel(localDir, localPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if manifest.Excluded(rel, entry.IsDir(), excludes) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		remotePath := path.Join(remoteDir, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if err := client.MkdirAll(remotePath); err != nil {
				return fmt.Errorf("create directory %s: %w", rel, err)
			}
			directories = append(directories, remoteDirectory{path: remotePath, mode: info.Mode().Perm()})
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported staged file %s (%s)", rel, info.Mode().Type())
		}
		reportProgress(s.Stderr, "Uploading", rel, info.Size())
		if err := uploadFile(client, localPath, remotePath, info.Mode().Perm(),
			s.Stderr, rel, info.Size()); err != nil {
			return fmt.Errorf("upload %s: %w", rel, err)
		}
		return nil
	})
	if err != nil {
		return fault.Wrap("UPLOAD_FAILED", "SFTP upload failed", true, err)
	}
	for index := len(directories) - 1; index >= 0; index-- {
		directory := directories[index]
		if err := client.Chmod(directory.path, directory.mode); err != nil && !extensionFallbackAllowed(err) {
			return fault.Wrap("UPLOAD_FAILED", "cannot set remote directory permissions", true, err)
		}
	}
	return nil
}

func uploadFile(
	client *sftp.Client,
	localPath, remotePath string,
	mode os.FileMode,
	progress io.Writer,
	relative string,
	size int64,
) error {
	input, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer input.Close()
	tempPath := remotePath + ".joyrun-part"
	inputInfo, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	var offset int64
	if existing, statErr := client.Stat(tempPath); statErr == nil {
		offset = existing.Size()
		if offset > inputInfo.Size() {
			_ = client.Remove(tempPath)
			offset = 0
		}
	}
	if offset > inputInfo.Size() {
		return fmt.Errorf("remote partial file is larger than local source")
	}
	if _, err := input.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE
	if offset > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	output, err := client.OpenFile(tempPath, flags)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, newProgressReader(input, progress, "Uploading", relative, size))
	chmodErr := output.Chmod(mode)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if chmodErr != nil && !extensionFallbackAllowed(chmodErr) {
		return chmodErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := client.PosixRename(tempPath, remotePath); err == nil {
		return nil
	} else if !extensionFallbackAllowed(err) {
		return err
	}
	if _, err := client.Lstat(remotePath); err == nil {
		if err := client.Remove(remotePath); err != nil {
			return err
		}
	}
	return client.Rename(tempPath, remotePath)
}

func extensionFallbackAllowed(err error) bool {
	var status *sftp.StatusError
	if !errors.As(err, &status) {
		return false
	}
	return status.Code == uint32(sftp.ErrSshFxFailure) || status.Code == uint32(sftp.ErrSshFxOpUnsupported)
}

func (s *SFTP) pull(ctx context.Context, client *sftp.Client, remoteDir, localDir string, files []string) error {
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return fault.Wrap("PULL_FAILED", "cannot create local result directory", false, err)
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return fault.Wrap("PULL_FAILED", "SFTP pull cancelled", true, err)
		}
		rel, err := cleanRelative(file)
		if err != nil {
			return fault.Wrap("PULL_FAILED", "unsafe remote file path", false, err)
		}
		if err := downloadFile(client, path.Join(remoteDir, rel),
			filepath.Join(localDir, filepath.FromSlash(rel)), s.Stderr, rel); err != nil {
			return fault.Wrap("PULL_FAILED", "SFTP download failed for "+rel, true, err)
		}
	}
	return nil
}

func downloadFile(client *sftp.Client, remotePath, localPath string, progress io.Writer, relative string) error {
	input, err := client.Open(remotePath)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("remote path is not a regular file: %s", remotePath)
	}
	reportProgress(progress, "Downloading", relative, info.Size())
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	tempPath := filepath.Join(filepath.Dir(localPath), "."+filepath.Base(localPath)+".joyrun-part")
	output, err := os.OpenFile(tempPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	keepTemp := true
	defer func() {
		_ = output.Close()
		if keepTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if existing, statErr := output.Stat(); statErr == nil {
		if existing.Size() > info.Size() {
			_ = output.Close()
			_ = os.Remove(tempPath)
			output, err = os.OpenFile(tempPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
		} else if _, err := input.Seek(existing.Size(), io.SeekStart); err != nil {
			return err
		}
	}
	if _, err := io.Copy(output,
		newProgressReader(input, progress, "Downloading", relative, info.Size())); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	if err := replaceLocalFile(tempPath, localPath); err != nil {
		return err
	}
	keepTemp = false
	return nil
}

func reportProgress(writer io.Writer, action, relative string, size int64) {
	if writer == nil {
		return
	}
	fmt.Fprintf(writer, "%s %s (%s)\n", action, relative, byteSize(size))
}

type progressReader struct {
	reader      io.Reader
	writer      io.Writer
	action      string
	relative    string
	total       int64
	transferred int64
	nextReport  int64
	lastReport  time.Time
}

func newProgressReader(
	reader io.Reader,
	writer io.Writer,
	action, relative string,
	total int64,
) io.Reader {
	if writer == nil || total < 1024*1024 {
		return reader
	}
	return &progressReader{
		reader: reader, writer: writer, action: action, relative: relative,
		total: total, nextReport: 16 * 1024 * 1024, lastReport: time.Now(),
	}
}

func (r *progressReader) Read(buffer []byte) (int, error) {
	count, err := r.reader.Read(buffer)
	r.transferred += int64(count)
	now := time.Now()
	done := err == io.EOF || r.transferred >= r.total
	if done || r.transferred >= r.nextReport || now.Sub(r.lastReport) >= 2*time.Second {
		percent := 100 * float64(r.transferred) / float64(r.total)
		fmt.Fprintf(r.writer, "%s %s: %s / %s (%.0f%%)\n",
			r.action, r.relative, byteSize(r.transferred), byteSize(r.total), percent)
		r.nextReport = r.transferred + 16*1024*1024
		r.lastReport = now
	}
	return count, err
}

func byteSize(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	for _, unit := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= 1024
		if value < 1024 || unit == "TiB" {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	panic("unreachable")
}

func cleanRelative(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("invalid relative path %q", value)
	}
	cleaned := path.Clean(value)
	if cleaned == "." || path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path escapes the task directory: %q", value)
	}
	return cleaned, nil
}

func (s *SFTP) open(ctx context.Context, host string) (*sftp.Client, func() error, error) {
	if s.connect != nil {
		return s.connect(ctx, host, s.Stderr)
	}
	return connectOpenSSH(ctx, host, s.Stderr, s.ControlPath)
}

func connectOpenSSH(ctx context.Context, host string, diagnostic io.Writer, controlPath string) (*sftp.Client, func() error, error) {
	args := append(remote.OpenSSHOptionsFor(controlPath), host, "-s", "sftp")
	cmd := exec.CommandContext(ctx, "ssh", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fault.Wrap("SFTP_FAILED", "cannot open OpenSSH stdin", false, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fault.Wrap("SFTP_FAILED", "cannot open OpenSSH stdout", false, err)
	}
	var stderr bytes.Buffer
	if diagnostic == nil {
		cmd.Stderr = &stderr
	} else {
		cmd.Stderr = io.MultiWriter(&stderr, diagnostic)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fault.Wrap("SFTP_FAILED", "cannot start OpenSSH SFTP subsystem", true, err)
	}
	client, err := sftp.NewClientPipe(stdout, stdin)
	if err != nil {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, nil, fault.Wrap("SFTP_FAILED", withDetail("cannot initialize SFTP", stderr.String()), true, err)
	}
	closeSession := func() error {
		clientErr := client.Close()
		processErr := cmd.Wait()
		return errors.Join(clientErr, processErr)
	}
	return client, closeSession, nil
}

func withDetail(message, detail string) string {
	if detail = strings.TrimSpace(detail); detail != "" {
		return message + ": " + detail
	}
	return message
}
