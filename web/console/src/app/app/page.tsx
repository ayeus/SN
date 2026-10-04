"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { Activity, Boxes, CalendarDays, Check, Hash } from "lucide-react";
import { Area, AreaChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { api } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { useData } from "@/lib/hooks";
import { compact, number } from "@/lib/format";
import type { ApiKey, Deployment } from "@/lib/types";
import { ButtonLink, Empty, PageHeader, Panel, Skeleton, Stat, StateBadge, TierBadge, cx } from "@/components/ui";
import { PowerRail } from "@/components/PowerRail";

type UsageDay = { date: string; requests: number; errors: number; tokens: number };

// Every step is derived from the account's real state; nothing is ticked by
// default.
function GettingStarted({ deployments, keys, requests }: { deployments: Deployment[]; keys: ApiKey[] | null; requests: number }) {
  const steps = [
    { done: deployments.length > 0, label: "Deploy a model", href: "/app/models", hint: "Pick one from the catalogue" },
    {
      done: deployments.some((d) => d.state === "serving" || d.state === "degraded"),
      label: "Wait for it to serve",
      href: deployments[0] ? `/app/deployments/${deployments[0].id}` : "/app/deployments",
      hint: "Watch it power on",
    },
    { done: requests > 0, label: "Send a request", href: "/app/playground", hint: "Try it in the playground" },
    { done: (keys?.length ?? 0) > 0, label: "Create an API key for your app", href: "/app/keys", hint: "Call it from your code" },
  ];
  const done = steps.filter((s) => s.done).length;
  if (done === steps.length) return null;
  return (
    <Panel title="Get started" description={`${done} of ${steps.length} done`} className="mb-6">
      <ol className="grid gap-2 sm:grid-cols-2 lg:grid-cols-4">
        {steps.map((s) => (
          <li key={s.label}>
            <Link
              href={s.href}
              className={cx(
                "flex h-full items-start gap-3 rounded-xl border p-3.5 transition-colors",
                s.done ? "border-serving/30 bg-serving-soft" : "border-line hover:border-nil/40 hover:bg-surface-2",
              )}
            >
              <span
                aria-hidden
                className={cx(
                  "mt-0.5 grid h-5 w-5 shrink-0 place-items-center rounded-full border",
                  s.done ? "border-serving bg-serving text-white" : "border-line",
                )}
              >
                {s.done && <Check size={12} strokeWidth={3} />}
              </span>
              <span className="min-w-0">
                <span className="block text-[14px] font-medium">{s.label}</span>
                <span className="block text-[13px] text-muted">{s.done ? "Done" : s.hint}</span>
              </span>
            </Link>
          </li>
        ))}
      </ol>
    </Panel>
  );
}

export default function OverviewPage() {
  const { me } = useAuth();
  const router = useRouter();
  const deps = useData(() => api.get<{ deployments: Deployment[] }>("/v1/deployments"), [], 5000);
  const keys = useData(() => api.get<{ api_keys: ApiKey[] }>("/v1/api-keys"), []);
  const usage = useData(() => api.get<{ days: UsageDay[] }>("/v1/usage/daily"), [], 60_000);

  const list = deps.data?.deployments ?? [];
  const serving = list.filter((d) => d.state === "serving" || d.state === "degraded").length;
  const requests = list.reduce((n, d) => n + (d.usage_24h?.requests ?? 0), 0);
  const tokens = list.reduce((n, d) => n + (d.usage_24h?.input_tokens ?? 0) + (d.usage_24h?.output_tokens ?? 0), 0);
  const days = usage.data?.days ?? [];
  const monthRequests = days.reduce((n, d) => n + d.requests, 0);
  const monthTokens = days.reduce((n, d) => n + d.tokens, 0);
  const series = days.slice(-14).map((d) => ({ day: d.date.slice(5), requests: d.requests, tokens: d.tokens }));

  return (
    <>
      <PageHeader
        title={`Hello, ${me?.user.name.split(" ")[0]}`}
        description="Your deployments and their traffic at a glance."
        actions={<ButtonLink href="/app/models">Deploy a model</ButtonLink>}
      />

      <div className="mb-6 grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <Panel>
          <Stat icon={Boxes} tone="serving" label="Serving" value={deps.data ? `${serving} of ${list.length}` : <Skeleton className="h-7 w-16" />} sub="deployments" />
        </Panel>
        <Panel>
          <Stat icon={Activity} tone="tier2" label="Requests today" value={deps.data ? number(requests) : <Skeleton className="h-7 w-16" />} />
        </Panel>
        <Panel>
          <Stat icon={Hash} tone="marigold" label="Tokens today" value={deps.data ? compact(tokens) : <Skeleton className="h-7 w-16" />} />
        </Panel>
        <Panel>
          <Stat icon={CalendarDays} label="Requests, 30 days" value={usage.data ? number(monthRequests) : <Skeleton className="h-7 w-20" />} sub={usage.data ? `${compact(monthTokens)} tokens` : undefined} />
        </Panel>
      </div>

      {deps.data && <GettingStarted deployments={list} keys={keys.data?.api_keys ?? null} requests={monthRequests} />}

      <div className="grid gap-6 xl:grid-cols-[minmax(0,1.6fr)_minmax(0,1fr)]">
        <Panel
          title="Deployments"
          actions={
            <Link href="/app/deployments" className="text-[13px] text-muted hover:text-ink">
              View all
            </Link>
          }
          flush
        >
          {deps.error ? (
            <p className="p-5 text-danger">Could not load deployments: {deps.error.message}</p>
          ) : deps.loading && !deps.data ? (
            <div className="grid gap-3 p-5">
              <Skeleton className="h-10" />
              <Skeleton className="h-10" />
            </div>
          ) : list.length === 0 ? (
            <Empty title="Deploy your first model" action={<ButtonLink href="/app/models">Browse models</ButtonLink>}>
              Pick a model, choose a tier, and you get an OpenAI-compatible endpoint. A warm GPU is serving in under a
              minute.
            </Empty>
          ) : (
            <div className="overflow-x-auto">
              <table className="table min-w-[640px]">
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Status</th>
                    <th className="w-[150px]">Progress</th>
                    <th>Tier</th>
                    <th className="text-right">Requests today</th>
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
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>

        <Panel title="Requests, last 14 days">
          {usage.error ? (
            <p className="text-danger">Could not load usage: {usage.error.message}</p>
          ) : !usage.data ? (
            <Skeleton className="h-[200px]" />
          ) : series.length === 0 ? (
            <p className="py-10 text-muted">No usage yet. Traffic appears here once your deployments serve requests.</p>
          ) : (
            <div className="h-[200px]" role="img" aria-label={`Daily requests for the last ${series.length} days`}>
              <ResponsiveContainer width="100%" height="100%">
                <AreaChart data={series} margin={{ left: -18, right: 6, top: 6 }}>
                  <defs>
                    <linearGradient id="traffic" x1="0" y1="0" x2="0" y2="1">
                      <stop offset="0%" stopColor="var(--nil)" stopOpacity={0.35} />
                      <stop offset="100%" stopColor="var(--nil)" stopOpacity={0} />
                    </linearGradient>
                  </defs>
                  <CartesianGrid stroke="var(--line)" vertical={false} />
                  <XAxis dataKey="day" tick={{ fill: "var(--muted)", fontSize: 12 }} tickLine={false} axisLine={false} />
                  <YAxis tick={{ fill: "var(--muted)", fontSize: 12 }} tickLine={false} axisLine={false} allowDecimals={false} />
                  <Tooltip
                    contentStyle={{ background: "var(--surface)", border: "1px solid var(--line)", borderRadius: 10, fontSize: 13 }}
                    formatter={(v) => [number(Number(v)), "Requests"]}
                  />
                  <Area type="monotone" dataKey="requests" stroke="var(--nil)" strokeWidth={2} fill="url(#traffic)" />
                </AreaChart>
              </ResponsiveContainer>
            </div>
          )}
        </Panel>
      </div>
    </>
  );
}
