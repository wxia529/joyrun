package daemon

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/wxia529/joyrun/internal/fault"
)

const (
	ProtocolVersion = 1
	maxFrameSize    = 8 << 20
)

type Paths struct {
	Endpoint string
	Lock     string
	Secret   string
	Log      string
}

type Request struct {
	Type          string   `json:"type"`
	Protocol      int      `json:"protocol,omitempty"`
	ClientVersion string   `json:"client_version,omitempty"`
	Secret        string   `json:"secret,omitempty"`
	RequestID     string   `json:"request_id,omitempty"`
	Method        string   `json:"method,omitempty"`
	CWD           string   `json:"cwd,omitempty"`
	Args          []string `json:"args,omitempty"`
}

type HelloResponse struct {
	Type        string `json:"type"`
	Protocol    int    `json:"protocol"`
	MinProtocol int    `json:"min_protocol"`
	Version     string `json:"version"`
	InstanceID  string `json:"instance_id"`
	PID         int    `json:"pid"`
	Schema      string `json:"schema"`
	StartedAt   string `json:"started_at"`
	RequestID   string `json:"request_id,omitempty"`
	Error       string `json:"error,omitempty"`
}

type Response struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id,omitempty"`
	OK        bool   `json:"ok"`
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	Error     string `json:"error,omitempty"`
}

type Handler func(context.Context, []string, string) (exitCode int, stdout, stderr string)

type Server struct {
	Paths       Paths
	Version     string
	Schema      string
	Handler     Handler
	Worker      func(context.Context)
	StartedAt   time.Time
	InstanceID  string
	lock        *Lock
	listener    net.Listener
	secret      string
	stop        chan struct{}
	stopOnce    sync.Once
	requestCtx  context.Context
	connections sync.WaitGroup
	slots       chan struct{}
}

func NewServer(paths Paths, version, schema string, handler Handler) *Server {
	return &Server{
		Paths: paths, Version: version, Schema: schema, Handler: handler,
		StartedAt: time.Now().UTC(), InstanceID: newInstanceID(), stop: make(chan struct{}), slots: make(chan struct{}, 64),
	}
}

func (s *Server) Run(ctx context.Context) error {
	workerCtx, workerCancel := context.WithCancel(ctx)
	defer workerCancel()
	s.requestCtx = workerCtx
	lock, err := AcquireLock(s.Paths.Lock)
	if err != nil {
		return err
	}
	s.lock = lock
	defer lock.Close()

	if err := prepareRuntime(s.Paths); err != nil {
		return fault.Wrap("DAEMON_START_FAILED", "cannot prepare daemon runtime", false, err)
	}
	secret, err := randomSecret()
	if err != nil {
		return fault.Wrap("DAEMON_START_FAILED", "cannot create daemon session secret", false, err)
	}
	s.secret = secret
	if err := writeSecret(s.Paths.Secret, secret); err != nil {
		return fault.Wrap("DAEMON_START_FAILED", "cannot write daemon session secret", false, err)
	}
	listener, err := Listen(s.Paths.Endpoint)
	if err != nil {
		_ = os.Remove(s.Paths.Secret)
		return fault.Wrap("DAEMON_START_FAILED", "cannot listen for local daemon clients", false, err)
	}
	s.listener = listener
	defer func() {
		_ = listener.Close()
		_ = removeEndpoint(s.Paths.Endpoint)
		_ = os.Remove(s.Paths.Secret)
	}()

	go func() {
		select {
		case <-ctx.Done():
			s.Stop()
		case <-s.stop:
			workerCancel()
		}
	}()
	if s.Worker != nil {
		go s.Worker(workerCtx)
	}
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			select {
			case <-s.stop:
				s.connections.Wait()
				return nil
			default:
			}
			if ne, ok := acceptErr.(net.Error); ok && ne.Temporary() {
				continue
			}
			return fault.Wrap("DAEMON_FAILED", "daemon listener failed", true, acceptErr)
		}
		s.connections.Add(1)
		go func() {
			defer s.connections.Done()
			s.serve(conn)
		}()
	}
}

func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
		if s.listener != nil {
			_ = s.listener.Close()
		}
	})
}

