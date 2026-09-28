// Package money provides exact decimal arithmetic for currency amounts.
//
// Currency must never be represented as float64. Binary floating point cannot
// represent most decimal fractions exactly, so repeated arithmetic accumulates
// drift, and a ledger whose running balance drifts is a ledger that cannot be
// reconciled. Amounts here are stored as integer micro-units (1e-6 of the
// currency unit), which matches the NUMERIC(14,6) columns in the schema exactly.
package money

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// Scale is the number of decimal places retained: 1e-6 of a currency unit.
// This matches NUMERIC(14,6) in the database.
const Scale = 6

// micros per whole currency unit.
const micros int64 = 1_000_000

var (
	ErrOverflow         = errors.New("money: arithmetic overflow")
	ErrCurrencyMismatch = errors.New("money: cannot combine amounts in different currencies")
	ErrInvalidAmount    = errors.New("money: invalid amount format")
	ErrNegativeAmount   = errors.New("money: amount must not be negative")
)

// Amount is an exact monetary amount in micro-units of a currency.
// The zero value is a valid zero amount with no currency assigned.
type Amount struct {
	micros   int64
	currency string // ISO 4217, empty for untyped zero
}

// New creates an Amount from whole units and a micro-unit remainder.
func New(units int64, microRemainder int64, currency string) (Amount, error) {
	if microRemainder <= -micros || microRemainder >= micros {
		return Amount{}, fmt.Errorf("money: micro remainder %d out of range", microRemainder)
	}
	total, err := mulAdd(units, micros, microRemainder)
	if err != nil {
		return Amount{}, err
	}
	return Amount{micros: total, currency: normalizeCurrency(currency)}, nil
}

// FromMicros creates an Amount directly from micro-units.
func FromMicros(m int64, currency string) Amount {
	return Amount{micros: m, currency: normalizeCurrency(currency)}
}

// Zero returns a zero amount in the given currency.
func Zero(currency string) Amount {
	return Amount{micros: 0, currency: normalizeCurrency(currency)}
}

// Parse converts a decimal string such as "12.345678" into an exact Amount.
// Parsing goes through math/big rather than strconv.ParseFloat so that no
// intermediate binary float can round the value.
func Parse(s, currency string) (Amount, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Amount{}, ErrInvalidAmount
	}

	rat, ok := new(big.Rat).SetString(s)
	if !ok {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}

	scaled := new(big.Rat).Mul(rat, new(big.Rat).SetInt64(micros))
	if !scaled.IsInt() {
		// More precision than the schema retains; round half away from zero.
		num, denom := scaled.Num(), scaled.Denom()
		quo, rem := new(big.Int).QuoRem(num, denom, new(big.Int))
		twiceRem := new(big.Int).Abs(new(big.Int).Mul(rem, big.NewInt(2)))
		if twiceRem.Cmp(denom) >= 0 {
			if scaled.Sign() < 0 {
				quo.Sub(quo, big.NewInt(1))
			} else {
				quo.Add(quo, big.NewInt(1))
			}
		}
		scaled = new(big.Rat).SetInt(quo)
	}

	if !scaled.Num().IsInt64() {
		return Amount{}, ErrOverflow
	}

	return Amount{micros: scaled.Num().Int64(), currency: normalizeCurrency(currency)}, nil
}

// MustParse is Parse but panics on error. For constants and tests only.
func MustParse(s, currency string) Amount {
	a, err := Parse(s, currency)
	if err != nil {
		panic(err)
	}
	return a
}

// FromFloat converts a float64 to an Amount, rounding to the retained scale.
// Use this only at boundaries with systems that hand over floats (for example a
// JSON API defined elsewhere). Never use it for internal arithmetic.
func FromFloat(f float64, currency string) (Amount, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return Amount{}, fmt.Errorf("%w: %v", ErrInvalidAmount, f)
	}
	scaled := f * float64(micros)
	if scaled > math.MaxInt64 || scaled < math.MinInt64 {
		return Amount{}, ErrOverflow
	}
	return Amount{micros: int64(math.Round(scaled)), currency: normalizeCurrency(currency)}, nil
}

