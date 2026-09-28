"use client";

import { api } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { useData } from "@/lib/hooks";
import { convert, money, TIERS } from "@/lib/format";
import type { Model, Pricing } from "@/lib/types";
import { ButtonLink, Notice, PageHeader, Panel, Skeleton, TierBadge } from "@/components/ui";

export default function ModelsPage() {
  const { me } = useAuth();
  const models = useData(() => api.get<{ models: Model[] }>("/v1/models"), []);
  const pricing = useData(() => api.get<Pricing>("/v1/pricing", false), []);
  const currency = me?.organization.currency ?? "USD";
  const rate = pricing.data?.fx_from_usd?.[currency];
  const spot = (pricing.data?.spot_price_percent ?? 55) / 100;
  const show = (m: Model["price_in_per_1m"]) => money(rate ? convert(m, currency, rate) : m);

  return (
    <>
      <PageHeader
        title="Models"
        description={`Open-source models you can deploy. Prices are per million tokens in ${currency}; spot (T3) is ${Math.round(spot * 100)}% of on-demand.`}
      />
      {models.error && <Notice tone="error">{models.error.message}</Notice>}
      <Panel flush>
        {!models.data ? (
          <div className="grid gap-3 p-5">
            {[0, 1, 2].map((i) => (
              <Skeleton key={i} className="h-14" />
            ))}
          </div>
        ) : (
          <ul className="divide-y divide-line">
            {models.data.models.map((m) => (
              <li key={m.id} className="grid gap-4 px-5 py-5 md:grid-cols-[minmax(0,1.6fr)_minmax(0,1fr)_auto] md:items-center">
                <div className="min-w-0">
                  <div className="flex flex-wrap items-center gap-2">
                    <h2 className="text-[16px] font-semibold">{m.name}</h2>
                    {m.is_byo && <span className="rounded-md bg-surface-2 px-1.5 py-0.5 text-[12px] text-muted">Your model</span>}
                  </div>
                  {m.description && <p className="mt-1 max-w-[62ch] text-[14px] text-muted">{m.description}</p>}
                  <p className="mt-1.5 text-[13px] text-muted">
                    {m.params_b}B parameters{m.context_length ? `, ${Math.round(m.context_length / 1024)}K context` : ""}, {m.license}
                  </p>
                </div>
                <div className="text-[14px]">
                  <div>
                    {show(m.price_in_per_1m)} in, {show(m.price_out_per_1m)} out
                  </div>
                  <div className="mt-1.5 flex flex-wrap gap-1.5">
                    {m.tiers_allowed.map((t) => (
                      <span key={t} title={TIERS[t].who}>
                        <TierBadge tier={t} />
                      </span>
                    ))}
                  </div>
                </div>
                <ButtonLink href={`/app/deploy?model=${m.id}`} variant="secondary">
                  Deploy
                </ButtonLink>
              </li>
            ))}
          </ul>
        )}
      </Panel>
    </>
  );
}
