package lifecycle_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ayeus/ayeusann/internal/lifecycle"
	"github.com/ayeus/ayeusann/internal/testutil"
	"github.com/jackc/pgx/v5"
)

// Two replicas of one deployment report stage changes at the same moment, as
// two hosts do when a model finishes loading on both. Every report must land:
// a dropped one leaves the replica looking stuck forever.
func TestConcurrentStageEventsAllLand(t *testing.T) {
	client := testutil.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var orgID, modelID, depID string
	if err := client.Pool.QueryRow(ctx, `INSERT INTO organizations (name) VALUES ('LifecycleRaceOrg') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := client.Pool.QueryRow(ctx, `SELECT id FROM models LIMIT 1`).Scan(&modelID); err != nil {
		t.Fatalf("model: %v", err)
	}
	if err := client.Pool.QueryRow(ctx, `
		INSERT INTO deployments (org_id, model_id, name, tier, region, min_replicas, max_replicas)
		VALUES ($1, $2, 'race', 't2', 'IN-SOUTH', 2, 2) RETURNING id`, orgID, modelID).Scan(&depID); err != nil {
		t.Fatalf("deployment: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = client.Pool.Exec(c, `DELETE FROM deployment_events WHERE deployment_id = $1`, depID)
		_, _ = client.Pool.Exec(c, `DELETE FROM replicas WHERE deployment_id = $1`, depID)
		_, _ = client.Pool.Exec(c, `DELETE FROM deployments WHERE id = $1`, depID)
		_, _ = client.Pool.Exec(c, `DELETE FROM hosts WHERE name LIKE 'race-host-%'`)
		_, _ = client.Pool.Exec(c, `DELETE FROM organizations WHERE id = $1`, orgID)
	})

	stages := []string{lifecycle.Pulling, lifecycle.Loading, lifecycle.Warming, lifecycle.Serving}
	for round := 0; round < 15; round++ {
		replicas := make([]string, 2)
		for i := range replicas {
			var hostID string
			if err := client.Pool.QueryRow(ctx, `
				INSERT INTO hosts (name, tier, region) VALUES ('race-host-' || gen_random_uuid(), 't2', 'IN-SOUTH') RETURNING id`).Scan(&hostID); err != nil {
				t.Fatalf("host: %v", err)
			}
			if err := client.Pool.QueryRow(ctx, `
				INSERT INTO replicas (deployment_id, host_id, state) VALUES ($1, $2, 'pending') RETURNING id`, depID, hostID).Scan(&replicas[i]); err != nil {
				t.Fatalf("replica: %v", err)
			}
		}

		var wg sync.WaitGroup
		errs := make(chan error, len(replicas)*len(stages))
		for _, rid := range replicas {
			wg.Add(1)
			go func(rid string) {
				defer wg.Done()
				for _, st := range stages {
					errs <- client.ExecTx(ctx, func(tx pgx.Tx) error {
						dep, _, err := lifecycle.SetReplicaState(ctx, tx, rid, st, "", "")
						if err != nil {
							return err
						}
						_, _, err = lifecycle.Recompute(ctx, tx, dep)
						return err
					})
				}
			}(rid)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: stage event lost: %v", round, err)
			}
		}

		var serving int
		var state string
		_ = client.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM replicas WHERE id = ANY($1) AND state = 'serving'`, replicas).Scan(&serving)
		_ = client.Pool.QueryRow(ctx, `SELECT state FROM deployments WHERE id = $1`, depID).Scan(&state)
		if serving != 2 || state != lifecycle.Serving {
			t.Fatalf("round %d: serving replicas = %d, deployment = %s; want 2, serving", round, serving, state)
		}
		if _, err := client.Pool.Exec(ctx, `UPDATE replicas SET state = 'stopped' WHERE id = ANY($1)`, replicas); err != nil {
			t.Fatalf("reset: %v", err)
		}
		if _, err := client.Pool.Exec(ctx, `UPDATE deployments SET state = 'pending' WHERE id = $1`, depID); err != nil {
			t.Fatalf("reset: %v", err)
		}
	}
}
