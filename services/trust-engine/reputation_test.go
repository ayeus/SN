package main

import (
	"testing"
	"time"

	"github.com/ayeus/ayeusann/internal/domain"
)

func f(v float64) *float64 { return &v }

func TestScoreUsesPRDWeights(t *testing.T) {
	// All evidence present and perfect.
	s, _ := Score(Inputs{UptimePct: 100, SuccessRatePct: f(100), StabilityPct: f(100), AgeDays: 30})
	if s != 100 {
		t.Fatalf("perfect host scored %d", s)
	}

	// Uptime 50, everything else perfect: 100 − 0.40·50 = 80.
	s, c := Score(Inputs{UptimePct: 50, SuccessRatePct: f(100), StabilityPct: f(100), AgeDays: 30})
	if s != 80 || c.Uptime != 50 {
		t.Fatalf("uptime weighting wrong: %d %+v", s, c)
	}

	// Correctness 0 costs its 25%.
	s, _ = Score(Inputs{UptimePct: 100, SuccessRatePct: f(0), StabilityPct: f(100), AgeDays: 30})
	if s != 75 {
		t.Fatalf("correctness weighting wrong: %d", s)
	}

	// Each incident in 30 days removes 20 points of the 10% incident component.
	s, _ = Score(Inputs{UptimePct: 100, SuccessRatePct: f(100), StabilityPct: f(100), AgeDays: 30, Incidents30d: 5})
	if s != 90 {
		t.Fatalf("incident weighting wrong: %d", s)
	}
}

func TestScoreRenormalisesMissingEvidence(t *testing.T) {
	// A new host with no traffic and no benchmark history is scored on uptime,
	// age and incidents only; unknowns are not treated as zero.
	s, c := Score(Inputs{UptimePct: 100, AgeDays: 0})
	if c.Correctness != nil || c.Stability != nil {
		t.Fatal("missing evidence must stay nil")
	}
	// (0.40·100 + 0.10·0 + 0.10·100) / 0.60 = 83.3
	if s != 83 {
		t.Fatalf("renormalised score = %d", s)
	}
}

func TestStability(t *testing.T) {
	if Stability([]float64{85}) != nil {
		t.Fatal("one run cannot measure stability")
	}
	if v := Stability([]float64{85, 85, 85}); v == nil || *v != 100 {
		t.Fatalf("identical runs should be 100, got %v", v)
	}
	if v := Stability([]float64{50, 100}); v == nil || *v > 70 || *v < 60 {
		t.Fatalf("CV of 33%% should give ~67, got %v", *v)
	}
}

func TestNextStatusFollowsUML(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	cases := []struct {
		from  string
		score int
		until *time.Time
		want  string
	}{
		{domain.HostStatusProbation, 70, &past, domain.HostStatusActive},
		{domain.HostStatusProbation, 70, &future, domain.HostStatusProbation},
		{domain.HostStatusProbation, 55, &past, domain.HostStatusProbation},
		{domain.HostStatusProbation, 30, &future, domain.HostStatusDemoted},
		{domain.HostStatusActive, 39, nil, domain.HostStatusDemoted},
		{domain.HostStatusActive, 40, nil, domain.HostStatusActive},
		{domain.HostStatusDemoted, 49, nil, domain.HostStatusDemoted},
		{domain.HostStatusDemoted, 50, nil, domain.HostStatusActive},
		{domain.HostStatusOffline, 10, nil, domain.HostStatusOffline},
		{domain.HostStatusBanned, 100, nil, domain.HostStatusBanned},
	}
	for _, c := range cases {
		if got := nextStatus(c.from, c.score, c.until); got != c.want {
			t.Errorf("%s @%d → %s, want %s", c.from, c.score, got, c.want)
		}
	}
}
