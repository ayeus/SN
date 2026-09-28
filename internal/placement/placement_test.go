package placement

import (
	"math"
	"strings"
	"testing"
)

func refs() map[string]RuntimeRef {
	return map[string]RuntimeRef{
		"ollama": {Model: "qwen2.5:7b", MinVramGB: 8},
		"vllm":   {Model: "Qwen/Qwen2.5-7B-Instruct", MinVramGB: 18},
	}
}

func base() Candidate {
	return Candidate{
		HostID: "h1", HostTier: "t2", HostStatus: "active", Region: "IN-SOUTH",
		Runtime: "vllm", Reputation: 80, GPUID: "g1", GPUModel: "RTX 4090", VramGB: 24,
	}
}

func req(tier string) Requirements {
	return Requirements{Tier: tier, Region: "IN-SOUTH", RuntimeRefs: refs(), FallbackMinVramGB: 16}
}

func TestTierSatisfies(t *testing.T) {
	cases := []struct {
		host, want string
		ok         bool
	}{
		{"t1", "t1", true}, {"t1", "t2", true}, {"t1", "t3", true},
		{"t2", "t1", false}, {"t2", "t2", true}, {"t2", "t3", true},
		{"t3", "t1", false}, {"t3", "t2", false}, {"t3", "t3", true},
		{"tx", "t3", false},
	}
	for _, c := range cases {
		if got := TierSatisfies(c.host, c.want); got != c.ok {
			t.Errorf("TierSatisfies(%s,%s)=%v want %v", c.host, c.want, got, c.ok)
		}
	}
}

func TestFilterRules(t *testing.T) {
	type tc struct {
		name   string
		mutC   func(*Candidate)
		mutR   func(*Requirements)
		reason string // substring; empty means accepted
	}
	cases := []tc{
		{"accepts a matching host", nil, nil, ""},
		{"tier below requested", func(c *Candidate) { c.HostTier = "t3" }, nil, "below requested"},
		{"BYO never on T3", func(c *Candidate) { c.HostTier = "t3" }, func(r *Requirements) { r.Tier = "t3"; r.IsBYO = true }, "BYO"},
		{"probation takes only spot", func(c *Candidate) { c.HostStatus = "probation" }, nil, "probation"},
		{"probation accepts spot catalogue", func(c *Candidate) { c.HostStatus = "probation" }, func(r *Requirements) { r.Tier = "t3" }, ""},
		{"low reputation", func(c *Candidate) { c.Reputation = 39 }, nil, "reputation"},
		{"region mismatch", func(c *Candidate) { c.Region = "IN-WEST" }, nil, "region"},
		{"spot may cross regions", func(c *Candidate) { c.Region = "US-EAST" }, func(r *Requirements) { r.Tier = "t3" }, ""},
		{"residency pins spot to India", func(c *Candidate) { c.Region = "US-EAST" }, func(r *Requirements) { r.Tier = "t3"; r.ResidentIN = true }, "pinned to India"},
		{"runtime without the model", func(c *Candidate) { c.Runtime = "tgi" }, nil, "not packaged"},
		{"not enough VRAM for vLLM", func(c *Candidate) { c.VramGB = 16 }, nil, "needs 18 GB"},
		{"ollama needs less VRAM", func(c *Candidate) { c.VramGB = 16; c.Runtime = "ollama" }, nil, ""},
		{"excluded host", nil, func(r *Requirements) { r.ExcludeHosts = map[string]bool{"h1": true} }, "already runs"},
	}
	for _, c := range cases {
		cand, r := base(), req("t2")
		if c.mutC != nil {
			c.mutC(&cand)
		}
		if c.mutR != nil {
			c.mutR(&r)
		}
		model, reason := Filter(cand, r)
		if c.reason == "" {
			if reason != "" {
				t.Errorf("%s: rejected: %s", c.name, reason)
			} else if model == "" {
				t.Errorf("%s: no runtime model returned", c.name)
			}
			continue
		}
		if !strings.Contains(reason, c.reason) {
			t.Errorf("%s: reason %q does not contain %q", c.name, reason, c.reason)
		}
	}
}

func TestScoreWeights(t *testing.T) {
	c := base()
	c.Reputation = 100
	c.CachedModels = []string{"Qwen/Qwen2.5-7B-Instruct"}
	r := req("t2")
	if got := Score(c, r, "Qwen/Qwen2.5-7B-Instruct"); math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("perfect candidate scored %v", got)
	}

	c.CachedModels = nil
	if got := Score(c, r, "Qwen/Qwen2.5-7B-Instruct"); math.Abs(got-0.80) > 1e-9 {
		t.Fatalf("cold cache should cost 0.20, got %v", got)
	}

	c.HostTier = "t1" // over-tier: price component halves
	if got := Score(c, r, "x"); math.Abs(got-0.675) > 1e-9 {
		t.Fatalf("over-tier score %v", got)
	}

	c.HostTier, c.Region = "t2", "IN-WEST" // same country, different region
	if got := Score(c, r, "x"); math.Abs(got-0.70) > 1e-9 {
		t.Fatalf("same-country score %v", got)
	}
}

func TestRankPrefersWarmCacheAndSpreadsAcrossHosts(t *testing.T) {
	warm := base()
	warm.HostID, warm.GPUID, warm.Reputation = "warm", "gw", 60
	warm.CachedModels = []string{"Qwen/Qwen2.5-7B-Instruct"}

	cold := base()
	cold.HostID, cold.GPUID, cold.Reputation = "cold", "gc", 90

	// Two GPUs on the cold host: only one survives, the smaller that fits.
	coldBig := cold
	coldBig.GPUID, coldBig.VramGB = "gc-big", 80

	ranked, _ := Rank([]Candidate{coldBig, cold, warm}, req("t2"))
	if len(ranked) != 2 {
		t.Fatalf("expected one entry per host, got %d", len(ranked))
	}
	// warm: .35*.6 + .25 + .2 + .2 = 0.86 ; cold: .35*.9 + .25 + .2 = 0.765
	if ranked[0].HostID != "warm" {
		t.Fatalf("warm cache should win, got %s", ranked[0].HostID)
	}
	if ranked[1].GPUID != "gc" {
		t.Fatalf("smallest fitting GPU should be kept, got %s", ranked[1].GPUID)
	}
}

func TestNoCapacityMessageExplainsWhy(t *testing.T) {
	r := req("t2")
	_, rej := Rank([]Candidate{{HostID: "a", HostTier: "t3", Region: "IN-SOUTH", Runtime: "ollama", Reputation: 90, VramGB: 16}}, r)
	msg := NoCapacityMessage(r, rej, 1)
	if !strings.Contains(msg, "below requested T2") {
		t.Fatalf("message should explain the tier mismatch: %s", msg)
	}
	if !strings.Contains(NoCapacityMessage(r, nil, 0), "no host is online") {
		t.Fatal("empty-fleet message wrong")
	}
}
