// Package billing prices inference usage and records it. It is the single
// implementation of Architecture §9 ("metering is sacred"): one usage event
// produces the customer charge and the host accrual in the same row, and the
// wallet debit commits in the same transaction, so billing and payouts can never
// disagree.
package billing

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ayeus/ayeusann/internal/db"
	"github.com/ayeus/ayeusann/internal/money"
	"github.com/jackc/pgx/v5"
)

// PriceCurrency is the currency catalogue token prices are quoted in (PRD §9
// lists managed-model prices in USD per 1M tokens).
const PriceCurrency = "USD"

// HostSharePercent is the share of every charge that accrues to the host that
// served it (PRD §1, F-14: "hosts keep 75%").
const HostSharePercent = 75

// SpotPricePercent is the T3 spot token price as a percentage of on-demand
// (PRD §1 decision 4 and §9: "50–60% of on-demand token price").
const SpotPricePercent = 55

// Prices are per 1M tokens in PriceCurrency.
type Prices struct {
	InPer1M  money.Amount `json:"in_per_1m"`
	OutPer1M money.Amount `json:"out_per_1m"`
	// Source explains where the price came from: "catalog", "catalog-spot" or
	// "price-book:<name>".
	Source string `json:"source"`
}

// ErrModelNotFound is returned when a model has no catalogue row.
var ErrModelNotFound = errors.New("billing: model not found")

// TierPrices applies the tier policy to catalogue on-demand prices.
func TierPrices(in, out money.Amount, tier string) (Prices, error) {
	in = money.FromMicros(in.Micros(), PriceCurrency)
	out = money.FromMicros(out.Micros(), PriceCurrency)
	if strings.EqualFold(tier, "t3") {
		spotIn, err := in.MulRate(SpotPricePercent, 100)
		if err != nil {
			return Prices{}, err
		}
		spotOut, err := out.MulRate(SpotPricePercent, 100)
		if err != nil {
			return Prices{}, err
		}
		return Prices{InPer1M: spotIn, OutPer1M: spotOut, Source: "catalog-spot"}, nil
	}
	return Prices{InPer1M: in, OutPer1M: out, Source: "catalog"}, nil
}

// ResolvePrices returns the token prices an organisation pays for a model on a
// tier. An organisation price book (the incubator partner rate, SRS FR-75) that
// has token prices for the tier and GPU overrides the catalogue.
func ResolvePrices(ctx context.Context, q db.Querier, orgID, modelID, tier, gpuModel string) (Prices, error) {
	var catIn, catOut money.Amount
	if err := q.QueryRow(ctx,
		`SELECT price_in_per_1m, price_out_per_1m FROM models WHERE id = $1;`, modelID,
	).Scan(&catIn, &catOut); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Prices{}, ErrModelNotFound
		}
		return Prices{}, fmt.Errorf("billing: failed to read model prices: %w", err)
	}

	if orgID != "" && gpuModel != "" {
		var bookName string
		var pbIn, pbOut *money.Amount
		err := q.QueryRow(ctx, `
			SELECT pb.name, pbe.price_in_per_1m, pbe.price_out_per_1m
			FROM organizations o
			JOIN price_books pb ON pb.id = o.price_book_id
			JOIN price_book_entries pbe ON pbe.price_book_id = pb.id
			WHERE o.id = $1
			  AND pbe.tier = $2
			  AND $3 ILIKE '%' || pbe.gpu_model || '%'
			  AND pbe.price_in_per_1m IS NOT NULL
			  AND pbe.price_out_per_1m IS NOT NULL
			LIMIT 1;
		`, orgID, strings.ToLower(tier), gpuModel).Scan(&bookName, &pbIn, &pbOut)
		switch {
		case err == nil && pbIn != nil && pbOut != nil:
			return Prices{
				InPer1M:  money.FromMicros(pbIn.Micros(), PriceCurrency),
				OutPer1M: money.FromMicros(pbOut.Micros(), PriceCurrency),
				Source:   "price-book:" + bookName,
			}, nil
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return Prices{}, fmt.Errorf("billing: failed to read price book: %w", err)
		}
	}

	return TierPrices(catIn, catOut, tier)
}

