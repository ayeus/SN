package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentv1 "github.com/ayeus/ayeusann/gen/go/agent/v1"
	"github.com/ayeus/ayeusann/internal/platform"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// stubCoordinator is a real gRPC AgentService with just enough behaviour to
// show what survives the gateway: it answers the registration, pushes a
// message on its own, answers a heartbeat, and notices the client finishing.
type stubCoordinator struct {
	agentv1.UnimplementedAgentServiceServer
	pushAfter time.Duration
	sawEOF    chan struct{}
}

func (s *stubCoordinator) Session(stream agentv1.AgentService_SessionServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	reg := first.GetRegister()
	if reg == nil {
		return status.Error(codes.InvalidArgument, "first message must be a registration")
	}
	if reg.GetHostname() == "banned" {
		return status.Error(codes.PermissionDenied, "this host has been banned from the network")
	}
	if reg.GetHostname() == "dropped" {
		// Accepted, then the platform ends the session while the agent is idle.
		if err := stream.Send(&agentv1.CoordinatorMessage{Payload: &agentv1.CoordinatorMessage_RegisterResponse{
			RegisterResponse: &agentv1.RegisterResponse{Accepted: true, HostId: "host-1"},
		}}); err != nil {
			return err
		}
		time.Sleep(200 * time.Millisecond)
		return status.Error(codes.Unavailable, "session closed by the platform; reconnect")
	}
	if err := stream.Send(&agentv1.CoordinatorMessage{Payload: &agentv1.CoordinatorMessage_RegisterResponse{
		RegisterResponse: &agentv1.RegisterResponse{Accepted: true, HostId: "host-1"},
	}}); err != nil {
		return err
	}

	// A message nobody asked for, sent while the client is silent.
	time.Sleep(s.pushAfter)
	if err := stream.Send(&agentv1.CoordinatorMessage{Payload: &agentv1.CoordinatorMessage_Manifest{
		Manifest: &agentv1.ManifestDispatch{ReplicaId: "replica-1"},
	}}); err != nil {
		return err
	}

	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			close(s.sawEOF)
			return nil
		}
		if err != nil {
			return err
		}
		if msg.GetHeartbeat() != nil {
			if err := stream.Send(&agentv1.CoordinatorMessage{Payload: &agentv1.CoordinatorMessage_Drain{
				Drain: &agentv1.DrainRequest{Reason: "heartbeat seen"},
			}}); err != nil {
				return err
			}
		}
	}
}

// gatewayInFrontOf starts the stub and a gateway in front of it, built the way
// production builds it, and returns the gateway's address.
func gatewayInFrontOf(t *testing.T, stub *stubCoordinator, readTimeout time.Duration) string {
	t.Helper()

	coordLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcSrv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(grpcSrv, stub)
	go func() { _ = grpcSrv.Serve(coordLis) }()
	t.Cleanup(grpcSrv.Stop)

	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "install.sh"), []byte("#!/bin/sh\necho install\n"), 0o644)
	elsewhere, _ := url.Parse("http://127.0.0.1:1") // nothing these tests call
	handler := NewHandler(Upstreams{
		ControlAPI: elsewhere, Inference: elsewhere, Trust: elsewhere,
		CoordinatorGRPC: &url.URL{Scheme: "http", Host: coordLis.Addr().String()},
		InstallDir:      dir, DownloadsDir: dir,
	})

	gwLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := platform.NewHTTPServer(gwLis.Addr().String(), platform.MetricsMiddlewareLabeled("gateway", handler), true)
	// Far shorter than production's 30 s, so a test that outlives it proves the
	// agent route really does clear its deadline.
	srv.ReadTimeout = readTimeout
	go func() { _ = srv.Serve(gwLis) }()
	t.Cleanup(func() { _ = srv.Close() })
	return gwLis.Addr().String()
}

