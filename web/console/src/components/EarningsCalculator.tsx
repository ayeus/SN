"use client";

import { useMemo, useState } from "react";
import type { Pricing } from "@/lib/types";
import { Field, Input, Select, cx } from "./ui";
import { TIERS } from "@/lib/format";

const inr = (v: number) =>
  new Intl.NumberFormat("en-IN", { style: "currency", currency: "INR", maximumFractionDigits: 0 }).format(v);

// PRD F-13: an earnings calculator that shows income net of electricity and
// says plainly when hardware is not worth running.
export function EarningsCalculator({ pricing }: { pricing: Pricing }) {
  const skus = pricing.gpu_skus;
  const [skuId, setSkuId] = useState(skus.find((s) => s.tier === "t2")?.id ?? skus[0]?.id ?? "");
  const [hours, setHours] = useState(12);
  const [utilisation, setUtilisation] = useState(50);
  const [tariff, setTariff] = useState(8);

  const sku = skus.find((s) => s.id === skuId);
  const r = useMemo(() => {
    if (!sku) return null;
    const rentedHours = hours * 30 * (utilisation / 100);
    const gross = Number(sku.price_per_hour_inr.amount) * rentedHours * (pricing.host_share_percent / 100);
    // Power is drawn only while a job runs; idle draw is ignored as the
    // machine would be on anyway.
    const power = (sku.tdp_watts / 1000) * rentedHours * tariff;
    return { gross, power, net: gross - power, rentedHours };
  }, [sku, hours, utilisation, tariff, pricing.host_share_percent]);

  if (!skus.length) return <p className="text-muted">The rate card is not configured yet.</p>;

  return (
    <div className="grid gap-8 md:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]">
      <div className="grid gap-4">
        <Field label="GPU" htmlFor="calc-gpu">
          <Select id="calc-gpu" value={skuId} onChange={(e) => setSkuId(e.target.value)}>
            {skus.map((s) => (
              <option key={s.id} value={s.id}>
                {s.gpu_model} — {TIERS[s.tier].short} {TIERS[s.tier].name}
              </option>
            ))}
          </Select>
        </Field>
        <Field label={`Hours available per day: ${hours}`} htmlFor="calc-hours">
          <input id="calc-hours" type="range" min={1} max={24} value={hours} onChange={(e) => setHours(+e.target.value)} className="accent-[var(--nil)]" />
        </Field>
        <Field label={`Expected utilisation: ${utilisation}%`} hint="How much of that time customers actually use it. New hosts usually see less." htmlFor="calc-util">
          <input id="calc-util" type="range" min={10} max={100} step={5} value={utilisation} onChange={(e) => setUtilisation(+e.target.value)} className="accent-[var(--nil)]" />
        </Field>
        <Field label="Electricity tariff (₹ per kWh)" htmlFor="calc-tariff">
          <Input id="calc-tariff" type="number" min={0} step={0.5} value={tariff} onChange={(e) => setTariff(Math.max(0, +e.target.value))} />
        </Field>
      </div>

      {r && sku && (
        <div className="flex flex-col justify-between rounded-xl bg-surface-2 p-6">
          <dl className="grid gap-3 text-[14px]">
            <div className="flex justify-between gap-4">
              <dt className="text-muted">Rented hours per month</dt>
              <dd>{Math.round(r.rentedHours)} h</dd>
            </div>
            <div className="flex justify-between gap-4">
              <dt className="text-muted">
                Your {pricing.host_share_percent}% of {inr(Number(sku.price_per_hour_inr.amount))}/h
              </dt>
              <dd>{inr(r.gross)}</dd>
            </div>
            <div className="flex justify-between gap-4">
              <dt className="text-muted">Electricity at {sku.tdp_watts} W</dt>
              <dd>−{inr(r.power)}</dd>
            </div>
          </dl>
          <div className="mt-6 border-t border-line pt-4">
            <div className="text-[13px] text-muted">Estimated net per month</div>
            <div className={cx("text-[34px] font-semibold tracking-[-0.02em]", r.net <= 0 && "text-danger")}>{inr(r.net)}</div>
            {r.net <= 0 ? (
              <p className="mt-2 text-[13px] text-danger">At this tariff the GPU costs more to run than it earns. We'd rather tell you now.</p>
            ) : (
              <p className="mt-2 text-[13px] text-muted">
                An estimate, not a promise. Payouts are weekly after KYC, with 1% TDS withheld under section 194-O.
              </p>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
