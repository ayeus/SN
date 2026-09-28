package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)


func TestSelectReplica_SingleCandidate(t *testing.T) {
	engine := &RouterEngine{
		health:     newReplicaHealth(),
		maxRetries: 2,
	}

	candidates := []candidateReplica{
		{ID: "r1", DeploymentID: "d1", HostID: "h1", OverlayIP: "10.200.0.1", InferencePort: 8000, Tier: "t1"},
	}

	selected, err := engine.selectReplica(candidates, nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if selected.ID != "r1" {
		t.Fatalf("expected replica r1, got: %s", selected.ID)
	}
}

func TestSelectReplica_ExcludesUnhealthy(t *testing.T) {
	engine := &RouterEngine{
		health:     newReplicaHealth(),
		maxRetries: 2,
	}

	// Mark r1 as unhealthy
	engine.health.markUnhealthy("r1")

	candidates := []candidateReplica{
		{ID: "r1", DeploymentID: "d1", HostID: "h1", OverlayIP: "10.200.0.1", InferencePort: 8000, Tier: "t1"},
		{ID: "r2", DeploymentID: "d1", HostID: "h2", OverlayIP: "10.200.0.2", InferencePort: 8000, Tier: "t2"},
	}

	selected, err := engine.selectReplica(candidates, nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if selected.ID != "r2" {
		t.Fatalf("expected replica r2 (healthy), got: %s", selected.ID)
	}
}

func TestSelectReplica_ExcludesExplicit(t *testing.T) {
	engine := &RouterEngine{
		health:     newReplicaHealth(),
		maxRetries: 2,
	}

	candidates := []candidateReplica{
		{ID: "r1", DeploymentID: "d1", HostID: "h1", OverlayIP: "10.200.0.1", InferencePort: 8000, Tier: "t1"},
		{ID: "r2", DeploymentID: "d1", HostID: "h2", OverlayIP: "10.200.0.2", InferencePort: 8000, Tier: "t2"},
	}

	excluded := map[string]bool{"r1": true}
	selected, err := engine.selectReplica(candidates, excluded)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if selected.ID != "r2" {
		t.Fatalf("expected replica r2, got: %s", selected.ID)
	}
}

func TestSelectReplica_NoHealthyCandidates(t *testing.T) {
	engine := &RouterEngine{
		health:     newReplicaHealth(),
		maxRetries: 2,
	}

	engine.health.markUnhealthy("r1")
	engine.health.markUnhealthy("r2")

	candidates := []candidateReplica{
		{ID: "r1", DeploymentID: "d1", HostID: "h1", OverlayIP: "10.200.0.1", InferencePort: 8000, Tier: "t1"},
		{ID: "r2", DeploymentID: "d1", HostID: "h2", OverlayIP: "10.200.0.2", InferencePort: 8000, Tier: "t2"},
	}

	_, err := engine.selectReplica(candidates, nil)
	if err != ErrNoHealthyReplica {
		t.Fatalf("expected ErrNoHealthyReplica, got: %v", err)
	}
}

func TestSelectReplica_LeastConnections(t *testing.T) {
	engine := &RouterEngine{
		health:     newReplicaHealth(),
		maxRetries: 2,
	}

	// Simulate r1 having more inflight requests
	engine.health.incrementReqCount("r1")
	engine.health.incrementReqCount("r1")
	engine.health.incrementReqCount("r1")

	candidates := []candidateReplica{
		{ID: "r1", DeploymentID: "d1", HostID: "h1", OverlayIP: "10.200.0.1", InferencePort: 8000, Tier: "t1"},
		{ID: "r2", DeploymentID: "d1", HostID: "h2", OverlayIP: "10.200.0.2", InferencePort: 8000, Tier: "t1"},
	}

	selected, err := engine.selectReplica(candidates, nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if selected.ID != "r2" {
		t.Fatalf("expected r2 (fewer connections), got: %s", selected.ID)
	}
}

func TestReplicaHealth_MarkAndCheck(t *testing.T) {
	rh := newReplicaHealth()

	// Default should be healthy
	if !rh.isHealthy("r1") {
		t.Fatal("new replica should default to healthy")
	}

	rh.markUnhealthy("r1")
	if rh.isHealthy("r1") {
		t.Fatal("replica should be unhealthy after markUnhealthy")
	}

	rh.markHealthy("r1")
	if !rh.isHealthy("r1") {
		t.Fatal("replica should be healthy after markHealthy")
	}
}

func TestReplicaHealth_ReqCountTracking(t *testing.T) {
	rh := newReplicaHealth()

	if rh.getReqCount("r1") != 0 {
		t.Fatal("initial req count should be 0")
	}

	rh.incrementReqCount("r1")
	rh.incrementReqCount("r1")
	if rh.getReqCount("r1") != 2 {
		t.Fatalf("expected req count 2, got %d", rh.getReqCount("r1"))
	}

	rh.decrementReqCount("r1")
	if rh.getReqCount("r1") != 1 {
		t.Fatalf("expected req count 1, got %d", rh.getReqCount("r1"))
	}

	// Should not go below 0
	rh.decrementReqCount("r1")
	rh.decrementReqCount("r1")
	if rh.getReqCount("r1") != 0 {
		t.Fatalf("expected req count 0 (no negative), got %d", rh.getReqCount("r1"))
	}
}

func TestInferenceEngineAdapter(t *testing.T) {
	// 1. Test genuine inference execution and real token extraction via httptest server
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-test-real-123",
			"object": "chat.completion",
			"created": 1700000000,
			"model": "llama-3.1-8b-instruct",
			"choices": [{
				"index": 0,
				"message": {"role": "assistant", "content": "Real response from model."},
				"finish_reason": "stop"
			}],
			"usage": {
				"prompt_tokens": 14,
				"completion_tokens": 28,
				"total_tokens": 42
			}
		}`))
	}))
	defer backend.Close()

	engine := NewHTTPInferenceEngine(backend.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := engine.Generate(ctx, WorkerChatRequest{
		Model: "llama-3.1-8b-instruct",
		Messages: []WorkerMessage{
			{Role: "user", Content: "Hello world"},
		},
	})
	if err != nil {
		t.Fatalf("expected successful inference, got: %v", err)
	}
	if resp.Choices[0].Message.Content != "Real response from model." {
		t.Fatalf("unexpected content: %s", resp.Choices[0].Message.Content)
	}
	if resp.Usage.PromptTokens != 14 || resp.Usage.CompletionTokens != 28 || resp.Usage.TotalTokens != 42 {
		t.Fatalf("unexpected token usage: %+v", resp.Usage)
	}

	// 2. Test explicit failure when backend is offline (never fake success)
	offlineEngine := NewHTTPInferenceEngine("http://127.0.0.1:59999/v1", "")
	_, err = offlineEngine.Generate(ctx, WorkerChatRequest{
		Model: "llama-3.1-8b-instruct",
		Messages: []WorkerMessage{
			{Role: "user", Content: "Fail test"},
		},
	})
	if err == nil {
		t.Fatal("expected error when inference backend is unreachable, got nil")
	}
}

