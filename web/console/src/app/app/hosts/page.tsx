"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { api } from "@/lib/api";
import { useData } from "@/lib/hooks";
import { ago, money } from "@/lib/format";
import type { HostSummary, Money } from "@/lib/types";
import { ButtonLink, Empty, Notice, PageHeader, Panel, Skeleton, Stat, StateBadge, TierBadge } from "@/components/ui";

type Earnings = { summary: { currency: string; today: Money; month_to_date: Money; lifetime: Money; requests: number } };

export default function MachinesPage() {
  const router = useRouter();
  const hosts = useData(() => api.get<{ hosts: HostSummary[] }>("/v1/hosts"), [], 5000);
  const earnings = useData(() => api.get<Earnings>("/v1/hosts/earnings"), [], 30_000);
  const list = hosts.data?.hosts ?? [];
  const online = list.filter((h) => h.online).length;

  return (
    <>
      <PageHeader
        title="Machines"
        description="The GPUs you've connected, what they're running, and what they've earned."
        actions={<ButtonLink href="/app/hosts/new">Add a machine</ButtonLink>}
      />
      {hosts.error && <Notice tone="error" className="mb-4">{hosts.error.message}</Notice>}

      {list.length > 0 && (
        <Panel className="mb-6">
          <div className="grid grid-cols-2 gap-6 md:grid-cols-4">
            <Stat label="Online now" value={`${online} of ${list.length}`} />
            <Stat label="Earned today" value={earnings.data ? money(earnings.data.summary.today) : <Skeleton className="h-7 w-20" />} />
            <Stat label="This month" value={earnings.data ? money(earnings.data.summary.month_to_date) : <Skeleton className="h-7 w-20" />} />
            <Stat label="All time" value={earnings.data ? money(earnings.data.summary.lifetime) : <Skeleton className="h-7 w-20" />} />
          </div>
        </Panel>
      )}

      <Panel flush>
        {!hosts.data ? (
          <div className="grid gap-3 p-5">
            <Skeleton className="h-10" />
          </div>
        ) : list.length === 0 ? (
          <Empty title="Connect your first GPU" action={<ButtonLink href="/app/hosts/new">Add a machine</ButtonLink>}>
            Install one agent on a machine with a GPU. It appears here within a minute and starts taking work once it
            passes its benchmark.
          </Empty>
        ) : (
          <div className="overflow-x-auto">
            <table className="table min-w-[860px]">
              <thead>
                <tr>
                  <th>Machine</th>
                  <th>Status</th>
                  <th>Tier</th>
                  <th className="text-right">Reputation</th>
                  <th className="text-right">Jobs</th>
                  <th className="text-right">Today</th>
                  <th className="text-right">All time</th>
                  <th className="text-right">Last seen</th>
                </tr>
              </thead>
              <tbody>
                {list.map((h) => (
                  <tr key={h.id} data-href onClick={() => router.push(`/app/hosts/${h.id}`)}>
                    <td>
                      <Link href={`/app/hosts/${h.id}`} className="font-medium hover:underline">
                        {h.name}
                      </Link>
                      <div className="text-[13px] text-muted">
                        {h.gpus.map((g) => `${g.model} ${g.vram_gb} GB`).join(", ") || "No GPU reported"}
                      </div>
                    </td>
                    <td>
                      {h.online ? <StateBadge state={h.paused ? "paused" : h.status} /> : <StateBadge state="offline" />}
                      {h.online && !h.runtime_healthy && <div className="text-[12px] text-danger">Runtime not reachable</div>}
                    </td>
                    <td>
                      <TierBadge tier={h.tier} />
                    </td>
                    <td className="text-right">{h.reputation}</td>
                    <td className="text-right">{h.active_jobs}</td>
                    <td className="text-right">{money(h.earnings_today)}</td>
                    <td className="text-right">{money(h.earnings_total)}</td>
                    <td className="text-right text-muted">{h.online ? "now" : ago(h.last_heartbeat_at)}</td>
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
