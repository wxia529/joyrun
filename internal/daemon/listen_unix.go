//go:build !windows

package daemon

import (
	"context"
	"net"
	"os"
)

func Listen(endpoint string) (net.Listener, error) {
	_ = os.Remove(endpoint)
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(endpoint, 0o600)
	return listener, nil
}

func Dial(ctx context.Context, endpoint string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", endpoint)
}

func removeEndpoint(endpoint string) error { return os.Remove(endpoint) }
