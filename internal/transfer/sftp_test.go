package transfer

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"
)

func TestSFTPPushAndPull(t *testing.T) {
	var progress bytes.Buffer
	backend := &SFTP{connect: inMemorySFTPConnector(t), Stderr: &progress}
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "input.txt"), []byte("input"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "ignored.tmp"), []byte("ignore"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "result.txt"), []byte("result"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := backend.Push(ctx, "unused", source, "/remote/work", []string{"*.tmp"}); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(destination, "input.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := backend.Pull(ctx, "unused", "/remote/work", destination, []string{"input.txt", "nested/result.txt"}); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(destination, "input.txt"), "input")
	assertFileContent(t, filepath.Join(destination, "nested", "result.txt"), "result")
	if _, err := os.Stat(filepath.Join(destination, "ignored.tmp")); !os.IsNotExist(err) {
		t.Fatalf("excluded file was unexpectedly downloaded: %v", err)
	}
	if !bytes.Contains(progress.Bytes(), []byte("Uploading input.txt")) ||
		!bytes.Contains(progress.Bytes(), []byte("Downloading nested/result.txt")) {
		t.Fatalf("missing transfer progress: %q", progress.String())
	}
}

func TestCleanRelativeRejectsEscapes(t *testing.T) {
	for _, value := range []string{"", "../secret", "/absolute", "a/../../secret", "a\x00b"} {
		if _, err := cleanRelative(value); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func inMemorySFTPConnector(t *testing.T) sftpConnect {
	t.Helper()
	handlers := sftp.InMemHandler()
	return func(_ context.Context, _ string, _ io.Writer) (*sftp.Client, func() error, error) {
		clientConn, serverConn := net.Pipe()
		server := sftp.NewRequestServer(serverConn, handlers)
		done := make(chan error, 1)
		go func() {
			done <- server.Serve()
		}()
		client, err := sftp.NewClientPipe(clientConn, clientConn)
		if err != nil {
			_ = server.Close()
			t.Fatal(err)
		}
		cleanup := func() error {
			_ = client.Close()
			_ = server.Close()
			<-done
			return nil
		}
		return client, cleanup, nil
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s: want %q, got %q", path, want, data)
	}
}