func (s *Server) serve(conn net.Conn) {
	defer conn.Close()
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return
	}
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	var hello Request
	if err := readFrame(conn, &hello); err != nil {
		return
	}
	if hello.Type != "hello" || hello.Protocol != ProtocolVersion || hello.Secret != s.secret {
		_ = writeFrame(conn, HelloResponse{Type: "hello_error", Protocol: ProtocolVersion,
			MinProtocol: ProtocolVersion, Version: s.Version, Error: "IPC_AUTH_FAILED"})
		return
	}
	if err := writeFrame(conn, HelloResponse{
		Type: "hello_ok", Protocol: ProtocolVersion, MinProtocol: ProtocolVersion,
		Version: s.Version, InstanceID: s.InstanceID, PID: os.Getpid(), Schema: s.Schema,
		StartedAt: s.StartedAt.Format(time.RFC3339Nano),
	}); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	var req Request
	if err := readFrame(conn, &req); err != nil {
		return
	}
	if req.Type != "request" {
		_ = writeFrame(conn, Response{Type: "response", RequestID: req.RequestID,
			OK: false, ExitCode: 1, Error: "IPC_INVALID_REQUEST"})
		return
	}
	if req.Method == "daemon.stop" {
		_ = writeFrame(conn, Response{Type: "response", RequestID: req.RequestID, OK: true})
		s.Stop()
		return
	}
	if req.Method == "daemon.status" {
		payload := map[string]any{
			"version": s.Version, "protocol": ProtocolVersion, "pid": os.Getpid(),
			"instance_id": s.InstanceID, "schema": s.Schema,
			"started_at": s.StartedAt.Format(time.RFC3339Nano),
		}
		data, _ := json.Marshal(payload)
		_ = writeFrame(conn, Response{Type: "response", RequestID: req.RequestID,
			OK: true, ExitCode: 0, Stdout: string(data) + "\n"})
		return
	}
	if s.Handler == nil {
		_ = writeFrame(conn, Response{Type: "response", RequestID: req.RequestID,
			OK: false, ExitCode: 1, Error: "DAEMON_HANDLER_UNAVAILABLE"})
		return
	}
	requestCtx := s.requestCtx
	if requestCtx == nil {
		requestCtx = context.Background()
	}
	code, stdout, stderr := s.Handler(requestCtx, req.Args, req.CWD)
	_ = writeFrame(conn, Response{Type: "response", RequestID: req.RequestID,
		OK: code == 0, ExitCode: code, Stdout: stdout, Stderr: stderr})
}

func Call(ctx context.Context, paths Paths, version string, args []string) (Response, error) {
	secret, err := os.ReadFile(paths.Secret)
	if err != nil {
		return Response{}, fault.Wrap("DAEMON_UNAVAILABLE", "cannot read daemon session secret", true, err)
	}
	conn, err := Dial(ctx, paths.Endpoint)
	if err != nil {
		return Response{}, fault.Wrap("DAEMON_UNAVAILABLE", "cannot connect to JoyRun daemon", true, err)
	}
	defer conn.Close()
	if err := writeFrame(conn, Request{Type: "hello", Protocol: ProtocolVersion,
		ClientVersion: version, Secret: string(secret)}); err != nil {
		return Response{}, fault.Wrap("DAEMON_UNAVAILABLE", "cannot send daemon handshake", true, err)
	}
	var hello HelloResponse
	if err := readFrame(conn, &hello); err != nil {
		return Response{}, fault.Wrap("DAEMON_UNAVAILABLE", "cannot read daemon handshake", true, err)
	}
	if hello.Type != "hello_ok" {
		return Response{}, fault.New("DAEMON_VERSION_MISMATCH", "daemon rejected the client protocol", false)
	}
	id := newInstanceID()
	method := "command.execute"
	if len(args) >= 2 && args[0] == "daemon" && args[1] == "stop" {
		method = "daemon.stop"
	}
	if len(args) >= 2 && args[0] == "daemon" && args[1] == "status" {
		method = "daemon.status"
	}
	if err := writeFrame(conn, Request{Type: "request", Protocol: ProtocolVersion,
		RequestID: id, Method: method, CWD: currentWorkingDirectory(), Args: args}); err != nil {
		return Response{}, fault.Wrap("DAEMON_UNAVAILABLE", "cannot send daemon request", true, err)
	}
	var response Response
	if err := readFrame(conn, &response); err != nil {
		return Response{}, fault.Wrap("DAEMON_UNAVAILABLE", "cannot read daemon response", true, err)
	}
	return response, nil
}

func currentWorkingDirectory() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}

func readFrame(r io.Reader, value any) error {
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return err
	}
	if length == 0 || length > maxFrameSize {
		return fmt.Errorf("invalid IPC frame length %d", length)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func writeFrame(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > maxFrameSize {
		return fmt.Errorf("IPC frame exceeds %d bytes", maxFrameSize)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func newInstanceID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err == nil {
		return "di_" + hex.EncodeToString(data[:])
	}
	return fmt.Sprintf("di_%d", time.Now().UnixNano())
}

func randomSecret() (string, error) {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}

func writeSecret(path, value string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(value)
	return err
}
