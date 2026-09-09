package money

import (
	"math/big"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestParseExact(t *testing.T) {
	cases := []struct {
		in     string
		micros int64
	}{
		{"0", 0},
		{"1", 1_000_000},
		{"0.000001", 1},
		{"12.345678", 12_345_678},
		{"-4.5", -4_500_000},
		{"0.1", 100_000},
		{"1000000.123456", 1_000_000_123_456},
	}
	for _, c := range cases {
		got, err := Parse(c.in, "USD")
		if err != nil {
			t.Fatalf("Parse(%q) returned error: %v", c.in, err)
		}
		if got.Micros() != c.micros {
			t.Errorf("Parse(%q) = %d micros, want %d", c.in, got.Micros(), c.micros)
		}
	}
}

func TestParseRoundsBeyondScale(t *testing.T) {
	// 7 decimal places: rounds half away from zero at the 6th.
	got, err := Parse("0.0000005", "USD")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Micros() != 1 {
		t.Errorf("got %d micros, want 1", got.Micros())
	}

	got, err = Parse("-0.0000005", "USD")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Micros() != -1 {
		t.Errorf("got %d micros, want -1", got.Micros())
	}
}

// TestNoDriftUnderRepeatedAddition is the reason this package exists. Summing
// 0.1 ten thousand times in float64 does not equal 1000; in exact arithmetic it does.
func TestNoDriftUnderRepeatedAddition(t *testing.T) {
	total := Zero("USD")
	increment := MustParse("0.1", "USD")

	for i := 0; i < 10_000; i++ {
		next, err := total.Add(increment)
		if err != nil {
			t.Fatalf("Add failed at iteration %d: %v", i, err)
		}
		total = next
	}

	want := MustParse("1000", "USD")
	if total.Micros() != want.Micros() {
		t.Errorf("after 10000 additions of 0.1 got %s, want %s", total, want)
	}

	// Demonstrate that the float path does drift, which is what the old code did.
	var f float64
	for i := 0; i < 10_000; i++ {
		f += 0.1
	}
	if f == 1000.0 {
		t.Log("note: float64 accumulation happened to land exactly on 1000 on this platform")
	}
}

func TestMulRateExactRevenueShare(t *testing.T) {
	// A 75% host revenue share on a customer charge.
	charge := MustParse("0.000123", "USD")
	hostShare, err := charge.MulRate(75, 100)
	if err != nil {
		t.Fatalf("MulRate failed: %v", err)
	}
	// 123 * 75 / 100 = 92.25 -> rounds to 92
	if hostShare.Micros() != 92 {
		t.Errorf("got %d micros, want 92", hostShare.Micros())
	}
}

func TestMulRateHalfAwayFromZero(t *testing.T) {
	a := FromMicros(5, "USD")
	got, err := a.MulRate(1, 2) // 2.5 -> 3
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Micros() != 3 {
		t.Errorf("got %d, want 3", got.Micros())
	}

	b := FromMicros(-5, "USD")
	got, err = b.MulRate(1, 2) // -2.5 -> -3
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Micros() != -3 {
		t.Errorf("got %d, want -3", got.Micros())
	}
}

func TestCurrencyMismatchIsRejected(t *testing.T) {
	usd := MustParse("10", "USD")
	inr := MustParse("10", "INR")

	if _, err := usd.Add(inr); err == nil {
		t.Error("expected currency mismatch error when adding USD to INR")
	}
	if _, err := usd.Cmp(inr); err == nil {
		t.Error("expected currency mismatch error when comparing USD to INR")
	}
}

func TestUntypedZeroCombinesWithAnyCurrency(t *testing.T) {
	var zero Amount
	usd := MustParse("5", "USD")

	sum, err := zero.Add(usd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sum.Currency() != "USD" || sum.Micros() != 5_000_000 {
		t.Errorf("got %s %s, want 5.000000 USD", sum, sum.Currency())
	}
}

func TestStringAndDisplay(t *testing.T) {
	a := MustParse("1234.5", "USD")
	if got := a.String(); got != "1234.500000" {
		t.Errorf("String() = %q, want \"1234.500000\"", got)
	}
	if got := a.Display(); got != "1234.50 USD" {
		t.Errorf("Display() = %q, want \"1234.50 USD\"", got)
	}

	neg := MustParse("-0.005", "USD")
	if got := neg.Display(); got != "-0.01 USD" {
		t.Errorf("Display() = %q, want \"-0.01 USD\"", got)
	}
}

func TestValueRendersDecimalNotFloat(t *testing.T) {
	a := MustParse("0.000001", "USD")
	v, err := a.Value()
	if err != nil {
		t.Fatalf("Value failed: %v", err)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("Value returned %T, want string", v)
	}
	if s != "0.000001" {
		t.Errorf("Value() = %q, want \"0.000001\"", s)
	}
}

func TestScanFromNumericString(t *testing.T) {
	a := Zero("USD")
	if err := a.Scan("42.123456"); err != nil {
		t.Fatalf("Scan failed: %v", err)
	}
	if a.Micros() != 42_123_456 {
		t.Errorf("got %d micros, want 42123456", a.Micros())
	}
	if a.Currency() != "USD" {
		t.Errorf("Scan dropped currency, got %q", a.Currency())
	}
}

func TestSum(t *testing.T) {
	total, err := Sum(
		MustParse("1.5", "USD"),
		MustParse("2.25", "USD"),
		MustParse("0.000001", "USD"),
	)
	if err != nil {
		t.Fatalf("Sum failed: %v", err)
	}
	if total.String() != "3.750001" {
		t.Errorf("got %s, want 3.750001", total)
	}
}

func TestTokenCostComputation(t *testing.T) {
	// $0.08 per 1M input tokens, 1500 input tokens.
	pricePerMillion := MustParse("0.08", "USD")
	scaled, err := pricePerMillion.MulInt(1500)
	if err != nil {
		t.Fatalf("MulInt failed: %v", err)
	}
	cost, err := scaled.MulRate(1, 1_000_000)
	if err != nil {
		t.Fatalf("MulRate failed: %v", err)
	}
	// 0.08 * 1500 / 1e6 = 0.00012
	if cost.String() != "0.000120" {
		t.Errorf("got %s, want 0.000120", cost)
	}
}

func TestUnmarshalAcceptsNumberStringAndObject(t *testing.T) {
	cases := []struct {
		name string
		json string
		want int64
	}{
		{"number", `0.1`, 100_000},
		{"string", `"0.1"`, 100_000},
		{"object", `{"amount":"0.1","currency":"USD","micros":100000}`, 100_000},
		{"integer", `42`, 42_000_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var a Amount
			if err := a.UnmarshalJSON([]byte(c.json)); err != nil {
				t.Fatalf("UnmarshalJSON(%s) failed: %v", c.json, err)
			}
			if a.Micros() != c.want {
				t.Errorf("got %d micros, want %d", a.Micros(), c.want)
			}
		})
	}
}

func TestUnmarshalZeroObjectDoesNotFallThrough(t *testing.T) {
	// An object with an explicit zero must not be misread as "no fields set".
	var a Amount
	if err := a.UnmarshalJSON([]byte(`{"amount":"0.000000","currency":"INR","micros":0}`)); err != nil {
		t.Fatalf("UnmarshalJSON failed: %v", err)
	}
	if !a.IsZero() || a.Currency() != "INR" {
		t.Errorf("got %s %s, want zero INR", a, a.Currency())
	}
}

func TestRoundTripJSON(t *testing.T) {
	orig := MustParse("1234.567891", "USD")
	blob, err := orig.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON failed: %v", err)
	}
	var back Amount
	if err := back.UnmarshalJSON(blob); err != nil {
		t.Fatalf("UnmarshalJSON failed: %v", err)
	}
	if back.Micros() != orig.Micros() || back.Currency() != orig.Currency() {
		t.Errorf("round trip changed %s %s into %s %s", orig, orig.Currency(), back, back.Currency())
	}
}

