// Package domain defines core domain entities and data structures for AyeusANN.
package domain

import (
	"encoding/json"
	"net"
	"time"
)

// Role constants
const (
	RoleAdmin   = "admin"
	RoleMember  = "member"
	RoleBilling = "billing"
)

// Tier constants
const (
	TierT1 = "t1" // Data Centres (99.5% SLA)
	TierT2 = "t2" // College/Computer Labs (99% SLA)
	TierT3 = "t3" // Personal Laptops (Best-effort / Spot)
)

// Region constants
const (
	RegionInSouth = "IN-SOUTH" // Chennai primary
	RegionInWest  = "IN-WEST"  // Mumbai secondary
)

// Deployment states
const (
	StatePending    = "pending"
	StateScheduling = "scheduling"
	StatePulling    = "pulling"
	StateLoading    = "loading"
	StateWarming    = "warming"
	StateServing    = "serving"
	StateDegraded   = "degraded"
	StatePaused     = "paused"
	StateStopping   = "stopping"
	StateStopped    = "stopped"
	StateFailed     = "failed"
)

// Host status
const (
	HostStatusRegistered   = "registered"
	HostStatusBenchmarking = "benchmarking"
	HostStatusProbation    = "probation"
	HostStatusActive       = "active"
	HostStatusDraining     = "draining"
	HostStatusOffline      = "offline"
	HostStatusDemoted      = "demoted"
	HostStatusBanned       = "banned"
)

// Ledger entry kinds
const (
	LedgerKindTopup        = "topup"
	LedgerKindDebit        = "debit"
	LedgerKindCredit       = "credit"
	LedgerKindRefund       = "refund"
	LedgerKindSignupCredit = "signup_credit"
)

// ─── 1. PriceBook ─────────────────────────────────────────────

type PriceBook struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description *string   `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type PriceBookEntry struct {
	ID             string   `json:"id"`
	PriceBookID    string   `json:"price_book_id"`
	GpuModel       string   `json:"gpu_model"`
	Tier           string   `json:"tier"`
	PricePerHour   float64  `json:"price_per_hour"`
	PriceInPer1M   *float64 `json:"price_in_per_1m,omitempty"`
	PriceOutPer1M  *float64 `json:"price_out_per_1m,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// ─── 2. User ──────────────────────────────────────────────────

type User struct {
	ID            string     `json:"id"`
	Email         string     `json:"email"`
	PasswordHash  *string    `json:"-"` // Never serialized in JSON
	Name          string     `json:"name"`
	PhoneEnc      []byte     `json:"-"` // Column-encrypted
	AuthProvider  string     `json:"auth_provider"`
	EmailVerified bool       `json:"email_verified"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	DeletedAt     *time.Time `json:"deleted_at,omitempty"`
}

// ─── 3. Organization ──────────────────────────────────────────

type Organization struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	GstinEnc      []byte     `json:"-"` // Column-encrypted
	DefaultRegion string     `json:"default_region"`
	PriceBookID   *string    `json:"price_book_id,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	DeletedAt     *time.Time `json:"deleted_at,omitempty"`
}

// ─── 4. Membership ────────────────────────────────────────────

type Membership struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	OrgID     string    `json:"org_id"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// ─── 5. ApiKey ────────────────────────────────────────────────

type ApiKey struct {
	ID           string     `json:"id"`
	OrgID        string     `json:"org_id"`
	Name         string     `json:"name"`
	Prefix       string     `json:"prefix"`
	Hash         string     `json:"-"` // SHA-256 hash, secret
	Scope        string     `json:"scope"`
	DeploymentID *string    `json:"deployment_id,omitempty"`
	Permissions  []string   `json:"permissions"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	Revoked      bool       `json:"revoked"`
	CreatedAt    time.Time  `json:"created_at"`
}

// ─── 6. Model & ModelArtifact ─────────────────────────────────

