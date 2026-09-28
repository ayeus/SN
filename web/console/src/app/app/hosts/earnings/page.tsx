"use client";

import { Bar, BarChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { api } from "@/lib/api";
import { useData } from "@/lib/hooks";
import { money, number } from "@/lib/format";
import type { HostSummary, Money } from "@/lib/types";
import { Notice, PageHeader, Panel, Skeleton, Stat } from "@/components/ui";

type Earnings = {
  summary: { currency: string; today: Money; month_to_date: Money; lifetime: Money; requests: number; tokens: number };
  daily: { date: string; earnings: Money; requests: number }[];
};

export default function EarningsPage() {
  const e = useData(() => api.get<Earnings>("/v1/hosts/earnings"), [], 30_000);
  const hosts = useData(() => api.get<{ hosts: HostSummary[] }>("/v1/hosts"), []);
  const kyc = hosts.data?.hosts[0]?.kyc_status;

  return (
    <>
      <PageHeader title="Earnings" description="Your share of what customers paid for your machines' time, across every machine you own." />

      {kyc && kyc !== "verified" && (
        <Notice className="mb-6">
          Earnings accrue from the first request. Payouts go out weekly once KYC is complete (PAN and a verified bank account),
          with 1% TDS withheld under section 194-O and Form 16A issued quarterly.
        </Notice>
      )}

      <Panel className="mb-6">
        {!e.data ? (
          <Skeleton className="h-16" />
        ) : (
          <div className="grid grid-cols-2 gap-6 md:grid-cols-4">
            <Stat label="Today" value={money(e.data.summary.today)} />
            <Stat label="This month" value={money(e.data.summary.month_to_date)} />
            <Stat label="All time" value={money(e.data.summary.lifetime)} />
            <Stat label="Requests served" value={number(e.data.summary.requests)} />
          </div>
        )}
      </Panel>

      <Panel title="Last 30 days">
        {e.data && e.data.daily.length > 0 ? (
          <div className="h-[260px]">
            <ResponsiveContainer width="100%" height="100%">
              <BarChart data={e.data.daily.map((d) => ({ date: d.date.slice(5), value: Number(d.earnings.amount), requests: d.requests }))} margin={{ left: -8, right: 8, top: 8 }}>
                <CartesianGrid stroke="var(--line)" vertical={false} />
                <XAxis dataKey="date" tick={{ fill: "var(--muted)", fontSize: 12 }} tickLine={false} axisLine={false} />
                <YAxis tick={{ fill: "var(--muted)", fontSize: 12 }} tickLine={false} axisLine={false} />
                <Tooltip
                  contentStyle={{ background: "var(--surface)", border: "1px solid var(--line)", borderRadius: 8, fontSize: 13 }}
                  formatter={(v) => [money({ amount: String(v), currency: e.data!.summary.currency, micros: 0 }, { precise: true }), "Earned"]}
                />
                <Bar dataKey="value" fill="var(--nil)" radius={[4, 4, 0, 0]} />
              </BarChart>
            </ResponsiveContainer>
          </div>
        ) : (
          <p className="py-6 text-muted">Nothing earned yet. Earnings appear here as your machines serve requests.</p>
        )}
      </Panel>
    </>
  );
}
