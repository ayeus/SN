// Package main implements the AyeusANN Request Router.
// Responsibilities: replica discovery, load-aware selection, health checks, retries, request proxying.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/platform"
)

func main() {
	port, _ := strconv.Atoi(platform.MustEnv("ROUTER_PORT", "8084"))
	dbURL := platform.MustEnv("DATABASE_URL", "postgres://ayeusann:ayeusann_dev@localhost:5433/ayeusann?sslmode=disable")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbClient, err := db.NewClient(ctx, db.Config{URL: dbURL})
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer dbClient.Close()

	engine := NewRouterEngine(dbClient)

	// Start local HTTP ReplicaWorker on port 8000
	worker := NewReplicaWorker(8000, "host-node-local", "t2")
	if err := worker.Start(); err != nil {
		log.Printf("failed to start local replica worker: %v", err)
	} else {
		log.Println("Started local HTTP ReplicaWorker on port 8000")
	}

	// Start background health checker (every 10 seconds)
	healthCtx, healthCancel := context.WithCancel(context.Background())
	defer healthCancel()
	engine.StartHealthChecker(healthCtx, 10*time.Second)

	srv, err := platform.NewServer(platform.ServiceConfig{
		Name:    "router",
		Version: "0.2.0",
		Port:    port,
	})
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	// POST /v1/route — Route an inference request to the best replica
	srv.Mux.HandleFunc("POST /v1/route", func(w http.ResponseWriter, r *http.Request) {
		var req RouteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeRouterError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		if req.Model == "" {
			writeRouterError(w, http.StatusBadRequest, "Model name is required")
			return
		}
		if req.OrgID == "" {
			writeRouterError(w, http.StatusBadRequest, "Org ID is required")
			return
		}

		resp, err := engine.RouteAndProxy(r.Context(), req)
		if err != nil {
			writeRouterError(w, http.StatusServiceUnavailable, err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_ = json.NewEncoder(w).Encode(resp)
	})

	// GET /v1/replicas?deployment_id=... — List serving replicas for a deployment
	srv.Mux.HandleFunc("GET /v1/replicas", func(w http.ResponseWriter, r *http.Request) {
		deploymentID := r.URL.Query().Get("deployment_id")
		if deploymentID == "" {
			writeRouterError(w, http.StatusBadRequest, "deployment_id query parameter is required")
			return
		}

		replicas, err := engine.discoverReplicas(r.Context(), deploymentID)
		if err != nil {
			writeRouterError(w, http.StatusInternalServerError, err.Error())
			return
		}

		// Annotate with health status
		type replicaInfo struct {
			candidateReplica
			Healthy bool `json:"healthy"`
		}
		result := make([]replicaInfo, len(replicas))
		for i, r := range replicas {
			result[i] = replicaInfo{
				candidateReplica: r,
				Healthy:          engine.health.isHealthy(r.ID),
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"deployment_id": deploymentID,
			"replicas":      result,
			"count":         len(result),
		})
	})

	srv.Mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"AyeusANN-router","version":"0.2.0"}`))
	})

	srv.SetReady()
	if err := srv.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func writeRouterError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
