"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { api } from "@/lib/api";
import { useData } from "@/lib/hooks";
import { ago, compact, number } from "@/lib/format";
import type { HostActivity, HostSummary } from "@/lib/types";
import { ButtonLink, Empty, Notice, PageHeader, Panel, Skeleton, Stat, StateBadge, TierBadge } from "@/components/ui";

export default function MachinesPage() {
  const router = useRouter();
  const hosts = useData(() => api.get<{ hosts: HostSummary[] }>("/v1/hosts"), [], 5000);
  const activity = useData(() => api.get<HostActivity>("/v1/hosts/activity"), [], 30_000);
  const list = hosts.data?.hosts ?? [];
  const online = list.filter((h) => h.online).length;

  return (
    <>
      <PageHeader
        title="Machines"
        description="The GPUs you've connected, what they're running, and the work they've served."
        actions={<ButtonLink href="/app/hosts/new">Add a machine</ButtonLink>}
      />
      {hosts.error && <Notice tone="error" className="mb-4">{hosts.error.message}</Notice>}

      {list.length > 0 && (
        <Panel className="mb-6">
          <div className="grid grid-cols-2 gap-6 md:grid-cols-4">
            <Stat label="Online now" value={`${online} of ${list.length}`} />
            <Stat label="Requests today" value={activity.data ? number(activity.data.summary.today.requests) : <Skeleton className="h-7 w-20" />} sub={activity.data ? `${compact(activity.data.summary.today.tokens)} tokens` : undefined} />
            <Stat label="This month" value={activity.data ? number(activity.data.summary.month_to_date.requests) : <Skeleton className="h-7 w-20" />} sub={activity.data ? `${compact(activity.data.summary.month_to_date.tokens)} tokens` : undefined} />
            <Stat label="All time" value={activity.data ? number(activity.data.summary.lifetime.requests) : <Skeleton className="h-7 w-20" />} sub={activity.data ? `${compact(activity.data.summary.lifetime.tokens)} tokens` : undefined} />
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
                  <th className="text-right">Requests today</th>
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
                      {h.agent_outdated && <div className="text-[12px] text-danger">Agent out of date</div>}
                    </td>
                    <td>
                      <TierBadge tier={h.tier} />
                    </td>
                    <td className="text-right">{h.reputation}</td>
                    <td className="text-right">{h.active_jobs}</td>
                    <td className="text-right">{number(h.requests_today)}</td>
                    <td className="text-right">{number(h.requests_total)}</td>
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