type Model struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	Family             string          `json:"family"`
	ParamsB            float32         `json:"params_b"`
	License            string          `json:"license"`
	MinVramGB          int             `json:"min_vram_gb"`
	TiersAllowed       []string        `json:"tiers_allowed"`
	PriceInPer1M       float64         `json:"price_in_per_1m"`
	PriceOutPer1M      float64         `json:"price_out_per_1m"`
	PricePerHourINR    *float64        `json:"price_per_hour_inr,omitempty"`
	IsBYO              bool            `json:"is_byo"`
	QuantizationPresets json.RawMessage `json:"quantization_presets"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

type ModelArtifact struct {
	ID          string    `json:"id"`
	ModelID     string    `json:"model_id"`
	Version     string    `json:"version"`
	ArtifactURL string    `json:"artifact_url"`
	SizeBytes   int64     `json:"size_bytes"`
	Checksum    string    `json:"checksum"`
	Format      string    `json:"format"`
	CreatedAt   time.Time `json:"created_at"`
}

// ─── 7. Host & GPU ────────────────────────────────────────────

type Host struct {
	ID              string     `json:"id"`
	UserID          *string    `json:"user_id,omitempty"`
	Name            string     `json:"name"`
	Hostname        *string    `json:"hostname,omitempty"`
	Tier            string     `json:"tier"`
	Region          string     `json:"region"`
	OverlayIP       *net.IP    `json:"overlay_ip,omitempty"`
	KycStatus       string     `json:"kyc_status"`
	Reputation      int        `json:"reputation"`
	Status          string     `json:"status"`
	HwFingerprint   *string    `json:"hw_fingerprint,omitempty"`
	PanEnc          []byte     `json:"-"` // Column-encrypted
	BankEnc         []byte     `json:"-"` // Column-encrypted
	AgentVersion    *string    `json:"agent_version,omitempty"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	DeletedAt       *time.Time `json:"deleted_at,omitempty"`
}

type GPU struct {
	ID            string    `json:"id"`
	HostID        string    `json:"host_id"`
	Model         string    `json:"model"`
	VramGB        int       `json:"vram_gb"`
	DriverVersion *string   `json:"driver_version,omitempty"`
	CudaVersion   *string   `json:"cuda_version,omitempty"`
	UUID          string    `json:"uuid"`
	Fingerprint   *string   `json:"fingerprint,omitempty"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

// ─── 8. Benchmark & Reputation Snapshot ───────────────────────

type Benchmark struct {
	ID            string    `json:"id"`
	HostID        string    `json:"host_id"`
	GpuID         *string   `json:"gpu_id,omitempty"`
	ScoreCompute  float32   `json:"score_compute"`
	VramBwGbps    *float32  `json:"vram_bw_gbps,omitempty"`
	DiskReadMbps  *float32  `json:"disk_read_mbps,omitempty"`
	DiskWriteMbps *float32  `json:"disk_write_mbps,omitempty"`
	NetUpMbps     *float32  `json:"net_up_mbps,omitempty"`
	NetDownMbps   *float32  `json:"net_down_mbps,omitempty"`
	LatencyPopMs  *float32  `json:"latency_pop_ms,omitempty"`
	HwFingerprint string    `json:"hw_fingerprint"`
	RanAt         time.Time `json:"ran_at"`
}

type ReputationSnapshot struct {
	ID                 string    `json:"id"`
	HostID             string    `json:"host_id"`
	Score              int       `json:"score"`
	UptimePct          float32   `json:"uptime_pct"`
	CorrectnessPct     *float32  `json:"correctness_pct,omitempty"`
	BenchmarkStability *float32  `json:"benchmark_stability,omitempty"`
	AgeDays            int       `json:"age_days"`
	IncidentRate       *float32  `json:"incident_rate,omitempty"`
	ComputedAt         time.Time `json:"computed_at"`
}

// ─── 9. Deployment & Replica ──────────────────────────────────

type Deployment struct {
	ID           string          `json:"id"`
	OrgID        string          `json:"org_id"`
	ModelID      string          `json:"model_id"`
	Name         string          `json:"name"`
	State        string          `json:"state"`
	Tier         string          `json:"tier"`
	Region       string          `json:"region"`
	MinReplicas  int             `json:"min_replicas"`
	MaxReplicas  int             `json:"max_replicas"`
	ScaleToZero  bool            `json:"scale_to_zero"`
	BurstToSpot  bool            `json:"burst_to_spot"`
	ResidentIn   bool            `json:"resident_in"`
	Endpoint     *string         `json:"endpoint,omitempty"`
	Quantization *string         `json:"quantization,omitempty"`
	ConfigJSON   json.RawMessage `json:"config_json"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	DeletedAt    *time.Time      `json:"deleted_at,omitempty"`
}

