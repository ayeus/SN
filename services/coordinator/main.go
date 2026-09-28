// Package main implements the AyeusANN Coordinator.
//
// Responsibilities (Architecture §4): persistent agent sessions, signed
// manifest dispatch, stage events, host liveness, and — per ADR-011 — the
// inference tunnel that carries requests to hosts over their outbound stream.
package main

import (
	"context"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	agentv1 "github.com/ayeus/ayeusann/gen/go/agent/v1"
	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/manifest"
	"github.com/ayeus/ayeusann/internal/platform"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

func main() {
	port := platform.EnvInt("COORDINATOR_PORT", 8083)
	grpcPort := platform.Env("COORDINATOR_GRPC_PORT", "50051")
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "coordinator")

	jwtSecret, err := platform.JWTSecret()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}
	internalSecret, err := platform.InternalSecret()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	devSeed := ""
	if !platform.IsProduction() {
		devSeed = internalSecret
	}
	signer, err := manifest.NewSigner(os.Getenv("MANIFEST_SIGNING_KEY"), devSeed)
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbClient, err := platform.ConnectDB(ctx)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer dbClient.Close()

	tm, err := auth.NewTokenManager(jwtSecret, 15*time.Minute, 7*24*time.Hour)
	if err != nil {
		log.Fatalf("failed to initialize token manager: %v", err)
	}
	svcAuth, err := auth.NewServiceAuthenticator(internalSecret, auth.ServiceCoordinator)
	if err != nil {
		log.Fatalf("failed to create service authenticator: %v", err)
	}

	agentServer := &AgentServer{
		db:             dbClient,
		tm:             tm,
		revocations:    auth.NewPGRevocationStore(dbClient.Pool),
		signer:         signer,
		sessions:       newRegistry(),
		log:            logger,
		probationDays:  platform.EnvInt("HOST_PROBATION_DAYS", 7),
		wgEndpoint:     platform.Env("WIREGUARD_ENDPOINT", ""),
		wgServerPubKey: platform.Env("WIREGUARD_SERVER_PUBKEY", ""),
	}

	// gRPC: keepalives detect dead NAT mappings on home/campus networks long
	// before TCP would; TLS is used whenever a certificate is configured.
	opts := []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 20 * time.Second, Timeout: 10 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	}
	if cert, key := os.Getenv("GRPC_TLS_CERT_FILE"), os.Getenv("GRPC_TLS_KEY_FILE"); cert != "" && key != "" {
		creds, err := credentials.NewServerTLSFromFile(cert, key)
		if err != nil {
			log.Fatalf("failed to load gRPC TLS credentials: %v", err)
		}
		opts = append(opts, grpc.Creds(creds))
	} else if platform.IsProduction() {
		log.Fatalf("configuration error: GRPC_TLS_CERT_FILE and GRPC_TLS_KEY_FILE are required in production")
	}

	lis, err := net.Listen("tcp", ":"+grpcPort)
	if err != nil {
		log.Fatalf("failed to listen on gRPC port %s: %v", grpcPort, err)
	}
	grpcServer := grpc.NewServer(opts...)
	agentv1.RegisterAgentServiceServer(grpcServer, agentServer)
	go func() {
		logger.Info("gRPC agent service listening", "port", grpcPort)
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("gRPC server error: %v", err)
		}
	}()
	defer grpcServer.GracefulStop()

	go agentServer.runLoops(ctx, platform.HeartbeatTimeout())

	srv, err := platform.NewServer(platform.ServiceConfig{Name: "coordinator", Version: "0.3.0", Port: port})
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	internal := svcAuth.RequireInternalService(auth.ServiceInferenceGateway, auth.ServiceControlAPI)
	srv.Mux.Handle("POST /internal/v1/infer", internal(http.HandlerFunc(agentServer.handleInfer)))
	srv.Mux.Handle("GET /internal/v1/connected", internal(http.HandlerFunc(agentServer.handleConnected)))

	srv.SetReadyCheck(func(ctx context.Context) error { return dbClient.Pool.Ping(ctx) })
	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
