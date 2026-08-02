//go:build !windows

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/wxia529/joyrun/internal/fault"
)

type Lock struct {
	file *os.File
}

func AcquireLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fault.Wrap("DAEMON_LOCK_FAILED", "cannot create daemon runtime directory", false, err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fault.Wrap("DAEMON_LOCK_FAILED", "cannot open daemon lock", false, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fault.New("DAEMON_UNAVAILABLE", "another JoyRun process owns the daemon lock", true)
		}
		return nil, fault.Wrap("DAEMON_LOCK_FAILED", "cannot acquire daemon lock", false, err)
	}
	return &Lock{file: file}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	return l.file.Close()
}
