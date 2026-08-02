//go:build windows

package daemon

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/wxia529/joyrun/internal/fault"
	"golang.org/x/sys/windows"
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
	var overlapped windows.Overlapped
	err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if err != nil {
		_ = file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
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
	var overlapped windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &overlapped)
	return l.file.Close()
}
