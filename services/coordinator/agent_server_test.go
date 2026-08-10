package main

import (
	"os"
	"context"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	agentv1 "github.com/ayeus/ayeusann/gen/go/agent/v1"
	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/db"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func getTestDBURL() string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://ayeusann:ayeusann_dev@localhost:5433/ayeusann?sslmode=disable"
}
const testJWTSecret = "test-secret-key-32-bytes-long-super-secure!"

func TestAgentServerRegistrationAndHeartbeat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbClient, err := db.NewClient(ctx, db.Config{URL: getTestDBURL()})
	if err != nil {
		t.Fatalf("Failed to connect to test database: %v", err)
	}
	defer dbClient.Close()

	tm := auth.NewTokenManager(testJWTSecret, 15*time.Minute, 7*24*time.Hour)

	// Create test user and registration token
	userID := uuid.New().String()
	orgID := uuid.New().String()

	// Seed test user in DB
	_, err = dbClient.Pool.Exec(ctx, `
		INSERT INTO users (id, email, name, auth_provider)
		VALUES ($1, $2, 'Host Owner', 'email');
	`, userID, "owner-"+userID+"@AyeusANN.io")
	if err != nil {
		t.Fatalf("Failed to seed test user: %v", err)
	}

	regToken, _, err := tm.GeneratePair(userID, orgID, "host_installer")
	if err != nil {
		t.Fatalf("Failed to generate registration token: %v", err)
	}

	// Start test gRPC server
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to listen on random port: %v", err)
	}
	defer lis.Close()

	grpcServer := grpc.NewServer()
	agentServer := NewAgentServer(dbClient, tm)
	agentv1.RegisterAgentServiceServer(grpcServer, agentServer)

	go func() {
		_ = grpcServer.Serve(lis)
	}()
	defer grpcServer.Stop()

	// Connect gRPC client
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Failed to connect gRPC client: %v", err)
	}
	defer conn.Close()

	client := agentv1.NewAgentServiceClient(conn)
	stream, err := client.Session(ctx)
	if err != nil {
		t.Fatalf("Failed to start session stream: %v", err)
	}

	// 1. Send RegisterRequest
	regMsg := &agentv1.AgentMessage{
		Payload: &agentv1.AgentMessage_Register{
			Register: &agentv1.RegisterRequest{
				RegistrationToken:   regToken,
				Hostname:            "node-chennai-01",
				Os:                  "Linux",
				OsVersion:           "Ubuntu 24.04 LTS",
				Kernel:              "6.8.0-generic",
				Region:              "IN-SOUTH",
				AgentVersion:        "0.1.0",
				HardwareFingerprint: "sha256:test_hw_fingerprint_01",
				Gpus: []*agentv1.GpuInfo{
					{
						Model:                   "NVIDIA GeForce RTX 4090",
						VramGb:                  24,
						DriverVersion:           "550.54.14",
						CudaVersion:             "12.4",
						ComputeCapabilityMajor: 8,
						ComputeCapabilityMinor: 9,
						Uuid:                    "GPU-" + uuid.New().String(),
						Fingerprint:             "sha256:rtx_4090_fp",
					},
				},
			},
		},
	}

	if err := stream.Send(regMsg); err != nil {
		t.Fatalf("Failed to send RegisterRequest: %v", err)
	}

	// 2. Receive RegisterResponse
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("Failed to receive RegisterResponse: %v", err)
	}

	regResp := resp.GetRegisterResponse()
	if regResp == nil || !regResp.Accepted {
		t.Fatalf("Expected registration accepted = true, got: %+v", regResp)
	}

	if regResp.HostId == "" || regResp.OverlayIp == "" {
		t.Fatalf("Expected non-empty HostId and OverlayIp, got host_id=%s, overlay_ip=%s", regResp.HostId, regResp.OverlayIp)
	}

	// 3. Send Heartbeat
	hbMsg := &agentv1.AgentMessage{
		Payload: &agentv1.AgentMessage_Heartbeat{
			Heartbeat: &agentv1.Heartbeat{
				CpuUsagePct:    12.5,
				MemoryUsagePct: 35.0,
				ActiveJobs:     0,
				GpuStatus: []*agentv1.GpuStatus{
					{
						GpuUuid:        regMsg.GetRegister().Gpus[0].Uuid,
						UtilizationPct: 5.0,
						VramUsedMb:     1024,
						VramTotalMb:    24576,
						TemperatureC:   45,
						PowerDrawW:     80,
					},
				},
			},
		},
	}

	if err := stream.Send(hbMsg); err != nil {
		t.Fatalf("Failed to send Heartbeat: %v", err)
	}
}
