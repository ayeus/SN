"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useState } from "react";
import { api } from "@/lib/api";
import { useData } from "@/lib/hooks";
import { ago, money, number } from "@/lib/format";
import type { Deployment } from "@/lib/types";
import { ButtonLink, Empty, Notice, PageHeader, Panel, Skeleton, StateBadge, TierBadge } from "@/components/ui";

export default function DeploymentsPage() {
  const router = useRouter();
  const [showStopped, setShowStopped] = useState(false);
  const deps = useData(
    () => api.get<{ deployments: Deployment[] }>(`/v1/deployments${showStopped ? "?include_stopped=true" : ""}`),
    [showStopped],
    5000,
  );
  const list = deps.data?.deployments ?? [];

  return (
    <>
      <PageHeader
        title="Deployments"
        description="Every model you've deployed, with its state and today's usage."
        actions={<ButtonLink href="/app/models">Deploy a model</ButtonLink>}
      />
      {deps.error && <Notice tone="error" className="mb-4">{deps.error.message}</Notice>}
      <Panel
        flush
        actions={
          <label className="flex items-center gap-2 text-[13px] text-muted">
            <input type="checkbox" checked={showStopped} onChange={(e) => setShowStopped(e.target.checked)} className="accent-[var(--nil)]" />
            Show stopped
          </label>
        }
        title={`${list.length} ${list.length === 1 ? "deployment" : "deployments"}`}
      >
        {!deps.data ? (
          <div className="grid gap-3 p-5">
            <Skeleton className="h-10" />
            <Skeleton className="h-10" />
          </div>
        ) : list.length === 0 ? (
          <Empty title="No deployments yet" action={<ButtonLink href="/app/models">Browse models</ButtonLink>}>
            A deployment gives you an OpenAI-compatible endpoint for one model.
          </Empty>
        ) : (
          <div className="overflow-x-auto">
            <table className="table min-w-[820px]">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Status</th>
                  <th>Where</th>
                  <th className="text-right">Replicas</th>
                  <th className="text-right">Requests today</th>
                  <th className="text-right">Cost today</th>
                  <th className="text-right">Created</th>
                </tr>
              </thead>
              <tbody>
                {list.map((d) => (
                  <tr key={d.id} data-href onClick={() => router.push(`/app/deployments/${d.id}`)}>
                    <td>
                      <Link href={`/app/deployments/${d.id}`} className="font-medium hover:underline">
                        {d.name}
                      </Link>
                      <div className="text-[13px] text-muted">{d.model_name}</div>
                    </td>
                    <td>
                      <StateBadge state={d.state} />
                    </td>
                    <td>
                      <span className="flex items-center gap-2">
                        <TierBadge tier={d.tier} /> <span className="text-[13px] text-muted">{d.region}</span>
                      </span>
                    </td>
                    <td className="text-right">
                      {d.replicas_serving}/{Math.max(d.min_replicas, 1)}
                    </td>
                    <td className="text-right">{number(d.usage_24h?.requests ?? 0)}</td>
                    <td className="text-right">{money(d.usage_24h?.cost)}</td>
                    <td className="text-right text-muted">{ago(d.created_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>
    </>
  );
}