// Micros returns the raw micro-unit value.
func (a Amount) Micros() int64 { return a.micros }

// Currency returns the ISO 4217 currency code.
func (a Amount) Currency() string { return a.currency }

// IsZero reports whether the amount is exactly zero.
func (a Amount) IsZero() bool { return a.micros == 0 }

// IsNegative reports whether the amount is below zero.
func (a Amount) IsNegative() bool { return a.micros < 0 }

// IsPositive reports whether the amount is above zero.
func (a Amount) IsPositive() bool { return a.micros > 0 }

// Sign returns -1, 0, or 1.
func (a Amount) Sign() int {
	switch {
	case a.micros < 0:
		return -1
	case a.micros > 0:
		return 1
	default:
		return 0
	}
}

// Add returns a + b. Both amounts must share a currency.
func (a Amount) Add(b Amount) (Amount, error) {
	cur, err := combineCurrency(a, b)
	if err != nil {
		return Amount{}, err
	}
	sum := a.micros + b.micros
	// Overflow check: the sign of the result cannot differ from both operands.
	if (a.micros > 0 && b.micros > 0 && sum < 0) || (a.micros < 0 && b.micros < 0 && sum >= 0) {
		return Amount{}, ErrOverflow
	}
	return Amount{micros: sum, currency: cur}, nil
}

// Sub returns a - b.
func (a Amount) Sub(b Amount) (Amount, error) {
	return a.Add(b.Neg())
}

// Neg returns -a.
func (a Amount) Neg() Amount {
	return Amount{micros: -a.micros, currency: a.currency}
}

// Abs returns the absolute value.
func (a Amount) Abs() Amount {
	if a.micros < 0 {
		return a.Neg()
	}
	return a
}

// MulRate multiplies the amount by a rate given as a numerator over a
// denominator, rounding half away from zero. Rates are expressed as exact
// fractions rather than floats so that, for example, a 75% revenue share is
// exactly three quarters.
func (a Amount) MulRate(num, denom int64) (Amount, error) {
	if denom == 0 {
		return Amount{}, errors.New("money: rate denominator must not be zero")
	}

	product := new(big.Int).Mul(big.NewInt(a.micros), big.NewInt(num))
	d := big.NewInt(denom)
	quo, rem := new(big.Int).QuoRem(product, d, new(big.Int))

	twiceRem := new(big.Int).Abs(new(big.Int).Mul(rem, big.NewInt(2)))
	if twiceRem.Cmp(new(big.Int).Abs(d)) >= 0 {
		if product.Sign() < 0 != (denom < 0) {
			quo.Sub(quo, big.NewInt(1))
		} else {
			quo.Add(quo, big.NewInt(1))
		}
	}

	if !quo.IsInt64() {
		return Amount{}, ErrOverflow
	}
	return Amount{micros: quo.Int64(), currency: a.currency}, nil
}

// MulInt multiplies by an integer count, such as a token count.
func (a Amount) MulInt(n int64) (Amount, error) {
	product := new(big.Int).Mul(big.NewInt(a.micros), big.NewInt(n))
	if !product.IsInt64() {
		return Amount{}, ErrOverflow
	}
	return Amount{micros: product.Int64(), currency: a.currency}, nil
}

// Cmp compares two amounts: -1 if a < b, 0 if equal, 1 if a > b.
func (a Amount) Cmp(b Amount) (int, error) {
	if _, err := combineCurrency(a, b); err != nil {
		return 0, err
	}
	switch {
	case a.micros < b.micros:
		return -1, nil
	case a.micros > b.micros:
		return 1, nil
	default:
		return 0, nil
	}
}

// LessThan reports whether a < b, treating a currency mismatch as false.
func (a Amount) LessThan(b Amount) bool {
	c, err := a.Cmp(b)
	return err == nil && c < 0
}

