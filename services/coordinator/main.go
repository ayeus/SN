// Package main implements the AyeusANN Coordinator.
// Responsibilities: persistent agent sessions, manifest dispatch, stage events.
package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"time"

	agentv1 "github.com/ayeus/ayeusann/gen/go/agent/v1"
	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/platform"
	"google.golang.org/grpc"
)

const devJWTSecret = "dev-only-insecure-jwt-signing-key-0001"

func main() {
	port := platform.EnvInt("COORDINATOR_PORT", 8083)
	grpcPort := platform.Env("COORDINATOR_GRPC_PORT", "50051")

	dbURL, err := platform.RequireEnv("DATABASE_URL",
		"postgres://ayeusann:ayeusann_dev@localhost:5433/ayeusann?sslmode=disable")
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	jwtSecret, err := platform.RequireSecret("JWT_SECRET", devJWTSecret, 32)
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbClient, err := db.NewClient(ctx, db.Config{URL: dbURL})
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer dbClient.Close()

	tm, err := auth.NewTokenManager(jwtSecret, 15*time.Minute, 7*24*time.Hour)
	if err != nil {
		log.Fatalf("failed to initialize token manager: %v", err)
	}
	revocations := auth.NewPGRevocationStore(dbClient.Pool)

	// Start gRPC Server for Agent Sessions
	lis, err := net.Listen("tcp", ":"+grpcPort)
	if err != nil {
		log.Fatalf("failed to listen on gRPC port %s: %v", grpcPort, err)
	}

	grpcServer := grpc.NewServer()
	agentServer := NewAgentServer(dbClient, tm, revocations)
	agentv1.RegisterAgentServiceServer(grpcServer, agentServer)

	go func() {
		log.Printf("gRPC AgentServer listening on :%s", grpcPort)
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("gRPC server error: %v", err)
		}
	}()

	// Start HTTP Platform Server (/healthz, /metrics)
	srv, err := platform.NewServer(platform.ServiceConfig{
		Name:    "coordinator",
		Version: "0.1.0",
		Port:    port,
	})
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	srv.Mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"AyeusANN-coordinator","version":"0.1.0"}`))
	})

	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
