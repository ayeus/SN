package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	agentv1 "github.com/spazor/spazenode/gen/go/agent/v1"
	"github.com/spazor/spazenode/internal/auth"
	"github.com/spazor/spazenode/internal/db"
	"github.com/spazor/spazenode/internal/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type AgentServer struct {
	agentv1.UnimplementedAgentServiceServer
	db         *db.Client
	tm         *auth.TokenManager
	ipCounter  uint32
	sessions   sync.Map
}

func NewAgentServer(database *db.Client, tm *auth.TokenManager) *AgentServer {
	return &AgentServer{
		db:        database,
		tm:        tm,
		ipCounter: 10, // Starts at 10.200.0.10
	}
}

func (s *AgentServer) Session(stream agentv1.AgentService_SessionServer) error {
	ctx := stream.Context()

	// 1. Receive first message — MUST be RegisterRequest
	firstMsg, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to receive registration message: %v", err)
	}

	regReq := firstMsg.GetRegister()
	if regReq == nil {
		return status.Error(codes.InvalidArgument, "first message in session MUST be RegisterRequest")
	}

	// 2. Verify registration token
	claims, err := s.tm.VerifyToken(regReq.RegistrationToken)
	if err != nil {
		_ = stream.Send(&agentv1.CoordinatorMessage{
			Payload: &agentv1.CoordinatorMessage_RegisterResponse{
				RegisterResponse: &agentv1.RegisterResponse{
					Accepted:        false,
					RejectionReason: "Invalid or expired registration token",
				},
			},
		})
		return status.Error(codes.Unauthenticated, "invalid registration token")
	}

	// Determine host tier based on GPUs: >=24GB VRAM = T1/T2, else T3
	tier := domain.TierT3
	for _, gpu := range regReq.Gpus {
		if gpu.VramGb >= 24 {
			tier = domain.TierT2
			break
		}
	}

	// Allocate next overlay IP in 10.200.0.0/16 mesh range
	ipOffset := atomic.AddUint32(&s.ipCounter, 1)
	overlayIP := fmt.Sprintf("10.200.%d.%d", (ipOffset>>8)&0xFF, ipOffset&0xFF)

	region := regReq.Region
	if region == "" {
		region = domain.RegionInSouth
	}

	// 3. Save Host and GPUs to Database
	var hostID string
	err = s.db.ExecTx(ctx, func(tx pgx.Tx) error {
		// Upsert Host
		hostQuery := `
			INSERT INTO hosts (user_id, name, hostname, tier, region, overlay_ip, kyc_status, reputation, status, hw_fingerprint, agent_version, last_heartbeat_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'verified', 50, 'active', $7, $8, NOW())
			ON CONFLICT (id) DO UPDATE SET
				status = 'active',
				agent_version = EXCLUDED.agent_version,
				last_heartbeat_at = NOW()
			RETURNING id;
		`
		err := tx.QueryRow(ctx, hostQuery,
			claims.UserID, regReq.Hostname, regReq.Hostname, tier, region,
			overlayIP, regReq.HardwareFingerprint, regReq.AgentVersion,
		).Scan(&hostID)
		if err != nil {
			return fmt.Errorf("failed to register host in DB: %w", err)
		}

		// Upsert GPUs
		gpuQuery := `
			INSERT INTO gpus (host_id, model, vram_gb, driver_version, cuda_version, uuid, fingerprint, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'available')
			ON CONFLICT DO NOTHING;
		`
		for _, gpu := range regReq.Gpus {
			_, err := tx.Exec(ctx, gpuQuery,
				hostID, gpu.Model, gpu.VramGb, gpu.DriverVersion, gpu.CudaVersion,
				gpu.Uuid, gpu.Fingerprint,
			)
			if err != nil {
				return fmt.Errorf("failed to register GPU %s in DB: %w", gpu.Uuid, err)
			}
		}

		return nil
	})

	if err != nil {
		_ = stream.Send(&agentv1.CoordinatorMessage{
			Payload: &agentv1.CoordinatorMessage_RegisterResponse{
				RegisterResponse: &agentv1.RegisterResponse{
					Accepted:        false,
					RejectionReason: err.Error(),
				},
			},
		})
		return status.Errorf(codes.Internal, "database registration error: %v", err)
	}

	// 4. Send RegisterResponse Success
	err = stream.Send(&agentv1.CoordinatorMessage{
		Payload: &agentv1.CoordinatorMessage_RegisterResponse{
			RegisterResponse: &agentv1.RegisterResponse{
				Accepted:     true,
				HostId:       hostID,
				OverlayIp:    overlayIP,
				WgPublicKey:  "wg_pub_key_placeholder",
				WgEndpoint:   "10.200.0.1:51820",
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to send RegisterResponse: %w", err)
	}

	s.sessions.Store(hostID, stream)
	defer s.sessions.Delete(hostID)

	// 5. Handle ongoing heartbeat stream
	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		if hb := msg.GetHeartbeat(); hb != nil {
			// Update host heartbeat timestamp in Postgres
			updateQuery := `UPDATE hosts SET last_heartbeat_at = NOW(), status = 'active' WHERE id = $1;`
			_, _ = s.db.Pool.Exec(ctx, updateQuery, hostID)
		}
	}
}

func parseIP(ipStr string) net.IP {
	return net.ParseIP(ipStr)
}