// GreaterThan reports whether a > b.
func (a Amount) GreaterThan(b Amount) bool {
	c, err := a.Cmp(b)
	return err == nil && c > 0
}

// String renders the amount as a fixed-point decimal with 6 places.
func (a Amount) String() string {
	neg := a.micros < 0
	abs := a.micros
	if neg {
		abs = -abs
	}
	units := abs / micros
	frac := abs % micros
	sign := ""
	if neg {
		sign = "-"
	}
	return fmt.Sprintf("%s%d.%06d", sign, units, frac)
}

// Display renders the amount for humans, rounded to 2 decimal places.
func (a Amount) Display() string {
	// Round to the nearest cent (10^4 micros), half away from zero.
	cents := (a.micros + sign(a.micros)*5000) / 10000
	neg := cents < 0
	if neg {
		cents = -cents
	}
	prefix := ""
	if neg {
		prefix = "-"
	}
	out := fmt.Sprintf("%s%d.%02d", prefix, cents/100, cents%100)
	if a.currency != "" {
		return out + " " + a.currency
	}
	return out
}

// Float64 returns an approximate float, for display and metrics only.
// Never feed this back into arithmetic that affects a balance.
func (a Amount) Float64() float64 {
	return float64(a.micros) / float64(micros)
}

// ─── JSON ─────────────────────────────────────────────────────

type amountJSON struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency,omitempty"`
	Micros   int64  `json:"micros"`
}

// MarshalJSON emits the amount as a decimal string alongside its exact
// micro-unit value. A string avoids the precision loss a JSON number would
// suffer in JavaScript clients.
func (a Amount) MarshalJSON() ([]byte, error) {
	return json.Marshal(amountJSON{
		Amount:   a.String(),
		Currency: a.currency,
		Micros:   a.micros,
	})
}

// UnmarshalJSON accepts the object form, a bare decimal string, or a JSON
// number. A JSON number is parsed from its literal text via json.Number, never
// through float64, so "0.1" stays exactly 0.1.
func (a *Amount) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" {
		*a = Amount{}
		return nil
	}

	if strings.HasPrefix(trimmed, "{") {
		var obj amountJSON
		if err := json.Unmarshal(data, &obj); err != nil {
			return err
		}
		if obj.Amount != "" {
			parsed, err := Parse(obj.Amount, obj.Currency)
			if err != nil {
				return err
			}
			*a = parsed
			return nil
		}
		*a = FromMicros(obj.Micros, obj.Currency)
		return nil
	}

	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		parsed, err := Parse(s, a.currency)
		if err != nil {
			return err
		}
		parsed.currency = a.currency
		*a = parsed
		return nil
	}

	var n json.Number
	if err := json.Unmarshal(data, &n); err == nil {
		parsed, err := Parse(n.String(), a.currency)
		if err != nil {
			return err
		}
		parsed.currency = a.currency
		*a = parsed
		return nil
	}

	return ErrInvalidAmount
}

// ─── database/sql ─────────────────────────────────────────────

// Value implements driver.Valuer, writing the amount as an exact decimal string
// so Postgres NUMERIC receives no float.
func (a Amount) Value() (driver.Value, error) {
	return a.String(), nil
}

// Scan implements sql.Scanner for NUMERIC columns.
func (a *Amount) Scan(src interface{}) error {
	switch v := src.(type) {
	case nil:
		*a = Amount{}
		return nil
	case string:
		parsed, err := Parse(v, a.currency)
		if err != nil {
			return err
		}
		parsed.currency = a.currency
		*a = parsed
		return nil
	case []byte:
		parsed, err := Parse(string(v), a.currency)
		if err != nil {
			return err
		}
		parsed.currency = a.currency
		*a = parsed
		return nil
	case int64:
		*a = Amount{micros: v * micros, currency: a.currency}
		return nil
	case float64:
		// pgx can hand back float64 for NUMERIC in some configurations.
		parsed, err := FromFloat(v, a.currency)
		if err != nil {
			return err
		}
		*a = parsed
		return nil
	default:
		return fmt.Errorf("money: cannot scan %T into Amount", src)
	}
}

