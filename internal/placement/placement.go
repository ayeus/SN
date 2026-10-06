// Package placement decides which host GPU should run a replica. It implements
// the scheduler activity in UML §8 and SRS FR-41:
//
//	filter  → vram ≥ model, tier ≥ requested, region match, online, runtime ok
//	score   → 0.35·reputation + 0.25·price + 0.20·locality + 0.20·cache-warmth
//	spread  → replicas of one deployment land on distinct hosts (FR-43)
//
// The package is pure apart from LoadCandidates, so the rules are unit-tested
// without a database.
package placement

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ayeus/ayeusann/internal/db"
)

// Score weights from SRS FR-41 / Architecture §4.
const (
	WeightReputation = 0.35
	WeightPrice      = 0.25
	WeightLocality   = 0.20
	WeightCache      = 0.20
)

// MinReputation is the floor below which a host receives no new work.
const MinReputation = 40

// tierRank orders tiers by trust: a deployment asking for T2 may run on T1 or
// T2, never on T3 (FR-41 "tier ≥ requested").
var tierRank = map[string]int{"t3": 1, "t2": 2, "t1": 3}

// TierSatisfies reports whether a host tier meets a requested tier.
func TierSatisfies(hostTier, requested string) bool {
	h, ok1 := tierRank[strings.ToLower(hostTier)]
	r, ok2 := tierRank[strings.ToLower(requested)]
	return ok1 && ok2 && h >= r
}

// RuntimeRef is how one runtime names a model and how much memory it needs.
type RuntimeRef struct {
	Model        string `json:"model"`
	Quantization string `json:"quantization,omitempty"`
	MinVramGB    int    `json:"min_vram_gb,omitempty"`
}

// ParseRuntimeRefs decodes models.runtime_refs.
func ParseRuntimeRefs(raw []byte) (map[string]RuntimeRef, error) {
	refs := map[string]RuntimeRef{}
	if len(raw) == 0 {
		return refs, nil
	}
	if err := json.Unmarshal(raw, &refs); err != nil {
		return nil, fmt.Errorf("placement: invalid runtime_refs: %w", err)
	}
	return refs, nil
}

// Requirements describe one replica to place.
type Requirements struct {
	Tier       string
	Region     string
	ResidentIN bool // DPDP pinning (FR-27): only Indian regions
	IsBYO      bool // BYO weights never run on T3 (NFR-12, ADR-007)
	// RuntimeRefs lists runtimes able to serve the model; a host whose runtime
	// is absent cannot serve it.
	RuntimeRefs map[string]RuntimeRef
	// FallbackMinVramGB is used when a runtime ref carries no VRAM floor.
	FallbackMinVramGB int
	// ExcludeHosts holds hosts that already run a replica of this deployment.
	ExcludeHosts map[string]bool
}

// Candidate is one available GPU on an online host.
type Candidate struct {
	HostID       string
	HostTier     string
	HostStatus   string
	Region       string
	Runtime      string
	Reputation   int
	CachedModels []string
	GPUID        string
	GPUUUID      string
	GPUModel     string
	VramGB       int
}

// Scored is a candidate that passed every filter.
type Scored struct {
	Candidate
	Score        float64
	RuntimeModel string
}

// Rejection explains why no candidate was chosen, for an honest error message.
type Rejection struct {
	Reason string
	Count  int
}

// Filter applies the hard constraints. It returns the runtime model to request
// on success or a human-readable reason on failure.
func Filter(c Candidate, req Requirements) (string, string) {
	if req.ExcludeHosts[c.HostID] {
		return "", "host already runs a replica of this deployment"
	}
	if !TierSatisfies(c.HostTier, req.Tier) {
		return "", fmt.Sprintf("host tier %s is below requested %s", strings.ToUpper(c.HostTier), strings.ToUpper(req.Tier))
	}
	if req.IsBYO && strings.EqualFold(c.HostTier, "t3") {
		return "", "BYO models never run on Tier 3 hosts"
	}
	// Hosts on probation serve only spot work on public catalogue models
	// (UML §6: "probation-eligible workloads only").
	if c.HostStatus == "probation" && (!strings.EqualFold(req.Tier, "t3") || req.IsBYO) {
		return "", "host is on probation and only takes spot catalogue work"
	}
	if c.Reputation < MinReputation {
		return "", fmt.Sprintf("host reputation %d is below %d", c.Reputation, MinReputation)
	}
	if req.ResidentIN && !strings.HasPrefix(strings.ToUpper(c.Region), "IN-") {
		return "", "deployment is pinned to India and the host is outside it"
	}
	// Region must match, except that spot work without residency pinning may
	// use any region; it is interruptible and priced for that.
	if !strings.EqualFold(c.Region, req.Region) && !(strings.EqualFold(req.Tier, "t3") && !req.ResidentIN) {
		return "", fmt.Sprintf("host region %s does not match %s", c.Region, req.Region)
	}
	ref, ok := req.RuntimeRefs[strings.ToLower(c.Runtime)]
	if !ok || ref.Model == "" {
		return "", fmt.Sprintf("model is not packaged for the %q runtime", c.Runtime)
	}
	need := ref.MinVramGB
	if need <= 0 {
		need = req.FallbackMinVramGB
	}
	if c.VramGB < need {
		return "", fmt.Sprintf("GPU has %d GB, model needs %d GB", c.VramGB, need)
	}
	return ref.Model, ""
}

