package main

import (
	"testing"
)

func TestComputeTokenCost(t *testing.T) {
	tests := []struct {
		name          string
		inputTokens   int
		outputTokens  int
		priceInPer1M  float64
		priceOutPer1M float64
		expected      float64
	}{
		{
			name:          "standard pricing",
			inputTokens:   1000,
			outputTokens:  500,
			priceInPer1M:  0.15,
			priceOutPer1M: 0.60,
			// (1000 * 0.15 / 1M) + (500 * 0.60 / 1M) = 0.00015 + 0.0003 = 0.00045
			expected: 0.00045,
		},
		{
			name:          "zero tokens",
			inputTokens:   0,
			outputTokens:  0,
			priceInPer1M:  0.15,
			priceOutPer1M: 0.60,
			expected:      0,
		},
		{
			name:          "large volume",
			inputTokens:   1_000_000,
			outputTokens:  1_000_000,
			priceInPer1M:  0.15,
			priceOutPer1M: 0.60,
			// (1M * 0.15 / 1M) + (1M * 0.60 / 1M) = 0.15 + 0.60 = 0.75
			expected: 0.75,
		},
		{
			name:          "input only",
			inputTokens:   500_000,
			outputTokens:  0,
			priceInPer1M:  0.15,
			priceOutPer1M: 0.60,
			expected:      0.075, // 500K * 0.15 / 1M
		},
		{
			name:          "output only",
			inputTokens:   0,
			outputTokens:  500_000,
			priceInPer1M:  0.15,
			priceOutPer1M: 0.60,
			expected:      0.30, // 500K * 0.60 / 1M
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := computeTokenCost(tc.inputTokens, tc.outputTokens, tc.priceInPer1M, tc.priceOutPer1M)
			// Use epsilon comparison for floating point
			diff := result - tc.expected
			if diff < -0.000001 || diff > 0.000001 {
				t.Fatalf("expected %.8f, got %.8f", tc.expected, result)
			}
		})
	}
}

func TestHostRevenueShare(t *testing.T) {
	customerAmount := 1.00 // $1.00
	hostAmount := customerAmount * HostRevenueSharePct

	if hostAmount != 0.75 {
		t.Fatalf("expected host share $0.75, got $%.2f", hostAmount)
	}

	platformFee := customerAmount - hostAmount
	if platformFee != 0.25 {
		t.Fatalf("expected platform fee $0.25, got $%.2f", platformFee)
	}
}

func TestGSTCalculation(t *testing.T) {
	subtotal := 100.0
	gst := subtotal * GSTRate
	total := subtotal + gst

	if gst != 18.0 {
		t.Fatalf("expected GST $18.00, got $%.2f", gst)
	}
	if total != 118.0 {
		t.Fatalf("expected total $118.00, got $%.2f", total)
	}
}

func TestUsageIngestRequest_Validation(t *testing.T) {
	meter := &MeterService{} // No DB needed for validation tests

	tests := []struct {
		name    string
		req     UsageIngestRequest
		wantErr bool
	}{
		{
			name: "missing request_id",
			req: UsageIngestRequest{
				DeploymentID: "d1",
				ReplicaID:    "r1",
				HostID:       "h1",
			},
			wantErr: true,
		},
		{
			name: "missing deployment_id",
			req: UsageIngestRequest{
				RequestID: "req1",
				ReplicaID: "r1",
				HostID:    "h1",
			},
			wantErr: true,
		},
		{
			name: "missing replica_id",
			req: UsageIngestRequest{
				RequestID:    "req1",
				DeploymentID: "d1",
				HostID:       "h1",
			},
			wantErr: true,
		},
		{
			name: "missing host_id",
			req: UsageIngestRequest{
				RequestID:    "req1",
				DeploymentID: "d1",
				ReplicaID:    "r1",
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := meter.IngestUsage(nil, tc.req)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}

func TestInvoiceGenerateRequest_Validation(t *testing.T) {
	meter := &MeterService{}

	tests := []struct {
		name    string
		req     InvoiceGenerateRequest
		wantErr bool
	}{
		{
			name:    "missing org_id",
			req:     InvoiceGenerateRequest{PeriodStart: "2026-08-01", PeriodEnd: "2026-09-01"},
			wantErr: true,
		},
		{
			name:    "missing period_start",
			req:     InvoiceGenerateRequest{OrgID: "org1", PeriodEnd: "2026-09-01"},
			wantErr: true,
		},
		{
			name:    "missing period_end",
			req:     InvoiceGenerateRequest{OrgID: "org1", PeriodStart: "2026-08-01"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := meter.GenerateInvoice(nil, tc.req)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}