func dialSession(t *testing.T, addr string) agentv1.AgentService_SessionClient {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	stream, err := agentv1.NewAgentServiceClient(conn).Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

func register(hostname string) *agentv1.AgentMessage {
	return &agentv1.AgentMessage{Payload: &agentv1.AgentMessage_Register{Register: &agentv1.RegisterRequest{Hostname: hostname}}}
}

// An agent session is one long-lived, two-way stream. Through the gateway it
// must behave exactly as it does against the coordinator's own port.
func TestAgentSessionSurvivesTheGateway(t *testing.T) {
	const readTimeout = 200 * time.Millisecond
	stub := &stubCoordinator{pushAfter: 3 * readTimeout, sawEOF: make(chan struct{})}
	addr := gatewayInFrontOf(t, stub, readTimeout)
	stream := dialSession(t, addr)
	start := time.Now()

	if err := stream.Send(register("laptop")); err != nil {
		t.Fatalf("send registration: %v", err)
	}
	resp, err := stream.Recv()
	if err != nil || !resp.GetRegisterResponse().GetAccepted() || resp.GetRegisterResponse().GetHostId() != "host-1" {
		t.Fatalf("registration answer = %v, %v", resp, err)
	}

	// The coordinator speaks first, well after the server's read timeout, while
	// this side has sent nothing more.
	pushed, err := stream.Recv()
	if err != nil || pushed.GetManifest().GetReplicaId() != "replica-1" {
		t.Fatalf("pushed message = %v, %v; the stream must stay open while the agent is silent", pushed, err)
	}
	if waited := time.Since(start); waited < 3*readTimeout {
		t.Fatalf("the push arrived after %s, before the read timeout could have fired", waited)
	}

	// Still usable in both directions a second later.
	time.Sleep(time.Second)
	if err := stream.Send(&agentv1.AgentMessage{Payload: &agentv1.AgentMessage_Heartbeat{Heartbeat: &agentv1.Heartbeat{ActiveJobs: 1}}}); err != nil {
		t.Fatalf("send after 1 s: %v", err)
	}
	reply, err := stream.Recv()
	if err != nil || reply.GetDrain().GetReason() != "heartbeat seen" {
		t.Fatalf("reply to a heartbeat after 1 s = %v, %v", reply, err)
	}

	// Finishing the client's side reaches the coordinator as end-of-stream, and
	// its clean return reaches the client the same way.
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stub.sawEOF:
	case <-time.After(5 * time.Second):
		t.Fatal("the coordinator never saw the agent finish its side of the stream")
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("after a clean end Recv = %v, want io.EOF", err)
	}
}

// The agent decides whether to retry from the status code, which travels in
// HTTP trailers. A refusal must arrive as itself, not as a generic failure.
func TestAgentRefusalKeepsItsStatusThroughTheGateway(t *testing.T) {
	addr := gatewayInFrontOf(t, &stubCoordinator{sawEOF: make(chan struct{})}, 30*time.Second)
	stream := dialSession(t, addr)
	if err := stream.Send(register("banned")); err != nil {
		t.Fatal(err)
	}
	_, err := stream.Recv()
	if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "this host has been banned from the network" {
		t.Fatalf("refusal arrived as %v, want PermissionDenied with the coordinator's message", err)
	}
}

// When the coordinator ends a session (it is restarting, or it replaced the
// session) the agent has to hear about it at once, not when it next happens
// to send something: until it does, it is attached to nothing.
func TestAgentHearsAtOnceWhenTheCoordinatorEndsTheSession(t *testing.T) {
	addr := gatewayInFrontOf(t, &stubCoordinator{sawEOF: make(chan struct{})}, 30*time.Second)
	stream := dialSession(t, addr)
	if err := stream.Send(register("dropped")); err != nil {
		t.Fatal(err)
	}
	if resp, err := stream.Recv(); err != nil || !resp.GetRegisterResponse().GetAccepted() {
		t.Fatalf("registration answer = %v, %v", resp, err)
	}
	// The agent now sends nothing at all.
	start := time.Now()
	_, err := stream.Recv()
	if status.Code(err) != codes.Unavailable || status.Convert(err).Message() != "session closed by the platform; reconnect" {
		t.Fatalf("the end of the session arrived as %v, want Unavailable with the coordinator's message", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("the agent heard %s after the session ended, want at once", waited.Round(time.Millisecond))
	}
}

func TestGatewayStillServesOrdinaryRequestsOnTheSamePort(t *testing.T) {
	addr := gatewayInFrontOf(t, &stubCoordinator{sawEOF: make(chan struct{})}, 30*time.Second)

	resp, err := http.Get("http://" + addr + "/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ProtoMajor != 1 || string(body) != "#!/bin/sh\necho install\n" {
		t.Fatalf("GET /install.sh over HTTP/1 = %d %s %q", resp.StatusCode, resp.Proto, body)
	}

	// A browser or script that stumbles on the agent address gets a plain answer.
	resp, err = http.Post("http://"+addr+agentServicePath+"Session", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("a non-gRPC request to the agent address = %d, want 415", resp.StatusCode)
	}
}

// With the coordinator down, an agent must see "unavailable" (and so retry),
// not a protocol error.
func TestAgentSeesUnavailableWhenTheCoordinatorIsDown(t *testing.T) {
	dir := t.TempDir()
	elsewhere, _ := url.Parse("http://127.0.0.1:1")
	handler := NewHandler(Upstreams{
		ControlAPI: elsewhere, Inference: elsewhere, Trust: elsewhere,
		CoordinatorGRPC: elsewhere, InstallDir: dir, DownloadsDir: dir,
	})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := platform.NewHTTPServer(lis.Addr().String(), handler, true)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { _ = srv.Close() })

	stream := dialSession(t, lis.Addr().String())
	_ = stream.Send(register("laptop"))
	if _, err := stream.Recv(); status.Code(err) != codes.Unavailable {
		t.Fatalf("with no coordinator the agent got %v, want Unavailable", err)
	}
}
