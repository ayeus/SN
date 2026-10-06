package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	agentv1 "github.com/ayeus/ayeusann/gen/go/agent/v1"
	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/domain"
	"github.com/ayeus/ayeusann/internal/lifecycle"
	"github.com/ayeus/ayeusann/internal/manifest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Tier admission rules (PRD F-10, F-11).
const (
	minT2VramGB          = 16  // T2 = labs and workstations: a real discrete-class GPU
	minNvidiaDriverMajor = 535 // PRD F-11: NVIDIA R535+ enforced
	telemetryEvery       = 15 * time.Second
)

// AgentServer implements the agent gRPC service.
type AgentServer struct {
	agentv1.UnimplementedAgentServiceServer
	db             *db.Client
	tm             *auth.TokenManager
	revocations    auth.RevocationStore
	signer         *manifest.Signer
	sessions       *registry
	log            *slog.Logger
	probationDays  int
	wgEndpoint     string
	wgServerPubKey string
	// coordinatorURLs is pushed to every agent at registration (COORDINATOR_URLS).
	coordinatorURLs []string
	// allowFakeGPU admits hosts that report a simulated GPU (development and
	// end-to-end tests only).
	allowFakeGPU bool

	// Liveness bookkeeping, touched only by the control loop (loops.go).
	clock      func() time.Time // wall clock; replaced in tests
	lastTick   time.Time        // when the loop last completed a pass
	graceUntil time.Time        // no host is declared offline before this
}

// enrolment is the host row an agent session binds to.
type enrolment struct {
	hostID     string
	tier       string
	status     string
	overlayIP  string
	credential string // set only on first enrolment
}

func reject(stream agentv1.AgentService_SessionServer, code codes.Code, reason string) error {
	_ = stream.Send(&agentv1.CoordinatorMessage{
		Payload: &agentv1.CoordinatorMessage_RegisterResponse{
			RegisterResponse: &agentv1.RegisterResponse{Accepted: false, RejectionReason: reason},
		},
	})
	return status.Error(code, reason)
}

