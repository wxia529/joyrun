//go:build windows

package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

type pipeListener struct {
	name   string
	mu     sync.Mutex
	closed bool
	active windows.Handle
}

type pipeConn struct {
	file *os.File
}

func Listen(endpoint string) (net.Listener, error) {
	if endpoint == "" {
		return nil, errors.New("empty named-pipe endpoint")
	}
	return &pipeListener{name: endpoint}, nil
}

func (l *pipeListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, net.ErrClosed
	}
	name, err := windows.UTF16PtrFromString(l.name)
	if err != nil {
		l.mu.Unlock()
		return nil, err
	}
	handle, err := windows.CreateNamedPipe(
		name,
		windows.PIPE_ACCESS_DUPLEX,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT,
		windows.PIPE_UNLIMITED_INSTANCES, 64<<10, 64<<10, 0, nil)
	if err != nil {
		l.mu.Unlock()
		return nil, err
	}
	l.active = handle
	l.mu.Unlock()

	err = windows.ConnectNamedPipe(handle, nil)
	if err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
		_ = windows.CloseHandle(handle)
		l.mu.Lock()
		l.active = 0
		closed := l.closed
		l.mu.Unlock()
		if closed {
			return nil, net.ErrClosed
		}
		return nil, err
	}
	file := os.NewFile(uintptr(handle), l.name)
	l.mu.Lock()
	l.active = 0
	l.mu.Unlock()
	return &pipeConn{file: file}, nil
}

func (l *pipeListener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	handle := l.active
	l.active = 0
	l.mu.Unlock()
	if handle != 0 {
		return windows.CloseHandle(handle)
	}
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr(l.name) }

func Dial(ctx context.Context, endpoint string) (net.Conn, error) {
	name, err := windows.UTF16PtrFromString(endpoint)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		handle, openErr := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
			0, nil, windows.OPEN_EXISTING, 0, 0)
		if openErr == nil {
			return &pipeConn{file: os.NewFile(uintptr(handle), endpoint)}, nil
		}
		if !errors.Is(openErr, windows.ERROR_PIPE_BUSY) && !errors.Is(openErr, windows.ERROR_FILE_NOT_FOUND) {
			return nil, openErr
		}
		wait := 100 * time.Millisecond
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wait {
			wait = time.Until(deadline)
		}
		if wait <= 0 {
			return nil, context.DeadlineExceeded
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *pipeConn) Read(p []byte) (int, error)  { return c.file.Read(p) }
func (c *pipeConn) Write(p []byte) (int, error) { return c.file.Write(p) }
func (c *pipeConn) Close() error {
	if c.file == nil {
		return nil
	}
	return c.file.Close()
}
func (c *pipeConn) LocalAddr() net.Addr              { return pipeAddr(c.file.Name()) }
func (c *pipeConn) RemoteAddr() net.Addr             { return pipeAddr(c.file.Name()) }
func (c *pipeConn) SetDeadline(time.Time) error      { return nil }
func (c *pipeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *pipeConn) SetWriteDeadline(time.Time) error { return nil }

type pipeAddr string

func (p pipeAddr) Network() string { return "pipe" }
func (p pipeAddr) String() string  { return string(p) }

func removeEndpoint(endpoint string) error { return nil }

var _ net.Listener = (*pipeListener)(nil)
var _ net.Conn = (*pipeConn)(nil)