// TokenCharge computes the exact charge for a token count: tokens × price / 1M,
// rounded half away from zero to a micro-unit.
func TokenCharge(inputTokens, outputTokens int64, p Prices) (money.Amount, error) {
	if inputTokens < 0 || outputTokens < 0 {
		return money.Amount{}, errors.New("billing: token counts must not be negative")
	}
	total := new(big.Int).Mul(big.NewInt(inputTokens), big.NewInt(p.InPer1M.Micros()))
	total.Add(total, new(big.Int).Mul(big.NewInt(outputTokens), big.NewInt(p.OutPer1M.Micros())))

	million := big.NewInt(1_000_000)
	quo, rem := new(big.Int).QuoRem(total, million, new(big.Int))
	if new(big.Int).Mul(rem, big.NewInt(2)).Cmp(million) >= 0 {
		quo.Add(quo, big.NewInt(1))
	}
	if !quo.IsInt64() {
		return money.Amount{}, money.ErrOverflow
	}
	return money.FromMicros(quo.Int64(), PriceCurrency), nil
}

// HostShare splits a charge into the host accrual and the platform margin. The
// margin is computed as charge − host, so Σ charges = Σ accruals + Σ margin holds
// exactly for every event (Architecture §9 invariant).
func HostShare(charge money.Amount) (host, margin money.Amount, err error) {
	host, err = charge.MulRate(HostSharePercent, 100)
	if err != nil {
		return money.Amount{}, money.Amount{}, err
	}
	margin, err = charge.Sub(host)
	return host, margin, err
}

// FXRate returns the rate that converts an amount in `from` into `to`, using the
// fx_rates table (base USD). It also returns the rate as a decimal string so it
// can be stored alongside the converted amount.
func FXRate(ctx context.Context, q db.Querier, from, to string) (*big.Rat, string, error) {
	from, to = strings.ToUpper(from), strings.ToUpper(to)
	if from == to {
		return big.NewRat(1, 1), "1", nil
	}
	usdTo, err := usdRate(ctx, q, to)
	if err != nil {
		return nil, "", err
	}
	usdFrom, err := usdRate(ctx, q, from)
	if err != nil {
		return nil, "", err
	}
	rate := new(big.Rat).Quo(usdTo, usdFrom)
	return rate, rate.FloatString(8), nil
}

func usdRate(ctx context.Context, q db.Querier, currency string) (*big.Rat, error) {
	if currency == "USD" {
		return big.NewRat(1, 1), nil
	}
	var s string
	err := q.QueryRow(ctx,
		`SELECT rate::TEXT FROM fx_rates WHERE base = 'USD' AND quote = $1;`, currency,
	).Scan(&s)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("billing: no FX rate configured for USD→%s", currency)
		}
		return nil, fmt.Errorf("billing: failed to read FX rate: %w", err)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() <= 0 {
		return nil, fmt.Errorf("billing: invalid FX rate %q for %s", s, currency)
	}
	return r, nil
}

// Convert multiplies an amount by an exact rate into the target currency,
// rounding half away from zero to a micro-unit.
func Convert(a money.Amount, rate *big.Rat, to string) (money.Amount, error) {
	prod := new(big.Rat).Mul(new(big.Rat).SetInt64(a.Micros()), rate)
	num, den := prod.Num(), prod.Denom()
	quo, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if new(big.Int).Abs(new(big.Int).Mul(rem, big.NewInt(2))).Cmp(den) >= 0 {
		if num.Sign() < 0 {
			quo.Sub(quo, big.NewInt(1))
		} else {
			quo.Add(quo, big.NewInt(1))
		}
	}
	if !quo.IsInt64() {
		return money.Amount{}, money.ErrOverflow
	}
	return money.FromMicros(quo.Int64(), to), nil
}
