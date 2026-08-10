// Package main implements the AyeusANN Coordinator.
// Responsibilities: persistent agent sessions, manifest dispatch, stage events.
package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	agentv1 "github.com/ayeus/ayeusann/gen/go/agent/v1"
	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/platform"
	"google.golang.org/grpc"
)

func main() {
	port, _ := strconv.Atoi(platform.MustEnv("COORDINATOR_PORT", "8083"))
	grpcPort := platform.MustEnv("COORDINATOR_GRPC_PORT", "50051")
	dbURL := platform.MustEnv("DATABASE_URL", "postgres://ayeusann:ayeusann_dev@localhost:5433/ayeusann?sslmode=disable")
	jwtSecret := platform.MustEnv("JWT_SECRET", "dev-secret-key-32-bytes-long-super-secure!")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbClient, err := db.NewClient(ctx, db.Config{URL: dbURL})
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer dbClient.Close()

	tm := auth.NewTokenManager(jwtSecret, 15*time.Minute, 7*24*time.Hour)

	// Start gRPC Server for Agent Sessions
	lis, err := net.Listen("tcp", ":"+grpcPort)
	if err != nil {
		log.Fatalf("failed to listen on gRPC port %s: %v", grpcPort, err)
	}

	grpcServer := grpc.NewServer()
	agentServer := NewAgentServer(dbClient, tm)
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