// TestNumericRoundTrip covers the pgx NUMERIC path, which is how every money
// column actually crosses the wire.
func TestNumericRoundTrip(t *testing.T) {
	for _, in := range []string{"0", "0.000001", "-0.000001", "1234.567891", "-98765.432109", "99999999.999999"} {
		orig := MustParse(in, "USD")
		num, err := orig.NumericValue()
		if err != nil {
			t.Fatalf("NumericValue(%s) failed: %v", in, err)
		}
		back := Zero("USD")
		if err := back.ScanNumeric(num); err != nil {
			t.Fatalf("ScanNumeric(%s) failed: %v", in, err)
		}
		if back.Micros() != orig.Micros() {
			t.Errorf("%s round tripped to %s", orig, back)
		}
		if back.Currency() != "USD" {
			t.Errorf("ScanNumeric dropped the currency, got %q", back.Currency())
		}
	}
}

func TestScanNumericHandlesForeignScales(t *testing.T) {
	cases := []struct {
		name     string
		unscaled int64
		exp      int32
		want     int64
	}{
		// NUMERIC(12,4) columns, as the models table declares, arrive with exp -4.
		{"scale4", 800, -4, 80_000},
		// A whole number arrives with exp 0.
		{"scale0", 42, 0, 42_000_000},
		// Postgres normalizes trailing zeros, so a positive exp is possible.
		{"positiveExp", 5, 2, 500_000_000},
		// More precision than we retain rounds half away from zero.
		{"roundsUp", 5, -7, 1},
		{"roundsDown", 4, -7, 0},
		{"roundsNegative", -5, -7, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := Zero("INR")
			err := a.ScanNumeric(pgtype.Numeric{Int: big.NewInt(c.unscaled), Exp: c.exp, Valid: true})
			if err != nil {
				t.Fatalf("ScanNumeric failed: %v", err)
			}
			if a.Micros() != c.want {
				t.Errorf("got %d micros, want %d", a.Micros(), c.want)
			}
		})
	}
}

func TestScanNumericNullYieldsZero(t *testing.T) {
	a := MustParse("5", "USD")
	if err := a.ScanNumeric(pgtype.Numeric{Valid: false}); err != nil {
		t.Fatalf("ScanNumeric(NULL) failed: %v", err)
	}
	if !a.IsZero() || a.Currency() != "USD" {
		t.Errorf("got %s %s, want zero USD", a, a.Currency())
	}
}

func TestScanNumericRejectsNaNAndInfinity(t *testing.T) {
	a := Zero("USD")
	if err := a.ScanNumeric(pgtype.Numeric{NaN: true, Valid: true}); err == nil {
		t.Error("expected an error scanning NaN into an Amount")
	}
	if err := a.ScanNumeric(pgtype.Numeric{InfinityModifier: pgtype.Infinity, Valid: true}); err == nil {
		t.Error("expected an error scanning Infinity into an Amount")
	}
}