type Replica struct {
	ID            string     `json:"id"`
	DeploymentID  string     `json:"deployment_id"`
	HostID        string     `json:"host_id"`
	GpuID         *string    `json:"gpu_id,omitempty"`
	State         string     `json:"state"`
	OverlayIP     *net.IP    `json:"overlay_ip,omitempty"`
	InferencePort *int       `json:"inference_port,omitempty"`
	ImageHash     *string    `json:"image_hash,omitempty"`
	ModelHash     *string    `json:"model_hash,omitempty"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	StoppedAt     *time.Time `json:"stopped_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// ─── 10. UsageEvent (Partitioned, Append-Only) ────────────────

type UsageEvent struct {
	RequestID      string    `json:"request_id"` // Idempotency Key (UUID)
	DeploymentID   string    `json:"deployment_id"`
	ReplicaID      string    `json:"replica_id"`
	HostID         string    `json:"host_id"`
	InputTokens    int       `json:"input_tokens"`
	OutputTokens   int       `json:"output_tokens"`
	GpuSeconds     float64   `json:"gpu_seconds"`
	Tier           string    `json:"tier"`
	AmountCustomer float64   `json:"amount_customer"` // USD charged to customer
	AmountHost     float64   `json:"amount_host"`     // USD accrued to host (75%)
	Timestamp      time.Time `json:"ts"`
	Status         string    `json:"status"`
}

// ─── 11. Billing: WalletLedger, Invoice, PayoutBatch ──────────

type WalletLedger struct {
	EntryID      string    `json:"entry_id"`
	OrgID        string    `json:"org_id"`
	Delta        float64   `json:"delta"`         // Positive = credit, negative = debit
	BalanceAfter float64   `json:"balance_after"` // Running balance
	Kind         string    `json:"kind"`          // topup|debit|credit|refund|signup_credit
	RefID        *string   `json:"ref_id,omitempty"`
	Description  *string   `json:"description,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

type Invoice struct {
	ID          string        `json:"id"`
	OrgID       string        `json:"org_id"`
	Number      string        `json:"number"`
	PeriodStart time.Time     `json:"period_start"`
	PeriodEnd   time.Time     `json:"period_end"`
	Subtotal    float64       `json:"subtotal"`
	GstAmount   float64       `json:"gst_amount"`
	Total       float64       `json:"total"`
	Status      string        `json:"status"`
	PdfURL      *string       `json:"pdf_url,omitempty"`
	Lines       []InvoiceLine `json:"lines,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
}

type InvoiceLine struct {
	ID          string    `json:"id"`
	InvoiceID   string    `json:"invoice_id"`
	Description string    `json:"description"`
	Quantity    float64   `json:"quantity"`
	UnitPrice   float64   `json:"unit_price"`
	Amount      float64   `json:"amount"`
	CreatedAt   time.Time `json:"created_at"`
}

type PayoutBatch struct {
	ID        string       `json:"id"`
	WeekStart time.Time    `json:"week_start"`
	WeekEnd   time.Time    `json:"week_end"`
	Gross     float64      `json:"gross"`
	Tds       float64      `json:"tds"` // TDS 1% under Section 194-O
	Net       float64      `json:"net"`
	RzpxRef   *string      `json:"rzpx_ref,omitempty"`
	Status    string       `json:"status"`
	Lines     []PayoutLine `json:"lines,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
}

type PayoutLine struct {
	ID             string    `json:"id"`
	BatchID        string    `json:"batch_id"`
	HostID         string    `json:"host_id"`
	Gross          float64   `json:"gross"`
	TdsAmount      float64   `json:"tds_amount"`
	Net            float64   `json:"net"`
	Status         string    `json:"status"`
	RzpxTransferID *string   `json:"rzpx_transfer_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type TaxDoc struct {
	ID          string    `json:"id"`
	HostID      string    `json:"host_id"`
	DocType     string    `json:"doc_type"` // form_16a | tds_certificate
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`
	PdfURL      *string   `json:"pdf_url,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// ─── 12. Trust & Operations ───────────────────────────────────

type TrustIncident struct {
	ID           string          `json:"id"`
	HostID       string          `json:"host_id"`
	Kind         string          `json:"kind"` // mismatch|spoof|abuse|downtime|benchmark_drift
	EvidenceJSON json.RawMessage `json:"evidence_json"`
	Severity     string          `json:"severity"` // low|medium|high|critical
	Action       *string         `json:"action,omitempty"`
	Resolved     bool            `json:"resolved"`
	CreatedAt    time.Time       `json:"created_at"`
	ResolvedAt   *time.Time      `json:"resolved_at,omitempty"`
}

type AuditLog struct {
	ID           string          `json:"id"`
	ActorID      string          `json:"actor_id"`
	ActorType    string          `json:"actor_type"` // user|system|admin|agent
	Action       string          `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceID   *string         `json:"resource_id,omitempty"`
	DetailsJSON  json.RawMessage `json:"details_json,omitempty"`
	IPAddress    *net.IP         `json:"ip_address,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}
