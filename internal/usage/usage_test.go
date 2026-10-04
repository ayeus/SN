package usage_test

import (
	"context"
	"testing"

	"github.com/ayeus/ayeusann/internal/testutil"
	"github.com/ayeus/ayeusann/internal/usage"
	"github.com/google/uuid"
)

// llama-3.1-8b-instruct from the seed catalogue.
const catalogModel = "550e8400-e29b-41d4-a716-446655440001"

func TestRecordCountsEachRequestOnce(t *testing.T) {
	client := testutil.DB(t)
	defer client.Close()
	ctx := context.Background()

	var org, host, dep, replica string
	scan := func(dest *string, sql string, args ...any) {
		t.Helper()
		if err := client.Pool.QueryRow(ctx, sql, args...).Scan(dest); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	scan(&org, `INSERT INTO organizations (name) VALUES ('UsageRecordOrg') RETURNING id`)
	scan(&host, `INSERT INTO hosts (name, tier, region) VALUES ('usage-' || gen_random_uuid(), 't3', 'IN-SOUTH') RETURNING id`)
	scan(&dep, `INSERT INTO deployments (org_id, model_id, name, tier, region) VALUES ($1, $2, 'usage-' || gen_random_uuid(), 't3', 'IN-SOUTH') RETURNING id`, org, catalogModel)
	scan(&replica, `INSERT INTO replicas (deployment_id, host_id, state) VALUES ($1, $2, 'serving') RETURNING id`, dep, host)

	ok := usage.Event{
		RequestID: uuid.NewString(), OrgID: org, DeploymentID: dep, ReplicaID: replica, HostID: host,
		ModelID: catalogModel, Tier: "t3", InputTokens: 12, OutputTokens: 30, GPUSeconds: 1.5, DurationMs: 1500,
	}
	if dup, err := usage.Record(ctx, client, ok); err != nil || dup {
		t.Fatalf("first record: duplicate=%v err=%v", dup, err)
	}
	// A retried write of the same request must not count twice.
	if dup, err := usage.Record(ctx, client, ok); err != nil || !dup {
		t.Fatalf("second record of the same request: duplicate=%v err=%v, want duplicate", dup, err)
	}

	failed := ok
	failed.RequestID, failed.Status, failed.OutputTokens = uuid.NewString(), usage.StatusError, 0
	if _, err := usage.Record(ctx, client, failed); err != nil {
		t.Fatalf("failed request: %v", err)
	}

	var events, tokens, total, failures int
	if err := client.Pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(input_tokens + output_tokens), 0) FROM usage_events WHERE deployment_id = $1`, dep).Scan(&events, &tokens); err != nil {
		t.Fatal(err)
	}
	if events != 2 || tokens != 12+30+12 {
		t.Fatalf("usage_events: %d rows, %d tokens; want 2 rows, 54 tokens", events, tokens)
	}
	if err := client.Pool.QueryRow(ctx, `SELECT total_requests, failed_requests FROM replicas WHERE id = $1`, replica).Scan(&total, &failures); err != nil {
		t.Fatal(err)
	}
	if total != 2 || failures != 1 {
		t.Fatalf("replica counters = %d total, %d failed; want 2 and 1", total, failures)
	}

	if _, err := usage.Record(ctx, client, usage.Event{RequestID: uuid.NewString()}); err == nil {
		t.Fatal("an event without its identifiers must be rejected")
	}
}