// Session is the long-lived agent stream.
func (s *AgentServer) Session(stream agentv1.AgentService_SessionServer) error {
	ctx := stream.Context()

	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to receive registration message: %v", err)
	}
	reg := first.GetRegister()
	if reg == nil {
		return status.Error(codes.InvalidArgument, "first message in session must be RegisterRequest")
	}

	var en *enrolment
	var reason string
	var code codes.Code
	switch {
	case reg.GetHostCredential() != "":
		en, code, reason = s.resume(ctx, reg)
	case reg.GetRegistrationToken() != "":
		en, code, reason = s.enrol(ctx, reg)
	default:
		code, reason = codes.Unauthenticated, "a registration token or host credential is required"
	}
	if reason != "" {
		s.log.Warn("agent registration rejected", "hostname", reg.GetHostname(), "reason", reason)
		return reject(stream, code, reason)
	}

	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		_, _ = s.db.Pool.Exec(ctx, `UPDATE hosts SET last_seen_ip = $2 WHERE id = $1;`, en.hostID, p.Addr.String())
	}

	if err := stream.Send(&agentv1.CoordinatorMessage{
		Payload: &agentv1.CoordinatorMessage_RegisterResponse{
			RegisterResponse: &agentv1.RegisterResponse{
				Accepted:          true,
				HostId:            en.hostID,
				OverlayIp:         en.overlayIP,
				WgPublicKey:       s.wgServerPubKey,
				WgEndpoint:        s.wgEndpoint,
				HostCredential:    en.credential,
				Tier:              en.tier,
				Status:            en.status,
				ManifestPublicKey: s.signer.PublicKey(),
				CoordinatorUrls:   s.coordinatorURLs,
			},
		},
	}); err != nil {
		return fmt.Errorf("failed to send RegisterResponse: %w", err)
	}

	// An agent that reconnects replaces its old stream. That one goes first,
	// so nothing more is sent down a line the agent has already left.
	s.sessions.drop(en.hostID)

	// Then agree with the agent on what it is running, before the dispatch
	// loop can see the new session and send it anything.
	orphans, err := s.reconcileOnRegister(ctx, en.hostID, reg)
	if err != nil {
		// Without the handshake nothing guarantees the two sides agree, and a
		// session installed anyway could leave replicas stranded. Ending the
		// stream makes the agent come back and try again.
		s.log.Error("failed to reconcile replicas on reconnect", "host_id", en.hostID, "err", err)
		return status.Error(codes.Unavailable, "the platform could not complete the handshake; reconnect")
	}

	sess := newSession(en.hostID, stream)
	if old := s.sessions.put(sess); old != nil {
		old.close()
	}
	s.log.Info("agent connected", "host_id", en.hostID, "tier", en.tier, "status", en.status,
		"runtime", reg.GetRuntime(), "still_serving", len(reg.GetReplicas()), "to_stop", len(orphans))

	defer func() {
		sess.close()
		if s.sessions.remove(sess) {
			s.log.Info("agent disconnected", "host_id", en.hostID)
		}
	}()

	// The agent is holding models the platform no longer wants there.
	for _, id := range orphans {
		if err := sess.send(&agentv1.CoordinatorMessage{Payload: &agentv1.CoordinatorMessage_StopReplica{
			StopReplica: &agentv1.StopReplica{ReplicaId: id, Reason: "no longer assigned to this host"},
		}}); err != nil {
			return err
		}
	}

	// Recv blocks until the agent speaks, so it runs on its own goroutine and
	// this one can also notice the session being closed from our side (a newer
	// session for the same host, or the host declared offline). Returning ends
	// the stream, which is what tells the agent to reconnect.
	type received struct {
		msg *agentv1.AgentMessage
		err error
	}
	inbox := make(chan received)
	go func() {
		for {
			msg, err := stream.Recv()
			select {
			case inbox <- received{msg, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var lastTelemetry time.Time
	for {
		var msg *agentv1.AgentMessage
		select {
		case <-sess.done:
			return status.Error(codes.Unavailable, "session closed by the platform; reconnect")
		case <-ctx.Done():
			// The agent went away. The receiving goroutine may have seen the
			// same thing and left without a word, so this is watched here too.
			return nil
		case r := <-inbox:
			if r.err != nil {
				if errors.Is(r.err, io.EOF) || status.Code(r.err) == codes.Canceled {
					return nil
				}
				return r.err
			}
			msg = r.msg
		}

		switch p := msg.Payload.(type) {
		case *agentv1.AgentMessage_Heartbeat:
			s.onHeartbeat(ctx, en.hostID, p.Heartbeat, &lastTelemetry)
		case *agentv1.AgentMessage_Benchmark:
			s.onBenchmark(ctx, en.hostID, p.Benchmark)
		case *agentv1.AgentMessage_StageEvent:
			s.onStage(ctx, en.hostID, p.StageEvent)
		case *agentv1.AgentMessage_InferenceChunk:
			sess.deliver(p.InferenceChunk, ctx.Done())
		case *agentv1.AgentMessage_DrainAck:
			s.log.Info("agent acknowledged drain", "host_id", en.hostID, "remaining", p.DrainAck.GetRemainingJobs())
		}
	}
}

// resume authenticates a reconnecting host by its credential.
func (s *AgentServer) resume(ctx context.Context, reg *agentv1.RegisterRequest) (*enrolment, codes.Code, string) {
	cred := reg.GetHostCredential()
	if !auth.ValidHostCredentialFormat(cred) {
		return nil, codes.Unauthenticated, "host credential is malformed"
	}

	var en enrolment
	var fingerprint *string
	err := s.db.Pool.QueryRow(ctx, `
		SELECT id, tier, status, COALESCE(HOST(overlay_ip), ''), hw_fingerprint
		FROM hosts WHERE credential_hash = $1 AND deleted_at IS NULL;
	`, auth.HashAPIKey(cred)).Scan(&en.hostID, &en.tier, &en.status, &en.overlayIP, &fingerprint)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, codes.Unauthenticated, "host credential not recognised; re-enrol this machine with a new registration token"
		}
		return nil, codes.Internal, "failed to look up host"
	}
	if en.status == domain.HostStatusBanned {
		return nil, codes.PermissionDenied, "this host has been banned from the network"
	}
	if reason := s.refuseGPUs(reg.GetGpus()); reason != "" {
		return nil, codes.FailedPrecondition, reason
	}

	// The credential is what proves which host this is. The fingerprint is a
	// description of the machine, and descriptions change: older agents built
	// it from the GPU and its driver version, so a routine driver update made
	// a machine look like a different one and locked it out for good. A valid
	// credential with a new fingerprint is therefore accepted and the record
	// brought up to date.
	//
	// The one case where the record is not updated is a fingerprint another
	// host already has. Two machines built from the same disk image share a
	// machine id, and a fingerprint is a string the agent chooses, so this
	// can be innocent or not; either way the host is not refused (that would
	// lock a machine out again, or let one host shut another out by claiming
	// its fingerprint first) and it keeps the fingerprint it had.
	newFP := reg.GetHardwareFingerprint()
	changed := newFP != "" && (fingerprint == nil || *fingerprint != newFP)
	if changed {
		var other string
		err := s.db.Pool.QueryRow(ctx, `
			SELECT id FROM hosts WHERE hw_fingerprint = $1 AND id <> $2 AND deleted_at IS NULL;
		`, newFP, en.hostID).Scan(&other)
		switch {
		case err == nil:
			s.log.Warn("two hosts report the same machine fingerprint (machines cloned from one image, or a copied data folder); keeping the stored one",
				"host_id", en.hostID, "other_host", other)
			newFP, changed = "", false
		case !errors.Is(err, pgx.ErrNoRows):
			return nil, codes.Internal, "failed to check the machine's identity"
		}
	}

	var lost []string
	err = s.db.ExecTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			UPDATE hosts
			SET hostname = $2, agent_version = $3, os = $4, runtime = NULLIF($5, ''),
			    cached_models = $6, last_heartbeat_at = NOW(), updated_at = NOW(),
			    hw_fingerprint = COALESCE(NULLIF($7, ''), hw_fingerprint),
			    status = CASE
			        WHEN status = 'offline' AND probation_until IS NOT NULL AND probation_until > NOW() THEN 'probation'
			        WHEN status = 'offline' THEN 'active'
			        ELSE status END
			WHERE id = $1
			RETURNING status;
		`, en.hostID, reg.GetHostname(), reg.GetAgentVersion(), reg.GetOs(), reg.GetRuntime(),
			nonNil(reg.GetCachedModels()), newFP).Scan(&en.status); err != nil {
			return err
		}
		var err error
		lost, err = syncGPUs(ctx, tx, en.hostID, reg.GetGpus())
		return err
	})
	if err != nil {
		if isUniqueViolation(err) {
			// Another host took that fingerprint between the check and the
			// update. The agent retries, and the check then sees it.
			return nil, codes.Unavailable, "the platform is busy; reconnect"
		}
		s.log.Error("host resume failed", "host_id", en.hostID, "err", err)
		return nil, codes.Internal, "failed to resume host session"
	}
	if changed && fingerprint != nil {
		s.log.Info("a host's machine fingerprint changed and was updated", "host_id", en.hostID)
	}
	if len(lost) > 0 {
		s.log.Warn("GPUs are no longer present on a host", "host_id", en.hostID, "gpus", lost)
	}
	return &en, codes.OK, ""
}

// enrol registers a new host with a single-use registration token.
func (s *AgentServer) enrol(ctx context.Context, reg *agentv1.RegisterRequest) (*enrolment, codes.Code, string) {
	claims, err := s.tm.VerifyRegistrationToken(reg.GetRegistrationToken())
	if err != nil {
		return nil, codes.Unauthenticated, "registration token is invalid or expired; generate a new one in the host console"
	}

	region := reg.GetRegion()
	if !domain.IsValidRegion(region) {
		return nil, codes.InvalidArgument, fmt.Sprintf("unknown region %q; use one of %s", region, strings.Join(domain.AllRegions, ", "))
	}
	if reason := s.refuseGPUs(reg.GetGpus()); reason != "" {
		return nil, codes.FailedPrecondition, reason
	}
	if reg.GetHardwareFingerprint() == "" {
		return nil, codes.InvalidArgument, "hardware fingerprint is required"
	}

	tier := strings.ToLower(claims.Tier)
	switch tier {
	case domain.TierT1, domain.TierT2, domain.TierT3:
	case "":
		tier = domain.TierT3
	default:
		return nil, codes.InvalidArgument, "registration token carries an unknown tier"
	}
	if tier == domain.TierT2 && maxVram(reg.GetGpus()) < minT2VramGB {
		return nil, codes.FailedPrecondition, fmt.Sprintf(
			"Tier 2 needs a GPU with at least %d GB of VRAM; this machine qualifies for Tier 3 (personal) instead", minT2VramGB)
	}

	// A machine belongs to one account. Re-enrolling it under the same account
	// rotates its credential; under another account it is refused. The machine
	// is recognised by its fingerprint, or by one an older agent computed for
	// it, so upgrading the agent and enrolling again does not make a second
	// host out of the same computer.
	known := append([]string{reg.GetHardwareFingerprint()}, reg.GetLegacyFingerprints()...)
	var existingID, existingUser *string
	err = s.db.Pool.QueryRow(ctx, `
		SELECT id::TEXT, user_id::TEXT FROM hosts
		WHERE hw_fingerprint = ANY($1) AND deleted_at IS NULL
		ORDER BY (user_id::TEXT = $3) DESC, (hw_fingerprint = $2) DESC, created_at ASC LIMIT 1;
	`, known, reg.GetHardwareFingerprint(), claims.UserID).Scan(&existingID, &existingUser)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, codes.Internal, "failed to check existing enrolment"
	}
	if existingID != nil && (existingUser == nil || *existingUser != claims.UserID) {
		return nil, codes.PermissionDenied, "this machine is already enrolled to another account"
	}

	// Consume only after every validation passed, so a typo does not burn the
	// token. The unique jti makes this atomic under concurrent redemption.
	if s.revocations != nil {
		if err := s.revocations.Consume(ctx, claims.TokenID(), claims.ExpiresAt.Time); err != nil {
			if errors.Is(err, auth.ErrTokenAlreadyUsed) {
				return nil, codes.Unauthenticated, "this registration token has already been used; generate a new one"
			}
			return nil, codes.Internal, "failed to consume registration token"
		}
	}

	raw, hash, err := auth.GenerateHostCredential()
	if err != nil {
		return nil, codes.Internal, "failed to issue host credential"
	}

	en := enrolment{tier: tier, status: domain.HostStatusBenchmarking, credential: raw}
	name := reg.GetHostname()
	if name == "" {
		fp := strings.TrimPrefix(reg.GetHardwareFingerprint(), "sha256:")
		if len(fp) > 8 {
			fp = fp[:8]
		}
		name = "host-" + fp
	}

	err = s.db.ExecTx(ctx, func(tx pgx.Tx) error {
		var err error
		if existingID != nil {
			// The account's own record of this machine: rotate its credential
			// and bring it up to date. Its fingerprint moves to the current
			// one unless another host already has that (see resume).
			err = tx.QueryRow(ctx, `
				UPDATE hosts h SET
				    tier = $2, region = $3, hostname = $4,
				    status = CASE WHEN h.status = 'banned' THEN 'banned' ELSE 'benchmarking' END,
				    hw_fingerprint = CASE WHEN EXISTS (
				            SELECT 1 FROM hosts o WHERE o.hw_fingerprint = $5 AND o.id <> h.id)
				        THEN h.hw_fingerprint ELSE $5 END,
				    agent_version = $6, os = $7, runtime = NULLIF($8, ''), cached_models = $9,
				    credential_hash = $10, last_heartbeat_at = NOW(), updated_at = NOW()
				WHERE h.id = $1::UUID
				RETURNING h.id, h.status;
			`, *existingID, tier, region, name, reg.GetHardwareFingerprint(), reg.GetAgentVersion(), reg.GetOs(),
				reg.GetRuntime(), nonNil(reg.GetCachedModels()), hash,
			).Scan(&en.hostID, &en.status)
		} else {
			// A new machine. The conflict clause is only for two enrolments of
			// the same machine racing each other, and it never reaches into
			// another account's record.
			err = tx.QueryRow(ctx, `
				INSERT INTO hosts (user_id, org_id, name, hostname, tier, region, kyc_status, reputation, status,
				                   hw_fingerprint, agent_version, os, runtime, cached_models, credential_hash, last_heartbeat_at)
				VALUES ($1, NULLIF($2, '')::UUID, $3, $3, $4, $5, 'pending', 50, 'benchmarking',
				        $6, $7, $8, NULLIF($9, ''), $10, $11, NOW())
				ON CONFLICT (hw_fingerprint) WHERE hw_fingerprint IS NOT NULL DO UPDATE SET
				    tier = EXCLUDED.tier,
				    region = EXCLUDED.region,
				    hostname = EXCLUDED.hostname,
				    status = CASE WHEN hosts.status = 'banned' THEN 'banned' ELSE 'benchmarking' END,
				    agent_version = EXCLUDED.agent_version,
				    os = EXCLUDED.os,
				    runtime = EXCLUDED.runtime,
				    cached_models = EXCLUDED.cached_models,
				    credential_hash = EXCLUDED.credential_hash,
				    last_heartbeat_at = NOW(),
				    updated_at = NOW()
				WHERE hosts.user_id = EXCLUDED.user_id
				RETURNING id, status;
			`, claims.UserID, claims.OrgID, name, tier, region,
				reg.GetHardwareFingerprint(), reg.GetAgentVersion(), reg.GetOs(), reg.GetRuntime(),
				nonNil(reg.GetCachedModels()), hash,
			).Scan(&en.hostID, &en.status)
			if errors.Is(err, pgx.ErrNoRows) {
				return errOtherAccount
			}
		}
		if err != nil {
			return fmt.Errorf("register host: %w", err)
		}
		if en.status == domain.HostStatusBanned {
			return errBanned
		}

		if err := tx.QueryRow(ctx, `SELECT allocate_overlay_ip($1::UUID)::TEXT;`, en.hostID).Scan(&en.overlayIP); err != nil {
			return fmt.Errorf("allocate overlay IP: %w", err)
		}
		en.overlayIP = strings.SplitN(en.overlayIP, "/", 2)[0]
		if _, err := tx.Exec(ctx, `UPDATE hosts SET overlay_ip = $1::INET WHERE id = $2;`, en.overlayIP, en.hostID); err != nil {
			return fmt.Errorf("store overlay IP: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE host_registration_tokens SET consumed_at = NOW(), consumed_by_host = $1
			WHERE jti = $2 AND consumed_at IS NULL;
		`, en.hostID, claims.TokenID()); err != nil {
			return fmt.Errorf("record token consumption: %w", err)
		}
		_, err = syncGPUs(ctx, tx, en.hostID, reg.GetGpus())
		return err
	})
	if err != nil {
		if errors.Is(err, errBanned) {
			return nil, codes.PermissionDenied, "this machine has been banned from the network"
		}
		if errors.Is(err, errOtherAccount) {
			return nil, codes.PermissionDenied, "this machine is already enrolled to another account"
		}
		s.log.Error("host enrolment failed", "err", err)
		return nil, codes.Internal, "failed to register host"
	}
	return &en, codes.OK, ""
}

