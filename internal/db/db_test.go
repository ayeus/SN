package db_test

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/ayeus/ayeusann/internal/crypto"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/domain"
	"github.com/ayeus/ayeusann/internal/testutil"
	"github.com/google/uuid"
)

// setupTestClient connects to the isolated test database (never the dev one);
// see testutil.DB for skip-versus-fail behaviour.
func setupTestClient(t *testing.T) *db.Client {
	t.Helper()
	return testutil.DB(t)
}

func TestDatabasePing(t *testing.T) {
	client := setupTestClient(t)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}
}

func TestOrgAndUserCreationWithPIIEncryption(t *testing.T) {
	client := setupTestClient(t)
	defer client.Close()

	ctx := context.Background()

	// Initialize crypto cipher with 32-byte test key
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cipher, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher error: %v", err)
	}

	// 1. Create User with encrypted phone
	phonePlain := "+919876543210"
	phoneEnc, err := cipher.EncryptString(phonePlain)
	if err != nil {
		t.Fatalf("Failed to encrypt phone: %v", err)
	}

	userEmail := "test-" + uuid.New().String() + "@AyeusANN.io"
	var userID string
	err = client.Pool.QueryRow(ctx, `
		INSERT INTO users (email, name, phone_enc, auth_provider)
		VALUES ($1, $2, $3, $4)
		RETURNING id;
	`, userEmail, "Test User", phoneEnc, "email").Scan(&userID)

	if err != nil {
		t.Fatalf("Failed to insert user: %v", err)
	}

	// Retrieve user and decrypt phone
	var fetchedPhoneEnc []byte
	var fetchedName string
	err = client.Pool.QueryRow(ctx, `
		SELECT name, phone_enc FROM users WHERE id = $1;
	`, userID).Scan(&fetchedName, &fetchedPhoneEnc)

	if err != nil {
		t.Fatalf("Failed to query user: %v", err)
	}

	decryptedPhone, err := cipher.DecryptString(fetchedPhoneEnc)
	if err != nil {
		t.Fatalf("Failed to decrypt phone: %v", err)
	}

	if decryptedPhone != phonePlain {
		t.Fatalf("Expected decrypted phone %q, got %q", phonePlain, decryptedPhone)
	}

	// 2. Create Organization
	orgName := "Test Org " + uuid.New().String()
	var orgID string
	err = client.Pool.QueryRow(ctx, `
		INSERT INTO organizations (name, default_region)
		VALUES ($1, $2)
		RETURNING id;
	`, orgName, domain.RegionInSouth).Scan(&orgID)

	if err != nil {
		t.Fatalf("Failed to insert org: %v", err)
	}

	if orgID == "" {
		t.Fatal("Expected non-empty org ID")
	}
}

func TestUsageEventAppendOnlyPartitioning(t *testing.T) {
	client := setupTestClient(t)
	defer client.Close()

	ctx := context.Background()

	// Setup dummy org, user, host, deployment, replica for FK requirements
	var orgID, hostID, deploymentID, replicaID string
	modelID := "550e8400-e29b-41d4-a716-446655440001" // Genuine catalog model

	_ = client.Pool.QueryRow(ctx, `INSERT INTO organizations (name) VALUES ('UsageTestOrg') RETURNING id`).Scan(&orgID)
	_ = client.Pool.QueryRow(ctx, `INSERT INTO hosts (name, tier, region) VALUES ('TestHost', 't1', 'IN-SOUTH') RETURNING id`).Scan(&hostID)
	_ = client.Pool.QueryRow(ctx, `INSERT INTO deployments (org_id, model_id, name, tier, region) VALUES ($1, $2, 'Dep1', 't1', 'IN-SOUTH') RETURNING id`, orgID, modelID).Scan(&deploymentID)
	_ = client.Pool.QueryRow(ctx, `INSERT INTO replicas (deployment_id, host_id, state) VALUES ($1, $2, 'serving') RETURNING id`, deploymentID, hostID).Scan(&replicaID)

	requestID := uuid.New().String()
	ts := time.Now()

	defer func() {
		_, _ = client.Pool.Exec(context.Background(), "DELETE FROM usage_events WHERE request_id = $1", requestID)
		_, _ = client.Pool.Exec(context.Background(), "DELETE FROM replicas WHERE id = $1", replicaID)
		_, _ = client.Pool.Exec(context.Background(), "DELETE FROM deployments WHERE id = $1", deploymentID)
		_, _ = client.Pool.Exec(context.Background(), "DELETE FROM hosts WHERE id = $1", hostID)
		_, _ = client.Pool.Exec(context.Background(), "DELETE FROM organizations WHERE id = $1", orgID)
	}()

	// Insert into partitioned usage_events
	query := `
		INSERT INTO usage_events (
			request_id, deployment_id, replica_id, host_id,
			input_tokens, output_tokens, gpu_seconds, tier,
			amount_customer, amount_host, ts, status
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);
	`

	_, err := client.Pool.Exec(ctx, query,
		requestID, deploymentID, replicaID, hostID,
		100, 200, 1.5, domain.TierT1,
		0.0001, 0.000075, ts, "success",
	)

	if err != nil {
		t.Fatalf("Failed to insert usage_event: %v", err)
	}

	// Verify idempotency constraint on (request_id, ts)
	_, err = client.Pool.Exec(ctx, query,
		requestID, deploymentID, replicaID, hostID,
		100, 200, 1.5, domain.TierT1,
		0.0001, 0.000075, ts, "success",
	)

	if err == nil {
		t.Fatal("Expected error when inserting duplicate (request_id, ts), got nil")
	}
}