// Score ranks a candidate that passed Filter. All components are in [0,1].
func Score(c Candidate, req Requirements, runtimeModel string) float64 {
	rep := float64(c.Reputation) / 100
	if rep > 1 {
		rep = 1
	}

	// Price: every host in a tier is priced the same, so an exact tier match is
	// the cheapest way to serve the request; putting T3 work on a T1 card
	// spends premium capacity on a spot price.
	price := 1.0
	if !strings.EqualFold(c.HostTier, req.Tier) {
		price = 0.5
	}

	locality := 0.0
	switch {
	case strings.EqualFold(c.Region, req.Region):
		locality = 1
	case countryOf(c.Region) == countryOf(req.Region):
		locality = 0.5
	}

	cache := 0.0
	for _, m := range c.CachedModels {
		if m == runtimeModel {
			cache = 1
			break
		}
	}

	return WeightReputation*rep + WeightPrice*price + WeightLocality*locality + WeightCache*cache
}

// Rank filters and scores candidates, best first. Only the best GPU on each
// host is kept, so successive placements spread across hosts.
func Rank(cands []Candidate, req Requirements) ([]Scored, []Rejection) {
	var out []Scored
	reasons := map[string]int{}
	seenHost := map[string]int{}

	for _, c := range cands {
		model, reason := Filter(c, req)
		if reason != "" {
			reasons[reason]++
			continue
		}
		s := Scored{Candidate: c, Score: Score(c, req, model), RuntimeModel: model}
		if i, ok := seenHost[c.HostID]; ok {
			// Prefer the smallest GPU that fits, leaving big cards for big models.
			if s.VramGB < out[i].VramGB {
				out[i] = s
			}
			continue
		}
		seenHost[c.HostID] = len(out)
		out = append(out, s)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].HostID < out[j].HostID
	})

	var rej []Rejection
	for r, n := range reasons {
		rej = append(rej, Rejection{Reason: r, Count: n})
	}
	sort.Slice(rej, func(i, j int) bool { return rej[i].Count > rej[j].Count })
	return out, rej
}

// NoCapacityMessage turns rejections into the text a customer sees.
func NoCapacityMessage(req Requirements, rej []Rejection, total int) string {
	if total == 0 {
		return fmt.Sprintf("Waiting for capacity: no host is online with a free GPU (tier ≥ %s, region %s).",
			strings.ToUpper(req.Tier), req.Region)
	}
	msg := fmt.Sprintf("Waiting for capacity: %d GPU(s) online, none eligible", total)
	if len(rej) > 0 {
		msg += " — most common reason: " + rej[0].Reason
	}
	return msg + "."
}

func countryOf(region string) string {
	if i := strings.IndexByte(region, '-'); i > 0 {
		return strings.ToUpper(region[:i])
	}
	return strings.ToUpper(region)
}

// LoadCandidates reads every free GPU on an online, unpaused host whose runtime
// is healthy. heartbeatWindow is the online threshold (FR-40: 3 missed 5 s
// heartbeats).
func LoadCandidates(ctx context.Context, q db.Querier, heartbeatWindow time.Duration) ([]Candidate, error) {
	rows, err := q.Query(ctx, `
		SELECT h.id, h.tier, h.status, h.region, COALESCE(h.runtime, ''), h.reputation,
		       h.cached_models, g.id, g.uuid, g.model, g.vram_gb
		FROM hosts h
		JOIN gpus g ON g.host_id = h.id
		WHERE h.deleted_at IS NULL
		  AND h.status IN ('active', 'probation')
		  AND NOT h.paused
		  AND NOT h.agent_outdated
		  AND h.runtime_healthy
		  AND h.last_heartbeat_at >= NOW() - ($1 * INTERVAL '1 second')
		  AND g.status = 'available'
		  AND g.replica_id IS NULL;
	`, int(heartbeatWindow.Seconds()))
	if err != nil {
		return nil, fmt.Errorf("placement: candidate query failed: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.HostID, &c.HostTier, &c.HostStatus, &c.Region, &c.Runtime,
			&c.Reputation, &c.CachedModels, &c.GPUID, &c.GPUUUID, &c.GPUModel, &c.VramGB); err != nil {
			return nil, fmt.Errorf("placement: candidate scan failed: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
