"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { Suspense } from "react";
import { api } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { useData } from "@/lib/hooks";
import { compact, money, number } from "@/lib/format";
import type { Deployment, Wallet } from "@/lib/types";
import { ButtonLink, Empty, Notice, PageHeader, Panel, Skeleton, Stat, StateBadge, TierBadge } from "@/components/ui";
import { PowerRail } from "@/components/PowerRail";

function Overview() {
  const { me } = useAuth();
  const router = useRouter();
  const deps = useData(() => api.get<{ deployments: Deployment[] }>("/v1/deployments"), [], 5000);
  const wallet = useData(() => api.get<Wallet>("/v1/billing/wallet"), [], 15000);

  const list = deps.data?.deployments ?? [];
  const serving = list.filter((d) => d.state === "serving" || d.state === "degraded").length;
  const requests = list.reduce((n, d) => n + (d.usage_24h?.requests ?? 0), 0);
  const tokens = list.reduce((n, d) => n + (d.usage_24h?.input_tokens ?? 0) + (d.usage_24h?.output_tokens ?? 0), 0);

  return (
    <>
      <PageHeader
        title={`Hello, ${me?.user.name.split(" ")[0]}`}
        description="Your deployments, spend and wallet at a glance."
        actions={<ButtonLink href="/app/models">Deploy a model</ButtonLink>}
      />

      {wallet.data?.low_balance && (
        <Notice tone="warn" className="mb-6">
          Your wallet is running low ({money(wallet.data.balance, { balance: true })}). Deployments pause automatically when it reaches zero.{" "}
          <Link href="/app/billing" className="font-medium underline">
            Top up
          </Link>
        </Notice>
      )}

      <Panel className="mb-6">
        <div className="grid grid-cols-2 gap-6 md:grid-cols-4">
          <Stat label="Wallet balance" value={wallet.data ? money(wallet.data.balance, { balance: true }) : <Skeleton className="h-7 w-24" />} />
          <Stat label="Spent today" value={wallet.data ? money(wallet.data.spend_24h) : <Skeleton className="h-7 w-20" />} />
          <Stat label="Serving deployments" value={deps.data ? `${serving} of ${list.length}` : <Skeleton className="h-7 w-16" />} />
          <Stat label="Requests today" value={deps.data ? number(requests) : <Skeleton className="h-7 w-16" />} sub={deps.data ? `${compact(tokens)} tokens` : undefined} />
        </div>
      </Panel>

      <Panel title="Deployments" actions={<Link href="/app/deployments" className="text-[13px] text-muted hover:text-ink">View all</Link>} flush>
        {deps.loading && !deps.data ? (
          <div className="grid gap-3 p-5">
            <Skeleton className="h-10" />
            <Skeleton className="h-10" />
          </div>
        ) : list.length === 0 ? (
          <Empty
            title="Deploy your first model"
            action={<ButtonLink href="/app/models">Browse models</ButtonLink>}
          >
            Pick a model, choose a tier, and you get an OpenAI-compatible endpoint. Most deployments on a warm GPU are
            serving in under a minute.
          </Empty>
        ) : (
          <div className="overflow-x-auto">
            <table className="table min-w-[760px]">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Status</th>
                  <th className="w-[180px]">Progress</th>
                  <th>Tier</th>
                  <th className="text-right">Requests today</th>
                  <th className="text-right">Cost today</th>
                </tr>
              </thead>
              <tbody>
                {list.slice(0, 8).map((d) => (
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
                      <PowerRail state={d.state} compact />
                    </td>
                    <td>
                      <TierBadge tier={d.tier} />
                    </td>
                    <td className="text-right">{number(d.usage_24h?.requests ?? 0)}</td>
                    <td className="text-right">{money(d.usage_24h?.cost)}</td>
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

export default function OverviewPage() {
  return (
    <Suspense>
      <Overview />
    </Suspense>
  );
}
