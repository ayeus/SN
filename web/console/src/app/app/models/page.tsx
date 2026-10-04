"use client";

import { api } from "@/lib/api";
import { useData } from "@/lib/hooks";
import { TIERS } from "@/lib/format";
import type { Model } from "@/lib/types";
import { ButtonLink, Notice, PageHeader, Panel, Skeleton, TierBadge } from "@/components/ui";

export default function ModelsPage() {
  const models = useData(() => api.get<{ models: Model[] }>("/v1/models"), []);

  return (
    <>
      <PageHeader
        title="Models"
        description="Open-source models you can deploy. Each one shows the GPU memory it needs and the tiers it can run on."
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
                  <div>Needs {m.min_vram_gb} GB of GPU memory</div>
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