var (
	errBanned       = errors.New("host is banned")
	errOtherAccount = errors.New("machine is enrolled to another account")
)

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// syncGPUs makes the host's GPU records match what the agent reports now.
// GPUs are attributes of a machine, refreshed at every connection: a driver
// update changes a row, a new card adds one, and a card that is gone is marked
// unavailable so nothing is placed on it. A replica that was running on a card
// that is gone is failed, so the scheduler places it somewhere real. It
// returns the UUIDs of the GPUs that went missing.
func syncGPUs(ctx context.Context, tx pgx.Tx, hostID string, gpus []*agentv1.GpuInfo) ([]string, error) {
	present := make([]string, 0, len(gpus))
	for _, g := range gpus {
		present = append(present, g.GetUuid())
		if _, err := tx.Exec(ctx, `
			INSERT INTO gpus (host_id, model, vram_gb, driver_version, cuda_version, uuid, fingerprint, status)
			VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6, $7, 'available')
			ON CONFLICT (host_id, uuid) DO UPDATE SET
			    model = EXCLUDED.model,
			    vram_gb = EXCLUDED.vram_gb,
			    driver_version = EXCLUDED.driver_version,
			    cuda_version = EXCLUDED.cuda_version,
			    fingerprint = EXCLUDED.fingerprint,
			    status = CASE WHEN gpus.status = 'unavailable' THEN 'available' ELSE gpus.status END;
		`, hostID, g.GetModel(), g.GetVramGb(), g.GetDriverVersion(), g.GetCudaVersion(), g.GetUuid(), g.GetFingerprint()); err != nil {
			return nil, fmt.Errorf("register GPU %s: %w", g.GetUuid(), err)
		}
	}

	rows, err := tx.Query(ctx, `
		SELECT g.id, g.uuid, r.id
		FROM gpus g
		LEFT JOIN replicas r ON r.gpu_id = g.id AND r.state NOT IN ('stopped', 'failed')
		WHERE g.host_id = $1 AND g.uuid <> ALL($2) AND (g.status <> 'unavailable' OR r.id IS NOT NULL);
	`, hostID, present)
	if err != nil {
		return nil, err
	}
	type gone struct {
		id, uuid string
		replica  *string
	}
	var missing []gone
	for rows.Next() {
		var g gone
		if err := rows.Scan(&g.id, &g.uuid, &g.replica); err != nil {
			rows.Close()
			return nil, err
		}
		missing = append(missing, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var lost []string
	deps := map[string]bool{}
	seen := map[string]bool{}
	for _, g := range missing {
		if g.replica != nil {
			depID, _, err := lifecycle.SetReplicaState(ctx, tx, *g.replica, lifecycle.Failed, "",
				"the GPU this replica ran on is no longer present on the host")
			if err != nil {
				return nil, err
			}
			if depID != "" {
				deps[depID] = true
			}
		}
		if seen[g.id] {
			continue
		}
		seen[g.id] = true
		// After the replica is failed: failing it releases the GPU, and this
		// has to be the last word on the GPU's status.
		if _, err := tx.Exec(ctx, `UPDATE gpus SET status = 'unavailable', replica_id = NULL WHERE id = $1;`, g.id); err != nil {
			return nil, err
		}
		lost = append(lost, g.uuid)
	}
	for depID := range deps {
		if _, _, err := lifecycle.Recompute(ctx, tx, depID); err != nil {
			return nil, err
		}
	}
	return lost, nil
}

// validateGPUs enforces the agent-side refusals of SRS FR-50 on the server too:
// a host must report at least one sane GPU, and NVIDIA hosts need R535+.
func validateGPUs(gpus []*agentv1.GpuInfo) string {
	if len(gpus) == 0 {
		return "no supported GPU was detected on this machine"
	}
	for _, g := range gpus {
		if g.GetUuid() == "" {
			return "a GPU was reported without a UUID"
		}
		if g.GetVramGb() <= 0 || g.GetVramGb() > 1024 {
			return fmt.Sprintf("GPU %s reported an implausible %d GB of VRAM", g.GetModel(), g.GetVramGb())
		}
		if strings.Contains(strings.ToUpper(g.GetModel()), "NVIDIA") && g.GetDriverVersion() != "" {
			major, err := strconv.Atoi(strings.SplitN(g.GetDriverVersion(), ".", 2)[0])
			if err == nil && major < minNvidiaDriverMajor {
				return fmt.Sprintf("NVIDIA driver %s is too old; R%d or newer is required", g.GetDriverVersion(), minNvidiaDriverMajor)
			}
		}
	}
	return ""
}

// fakeGPUPrefix marks the simulated GPU an agent reports under --fake-gpu.
const fakeGPUPrefix = "GPU-fake-"

// refuseGPUs returns why this machine's hardware may not join, or "".
//
// The simulated-GPU check is a guard against a mistake (a development agent
// pointed at a real installation), not a defence: a host that wants to lie
// about its hardware is the trust engine's problem, not this function's.
func (s *AgentServer) refuseGPUs(gpus []*agentv1.GpuInfo) string {
	if reason := validateGPUs(gpus); reason != "" {
		return reason
	}
	if !s.allowFakeGPU {
		for _, g := range gpus {
			if strings.HasPrefix(g.GetUuid(), fakeGPUPrefix) {
				return "this agent is reporting a simulated GPU (--fake-gpu or SN_FAKE_GPU), which only a development platform accepts; run it without that setting"
			}
		}
	}
	return ""
}

func maxVram(gpus []*agentv1.GpuInfo) int32 {
	var m int32
	for _, g := range gpus {
		if g.GetVramGb() > m {
			m = g.GetVramGb()
		}
	}
	return m
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (s *AgentServer) onHeartbeat(ctx context.Context, hostID string, hb *agentv1.Heartbeat, lastTelemetry *time.Time) {
	if _, err := s.db.Pool.Exec(ctx, `
		UPDATE hosts
		SET last_heartbeat_at = NOW(),
		    runtime_healthy = $2,
		    cached_models = $3,
		    status = CASE
		        WHEN status = 'offline' AND probation_until IS NOT NULL AND probation_until > NOW() THEN 'probation'
		        WHEN status = 'offline' THEN 'active'
		        ELSE status END
		WHERE id = $1;
	`, hostID, hb.GetRuntimeHealthy(), nonNil(hb.GetCachedModels())); err != nil {
		s.log.Error("heartbeat update failed", "host_id", hostID, "err", err)
		return
	}

	// Persisting every 5 s heartbeat would write 17k rows per host per day for
	// no analytical gain; one sample per 15 s is enough for uptime and charts.
	if time.Since(*lastTelemetry) < telemetryEvery {
		return
	}
	*lastTelemetry = time.Now()
	gpuStatus, _ := json.Marshal(hb.GetGpuStatus())
	if len(gpuStatus) == 0 || string(gpuStatus) == "null" {
		gpuStatus = []byte("[]")
	}
	if _, err := s.db.Pool.Exec(ctx, `
		INSERT INTO host_telemetry (host_id, cpu_usage_pct, memory_usage_pct, gpu_status, active_jobs, host_user_active)
		VALUES ($1, $2, $3, $4, $5, $6);
	`, hostID, hb.GetCpuUsagePct(), hb.GetMemoryUsagePct(), gpuStatus, hb.GetActiveJobs(), hb.GetHostUserActive()); err != nil {
		s.log.Error("telemetry insert failed", "host_id", hostID, "err", err)
	}
}

// onBenchmark stores results and advances the host lifecycle (UML §5):
// BENCHMARKING → ACTIVE for T1, → PROBATION for T2/T3.
func (s *AgentServer) onBenchmark(ctx context.Context, hostID string, bm *agentv1.BenchmarkReport) {
	var gpuID *string
	insert := func(gb *agentv1.GpuBenchmark) {
		gpuID = nil
		if gb != nil {
			var id string
			if err := s.db.Pool.QueryRow(ctx, `SELECT id FROM gpus WHERE host_id = $1 AND uuid = $2;`, hostID, gb.GetGpuUuid()).Scan(&id); err == nil {
				gpuID = &id
			}
		}
		if _, err := s.db.Pool.Exec(ctx, `
			INSERT INTO benchmarks (host_id, gpu_id, score_compute, vram_bw_gbps, disk_read_mbps, disk_write_mbps,
			                        net_up_mbps, net_down_mbps, latency_pop_ms, hw_fingerprint)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);
		`, hostID, gpuID, positive(gb.GetComputeScore()), positive(gb.GetVramBandwidthGbps()),
			positive(bm.GetDiskReadMbps()), positive(bm.GetDiskWriteMbps()), positive(bm.GetNetUploadMbps()),
			positive(bm.GetNetDownloadMbps()), positive(bm.GetLatencyToPopMs()), bm.GetHardwareFingerprint()); err != nil {
			s.log.Error("benchmark insert failed", "host_id", hostID, "err", err)
		}
	}
	if len(bm.GetGpuBenchmarks()) == 0 {
		insert(nil)
	}
	for _, gb := range bm.GetGpuBenchmarks() {
		insert(gb)
	}

	if _, err := s.db.Pool.Exec(ctx, `
		UPDATE hosts
		SET benchmark_completed_at = NOW(),
		    probation_until = CASE WHEN tier = 't1' OR $2 <= 0 THEN NULL ELSE NOW() + ($2 * INTERVAL '1 day') END,
		    status = CASE WHEN tier = 't1' OR $2 <= 0 THEN 'active' ELSE 'probation' END,
		    updated_at = NOW()
		WHERE id = $1 AND status IN ('registered', 'benchmarking');
	`, hostID, s.probationDays); err != nil {
		s.log.Error("host status transition failed", "host_id", hostID, "err", err)
	}
}

func positive(v float64) *float64 {
	if v <= 0 {
		return nil
	}
	return &v
}

var replicaStateNames = map[agentv1.ReplicaState]string{
	agentv1.ReplicaState_REPLICA_STATE_PENDING:  lifecycle.Pending,
	agentv1.ReplicaState_REPLICA_STATE_PULLING:  lifecycle.Pulling,
	agentv1.ReplicaState_REPLICA_STATE_LOADING:  lifecycle.Loading,
	agentv1.ReplicaState_REPLICA_STATE_WARMING:  lifecycle.Warming,
	agentv1.ReplicaState_REPLICA_STATE_SERVING:  lifecycle.Serving,
	agentv1.ReplicaState_REPLICA_STATE_DEGRADED: lifecycle.Degraded,
	agentv1.ReplicaState_REPLICA_STATE_STOPPING: lifecycle.Stopping,
	agentv1.ReplicaState_REPLICA_STATE_STOPPED:  lifecycle.Stopped,
	agentv1.ReplicaState_REPLICA_STATE_FAILED:   lifecycle.Failed,
}

// onStage applies a replica stage event and rolls it up into the deployment.
func (s *AgentServer) onStage(ctx context.Context, hostID string, se *agentv1.StageEvent) {
	state, ok := replicaStateNames[se.GetState()]
	if !ok {
		return
	}
	err := s.db.ExecTx(ctx, func(tx pgx.Tx) error {
		// A host may only report on replicas assigned to it.
		var owner string
		if err := tx.QueryRow(ctx, `SELECT host_id FROM replicas WHERE id = $1;`, se.GetReplicaId()).Scan(&owner); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if owner != hostID {
			s.log.Warn("host reported on a replica it does not own", "host_id", hostID, "replica_id", se.GetReplicaId())
			return nil
		}
		depID, _, err := lifecycle.SetReplicaState(ctx, tx, se.GetReplicaId(), state, se.GetDetail(), se.GetErrorMessage())
		if err != nil || depID == "" {
			return err
		}
		if se.GetImageHash() != "" || se.GetModelHash() != "" {
			if _, err := tx.Exec(ctx, `
				UPDATE replicas SET image_hash = COALESCE(NULLIF($2, ''), image_hash),
				                    model_hash = COALESCE(NULLIF($3, ''), model_hash)
				WHERE id = $1;
			`, se.GetReplicaId(), se.GetImageHash(), se.GetModelHash()); err != nil {
				return err
			}
		}
		_, _, err = lifecycle.Recompute(ctx, tx, depID)
		return err
	})
	if err != nil {
		s.log.Error("stage event failed", "replica_id", se.GetReplicaId(), "state", state, "err", err)
	}
}

// capReplicaReport is the capability of an agent that lists, when it
// registers, the replicas it is still serving.
const capReplicaReport = "replica-report"

// reconcileOnRegister brings the platform's view of a host and the host's own
// view back together when it connects.
//
// The connection between them drops for ordinary reasons: Wi-Fi, a platform
// restart, a laptop lid. The models stay loaded through all of them, so:
//
//   - a replica both sides agree is serving is left exactly as it is;
//   - a replica the platform expects but the agent does not have is sent again;
//   - a replica the agent holds but the platform has given up on (stopped,
//     failed, moved elsewhere) is returned, for the caller to stop.
//
// An agent without the replica-report capability says nothing about what it
// holds, so every job is sent again, as before.
func (s *AgentServer) reconcileOnRegister(ctx context.Context, hostID string, reg *agentv1.RegisterRequest) (orphans []string, err error) {
	reports := false
	for _, c := range reg.GetCapabilities() {
		if c == capReplicaReport {
			reports = true
		}
	}
	held := map[string]bool{}
	if reports {
		for _, r := range reg.GetReplicas() {
			held[r.GetReplicaId()] = true
		}
	}

	err = s.db.ExecTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, state FROM replicas
			WHERE host_id = $1 AND state IN ('pending', 'pulling', 'loading', 'warming', 'serving', 'degraded', 'stopping');
		`, hostID)
		if err != nil {
			return err
		}
		type rep struct{ id, state string }
		var expected []rep
		for rows.Next() {
			var r rep
			if err := rows.Scan(&r.id, &r.state); err != nil {
				rows.Close()
				return err
			}
			expected = append(expected, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		deps := map[string]bool{}
		for _, r := range expected {
			var depID string
			var err error
			switch {
			case r.state == lifecycle.Stopping:
				// The stop may have gone to the old connection; send it again.
				// If the agent still holds the replica it is also told now, as
				// an orphan: the control loop can close a stopping replica out
				// in the instant this host has no session, and would then
				// never send the stop.
				_, err = tx.Exec(ctx, `UPDATE replicas SET stop_sent_at = NULL WHERE id = $1;`, r.id)
				if err == nil && held[r.id] {
					continue
				}
			case held[r.id] && r.state == lifecycle.Serving:
				// Both sides agree. Nothing to do, and nothing is done.
			case held[r.id]:
				// It kept serving while we could not hear it (or its last
				// report was lost with the old connection).
				depID, _, err = lifecycle.SetReplicaState(ctx, tx, r.id, lifecycle.Serving, "host reconnected; still serving", "")
			default:
				depID, _, err = lifecycle.SetReplicaState(ctx, tx, r.id, lifecycle.Pending, "host reconnected; re-sending job", "")
				if err == nil {
					_, err = tx.Exec(ctx, `UPDATE replicas SET dispatched_at = NULL WHERE id = $1;`, r.id)
				}
			}
			if err != nil {
				return err
			}
			if depID != "" {
				deps[depID] = true
			}
			delete(held, r.id)
		}
		for depID := range deps {
			if _, _, err := lifecycle.Recompute(ctx, tx, depID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Whatever is left was reported by the agent and is not expected of it.
	for id := range held {
		orphans = append(orphans, id)
	}
	sort.Strings(orphans)
	return orphans, nil
}