// ─── pgx NUMERIC ──────────────────────────────────────────────
//
// Implementing pgtype's Numeric interfaces means pgx encodes and decodes an
// Amount against a NUMERIC column exactly, in either wire format, without a
// cast in the SQL and without routing the value through float64. Relying on the
// generic database/sql fallbacks above would leave the binary-format path
// handing raw bytes to Scan.

// NumericValue implements pgtype.NumericValuer.
func (a Amount) NumericValue() (pgtype.Numeric, error) {
	return pgtype.Numeric{Int: big.NewInt(a.micros), Exp: -Scale, Valid: true}, nil
}

// ScanNumeric implements pgtype.NumericScanner. The currency of the receiver is
// preserved, because a NUMERIC column carries no currency of its own.
func (a *Amount) ScanNumeric(v pgtype.Numeric) error {
	if !v.Valid {
		*a = Amount{currency: a.currency}
		return nil
	}
	if v.NaN {
		return errors.New("money: cannot scan NaN into Amount")
	}
	if v.InfinityModifier != pgtype.Finite {
		return fmt.Errorf("money: cannot scan %v into Amount", v.InfinityModifier)
	}

	m, err := microsFromNumeric(v.Int, v.Exp)
	if err != nil {
		return err
	}
	*a = Amount{micros: m, currency: a.currency}
	return nil
}

// maxNumericShift bounds the size of the intermediate big.Int when a NUMERIC
// arrives with far more decimal places than the retained scale. Postgres allows
// a dscale up to 16383; nothing legitimate in this schema comes close.
const maxNumericShift = 1000

// microsFromNumeric converts unscaled * 10^exp into micro-units, rounding half
// away from zero when the value carries more precision than is retained.
func microsFromNumeric(unscaled *big.Int, exp int32) (int64, error) {
	if unscaled == nil {
		return 0, nil
	}

	shift := int(exp) + Scale
	n := new(big.Int).Set(unscaled)

	switch {
	case shift == 0:
		// Already in micro-units.
	case shift > 0:
		if shift > maxNumericShift {
			return 0, ErrOverflow
		}
		n.Mul(n, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(shift)), nil))
	default:
		if -shift > maxNumericShift {
			return 0, ErrOverflow
		}
		divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-shift)), nil)
		quo, rem := new(big.Int).QuoRem(n, divisor, new(big.Int))
		twiceRem := new(big.Int).Abs(rem)
		twiceRem.Mul(twiceRem, big.NewInt(2))
		if twiceRem.Cmp(divisor) >= 0 {
			if n.Sign() < 0 {
				quo.Sub(quo, big.NewInt(1))
			} else {
				quo.Add(quo, big.NewInt(1))
			}
		}
		n = quo
	}

	if !n.IsInt64() {
		return 0, ErrOverflow
	}
	return n.Int64(), nil
}

// ─── helpers ──────────────────────────────────────────────────

func normalizeCurrency(c string) string {
	return strings.ToUpper(strings.TrimSpace(c))
}

func combineCurrency(a, b Amount) (string, error) {
	switch {
	case a.currency == b.currency:
		return a.currency, nil
	case a.currency == "":
		return b.currency, nil
	case b.currency == "":
		return a.currency, nil
	default:
		return "", fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, a.currency, b.currency)
	}
}

func mulAdd(a, b, c int64) (int64, error) {
	product := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	product.Add(product, big.NewInt(c))
	if !product.IsInt64() {
		return 0, ErrOverflow
	}
	return product.Int64(), nil
}

func sign(v int64) int64 {
	if v < 0 {
		return -1
	}
	return 1
}

// Sum adds a series of amounts, returning an error on currency mismatch.
func Sum(amounts ...Amount) (Amount, error) {
	var total Amount
	for _, a := range amounts {
		next, err := total.Add(a)
		if err != nil {
			return Amount{}, err
		}
		total = next
	}
	return total, nil
}
