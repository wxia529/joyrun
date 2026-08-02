package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wxia529/joyrun/internal/fault"
)

func TestFrameRoundTrip(t *testing.T) {
	want := Request{
		Type: "request", Protocol: ProtocolVersion, RequestID: "rq_test",
		Method: "command.execute", CWD: "/tmp/project", Args: []string{"status", "--all"},
	}
	var buffer bytes.Buffer
	if err := writeFrame(&buffer, want); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	var got Request
	if err := readFrame(&buffer, &got); err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	dataWant, _ := json.Marshal(want)
	dataGot, _ := json.Marshal(got)
	if !bytes.Equal(dataWant, dataGot) {
		t.Fatalf("frame mismatch: got %s want %s", dataGot, dataWant)
	}
}

func TestFrameRejectsOversize(t *testing.T) {
	var buffer bytes.Buffer
	if err := writeFrame(&buffer, string(bytes.Repeat([]byte{'x'}, maxFrameSize))); err == nil {
		t.Fatal("writeFrame accepted an oversized frame")
	}
}

func TestLockIsExclusive(t *testing.T) {
	path := t.TempDir() + "/daemon.lock"
	first, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer first.Close()
	second, err := AcquireLock(path)
	if err == nil {
		second.Close()
		t.Fatal("second lock unexpectedly succeeded")
	}
	if code := fault.As(err).Code; code != "DAEMON_UNAVAILABLE" {
		t.Fatalf("second lock error code = %q, want DAEMON_UNAVAILABLE", code)
	}
}

func TestServerCallLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a temporary Unix socket endpoint")
	}
	root := t.TempDir()
	paths := Paths{
		Endpoint: filepath.Join(root, "daemon.sock"),
		Lock:     filepath.Join(root, "daemon.lock"),
		Secret:   filepath.Join(root, "daemon.secret"),
		Log:      filepath.Join(root, "daemon.log"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(paths, "test", "stable/stable-1", func(context.Context, []string, string) (int, string, string) {
		return 0, "handled\n", ""
	})
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()

	callCtx, callCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer callCancel()
	var response Response
	var err error
	for callCtx.Err() == nil {
		response, err = Call(callCtx, paths, "client", []string{"daemon", "status"})
		if err == nil {
			break
		}
		select {
		case serverErr := <-done:
			if strings.Contains(serverErr.Error(), "operation not permitted") {
				t.Skip("environment does not permit temporary Unix sockets")
			}
			t.Fatalf("daemon exited before readiness: %v", serverErr)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		select {
		case serverErr := <-done:
			t.Fatalf("status call: %v (server exited: %v)", err, serverErr)
		default:
		}
		t.Fatalf("status call: %v", err)
	}
	if !response.OK || response.ExitCode != 0 {
		t.Fatalf("unexpected status response: %#v", response)
	}
	response, err = Call(callCtx, paths, "client", []string{"echo"})
	if err != nil || response.ExitCode != 0 || response.Stdout != "handled\n" {
		t.Fatalf("handler response: %#v err=%v", response, err)
	}
	response, err = Call(callCtx, paths, "client", []string{"daemon", "stop"})
	if err != nil || !response.OK {
		t.Fatalf("stop response: %#v err=%v", response, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server exit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop after daemon.stop")
	}
}
