package billing

import (
	"math/big"
	"testing"

	"github.com/ayeus/ayeusann/internal/money"
)

func usd(s string) money.Amount { return money.MustParse(s, "USD") }

func TestTierPricesAppliesSpotDiscountOnlyToT3(t *testing.T) {
	p, err := TierPrices(usd("0.08"), usd("0.22"), "t2")
	if err != nil {
		t.Fatal(err)
	}
	if p.InPer1M.String() != "0.080000" || p.OutPer1M.String() != "0.220000" || p.Source != "catalog" {
		t.Fatalf("t2 prices changed: %+v", p)
	}

	spot, err := TierPrices(usd("0.08"), usd("0.22"), "t3")
	if err != nil {
		t.Fatal(err)
	}
	// 55% of on-demand.
	if spot.InPer1M.String() != "0.044000" || spot.OutPer1M.String() != "0.121000" {
		t.Fatalf("unexpected spot prices: in=%s out=%s", spot.InPer1M, spot.OutPer1M)
	}
	if spot.Source != "catalog-spot" {
		t.Fatalf("source = %q", spot.Source)
	}
}

func TestTokenChargeIsExact(t *testing.T) {
	p := Prices{InPer1M: usd("0.08"), OutPer1M: usd("0.22")}

	cases := []struct {
		in, out int64
		want    string
	}{
		{0, 0, "0.000000"},
		{1_000_000, 0, "0.080000"},
		{0, 1_000_000, "0.220000"},
		{100, 50, "0.000019"}, // 8 + 11 = 19 micros
		{1, 1, "0.000000"},    // 0.30 micros rounds down
		{3, 1, "0.000000"},    // 0.46 micros rounds down
		{5, 1, "0.000001"},    // 0.62 micros rounds up
	}
	for _, c := range cases {
		got, err := TokenCharge(c.in, c.out, p)
		if err != nil {
			t.Fatal(err)
		}
		if got.String() != c.want {
			t.Errorf("TokenCharge(%d,%d) = %s, want %s", c.in, c.out, got, c.want)
		}
	}

	if _, err := TokenCharge(-1, 0, p); err == nil {
		t.Fatal("negative tokens must be rejected")
	}
}

func TestHostShareReconcilesExactly(t *testing.T) {
	// The Architecture §9 invariant: charge = host accrual + platform margin,
	// with no rounding drift, for awkward values too.
	for _, s := range []string{"0.000001", "0.000003", "0.000019", "1.234567", "99.999999"} {
		charge := money.MustParse(s, "INR")
		host, margin, err := HostShare(charge)
		if err != nil {
			t.Fatal(err)
		}
		sum, err := host.Add(margin)
		if err != nil {
			t.Fatal(err)
		}
		if sum.Micros() != charge.Micros() {
			t.Fatalf("charge %s != host %s + margin %s", charge, host, margin)
		}
	}

	host, _, _ := HostShare(money.MustParse("1.00", "INR"))
	if host.String() != "0.750000" {
		t.Fatalf("75%% of 1.00 = %s", host)
	}
}

func TestConvertRoundsHalfAwayFromZero(t *testing.T) {
	rate := big.NewRat(84, 1)
	got, err := Convert(usd("0.000019"), rate, "INR")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "0.001596" || got.Currency() != "INR" {
		t.Fatalf("got %s %s", got, got.Currency())
	}

	// INR → USD at 1/84: 1 micro-rupee is 0.0119 micro-dollars, rounds to 0.
	back, _ := Convert(money.MustParse("0.000001", "INR"), big.NewRat(1, 84), "USD")
	if back.Micros() != 0 {
		t.Fatalf("expected rounding to zero, got %s", back)
	}

	half, _ := Convert(money.FromMicros(1, "USD"), big.NewRat(1, 2), "USD")
	if half.Micros() != 1 {
		t.Fatalf("0.5 micro must round away from zero, got %d", half.Micros())
	}
	negHalf, _ := Convert(money.FromMicros(-1, "USD"), big.NewRat(1, 2), "USD")
	if negHalf.Micros() != -1 {
		t.Fatalf("-0.5 micro must round away from zero, got %d", negHalf.Micros())
	}
}
