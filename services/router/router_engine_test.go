package main

import (
	"testing"
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

func TestSimulatedReplicaResponse(t *testing.T) {
	engine := &RouterEngine{
		health:     newReplicaHealth(),
		maxRetries: 2,
	}

	replica := &candidateReplica{
		ID:       "abc12345-1234-1234-1234-1234567890ab",
		HostID:   "def12345-1234-1234-1234-1234567890ab",
		Tier:     "t2",
	}

	resp, err := engine.simulatedReplicaResponse(replica)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected status 200, got: %d", resp.StatusCode)
	}
	if resp.ReplicaID != replica.ID {
		t.Fatalf("expected replica ID %s, got: %s", replica.ID, resp.ReplicaID)
	}
	if resp.Body == "" {
		t.Fatal("expected non-empty body")
	}
}
