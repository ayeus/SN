"use client";

import { Bar, BarChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { api } from "@/lib/api";
import { useData } from "@/lib/hooks";
import { compact, number } from "@/lib/format";
import type { HostActivity } from "@/lib/types";
import { PageHeader, Panel, Skeleton, Stat } from "@/components/ui";

export default function ActivityPage() {
  const a = useData(() => api.get<HostActivity>("/v1/hosts/activity"), [], 30_000);

  return (
    <>
      <PageHeader title="Activity" description="The requests your machines have served, across every machine you own." />

      <Panel className="mb-6">
        {!a.data ? (
          <Skeleton className="h-16" />
        ) : (
          <div className="grid grid-cols-2 gap-6 md:grid-cols-4">
            <Stat label="Requests today" value={number(a.data.summary.today.requests)} sub={`${compact(a.data.summary.today.tokens)} tokens`} />
            <Stat label="This month" value={number(a.data.summary.month_to_date.requests)} sub={`${compact(a.data.summary.month_to_date.tokens)} tokens`} />
            <Stat label="All time" value={number(a.data.summary.lifetime.requests)} />
            <Stat label="Tokens served" value={compact(a.data.summary.lifetime.tokens)} />
          </div>
        )}
      </Panel>

      <Panel title="Requests, last 30 days">
        {a.data && a.data.daily.length > 0 ? (
          <div className="h-[260px]" role="img" aria-label={`Requests served per day for the last ${a.data.daily.length} days`}>
            <ResponsiveContainer width="100%" height="100%">
              <BarChart data={a.data.daily.map((d) => ({ date: d.date.slice(5), requests: d.requests, tokens: d.tokens }))} margin={{ left: -8, right: 8, top: 8 }}>
                <CartesianGrid stroke="var(--line)" vertical={false} />
                <XAxis dataKey="date" tick={{ fill: "var(--muted)", fontSize: 12 }} tickLine={false} axisLine={false} />
                <YAxis tick={{ fill: "var(--muted)", fontSize: 12 }} tickLine={false} axisLine={false} allowDecimals={false} />
                <Tooltip
                  contentStyle={{ background: "var(--surface)", border: "1px solid var(--line)", borderRadius: 8, fontSize: 13 }}
                  formatter={(v) => [number(Number(v)), "Requests"]}
                />
                <Bar dataKey="requests" fill="var(--nil)" radius={[4, 4, 0, 0]} />
              </BarChart>
            </ResponsiveContainer>
          </div>
        ) : (
          <p className="py-6 text-muted">Nothing served yet. Requests appear here once your machines take work.</p>
        )}
      </Panel>
    </>
  );
}
