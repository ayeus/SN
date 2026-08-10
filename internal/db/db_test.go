package db_test

import (
	"os"
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ayeus/ayeusann/internal/crypto"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/domain"
)

func getTestDBURL() string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://ayeusann:ayeusann_dev@localhost:5433/ayeusann?sslmode=disable"
}

func setupTestClient(t *testing.T) *db.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := db.NewClient(ctx, db.Config{
		URL:      getTestDBURL(),
		MaxConns: 5,
	})
	if err != nil {
		t.Fatalf("Failed to connect to test database (%s): %v", getTestDBURL(), err)
	}

	return client
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

	// Setup dummy org, user, host, model, deployment, replica for FK requirements
	var orgID, modelID, hostID, deploymentID, replicaID string

	_ = client.Pool.QueryRow(ctx, `INSERT INTO organizations (name) VALUES ('UsageTestOrg') RETURNING id`).Scan(&orgID)
	_ = client.Pool.QueryRow(ctx, `INSERT INTO models (name, family, params_b, license, min_vram_gb, price_in_per_1m, price_out_per_1m) VALUES ($1, 'llama', 8, 'mit', 16, 0.08, 0.22) RETURNING id`, "model-"+uuid.New().String()).Scan(&modelID)
	_ = client.Pool.QueryRow(ctx, `INSERT INTO hosts (name, tier, region) VALUES ('TestHost', 't1', 'IN-SOUTH') RETURNING id`).Scan(&hostID)
	_ = client.Pool.QueryRow(ctx, `INSERT INTO deployments (org_id, model_id, name, tier, region) VALUES ($1, $2, 'Dep1', 't1', 'IN-SOUTH') RETURNING id`, orgID, modelID).Scan(&deploymentID)
	_ = client.Pool.QueryRow(ctx, `INSERT INTO replicas (deployment_id, host_id, state) VALUES ($1, $2, 'serving') RETURNING id`, deploymentID, hostID).Scan(&replicaID)

	requestID := uuid.New().String()
	ts := time.Now()

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

func TestLedgerServiceTransactions(t *testing.T) {
	client := setupTestClient(t)
	defer client.Close()

	ctx := context.Background()
	ledger := db.NewLedgerService(client)

	// Create test org
	var orgID string
	err := client.Pool.QueryRow(ctx, `INSERT INTO organizations (name) VALUES ('LedgerTestOrg') RETURNING id`).Scan(&orgID)
	if err != nil {
		t.Fatalf("Failed to create org: %v", err)
	}

	// 1. Initial balance should be 0
	bal, err := ledger.GetBalance(ctx, orgID)
	if err != nil {
		t.Fatalf("GetBalance error: %v", err)
	}
	if bal != 0 {
		t.Fatalf("Expected initial balance 0, got %f", bal)
	}

	// 2. Record Topup ($50 credit)
	ref1 := uuid.New().String()
	desc1 := "Initial Topup"
	entry1, err := ledger.RecordTransaction(ctx, orgID, 50.00, domain.LedgerKindTopup, &ref1, &desc1, false)
	if err != nil {
		t.Fatalf("RecordTransaction topup error: %v", err)
	}

	if entry1.BalanceAfter != 50.00 {
		t.Fatalf("Expected balance_after 50.00, got %f", entry1.BalanceAfter)
	}

	// 3. Record Debit ($15)
	ref2 := uuid.New().String()
	desc2 := "Inference Usage"
	entry2, err := ledger.RecordTransaction(ctx, orgID, -15.00, domain.LedgerKindDebit, &ref2, &desc2, false)
	if err != nil {
		t.Fatalf("RecordTransaction debit error: %v", err)
	}

	if entry2.BalanceAfter != 35.00 {
		t.Fatalf("Expected balance_after 35.00, got %f", entry2.BalanceAfter)
	}

	// 4. Record Excessive Debit ($40) when balance is $35 -> Should fail with ErrInsufficientBalance
	_, err = ledger.RecordTransaction(ctx, orgID, -40.00, domain.LedgerKindDebit, nil, nil, false)
	if err == nil || err != db.ErrInsufficientBalance {
		t.Fatalf("Expected ErrInsufficientBalance, got %v", err)
	}

	// Balance should remain $35.00
	balFinal, err := ledger.GetBalance(ctx, orgID)
	if err != nil {
		t.Fatalf("GetBalance error: %v", err)
	}
	if balFinal != 35.00 {
		t.Fatalf("Expected balance to remain 35.00, got %f", balFinal)
	}
}
