"use client";

import { useParams, useRouter } from "next/navigation";
import { useState } from "react";
import { Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis, CartesianGrid } from "recharts";
import { api, ApiError } from "@/lib/api";
import { useData } from "@/lib/hooks";
import { ago, dateTime, money, number, time } from "@/lib/format";
import type { GPU, Host, Money } from "@/lib/types";
import { Button, Confirm, Empty, Input, Notice, PageHeader, Panel, Skeleton, Stat, StateBadge, TierBadge } from "@/components/ui";

type Detail = {
  host: Host;
  online: boolean;
  gpus: GPU[] | null;
  benchmark: { score_compute?: number; vram_bw_gbps?: number; disk_read_mbps?: number; disk_write_mbps?: number; latency_pop_ms?: number; ran_at: string } | null;
  reputation: { score: number; uptime_pct: number; correctness_pct?: number; benchmark_stability?: number; age_days: number; incident_rate?: number; computed_at: string } | null;
  jobs: { id: string; model_name: string; state: string; detail?: string; started_at?: string; requests: number; failed: number }[];
  incidents: { kind: string; severity: string; action?: string; resolved: boolean; created_at: string }[];
};
type Telemetry = { samples: { ts: string; cpu_pct: number; mem_pct: number; gpus: { utilization_pct?: number }[] | null }[] };
type Earnings = { summary: { today: Money; month_to_date: Money; lifetime: Money; requests: number } };

function Meter({ label, value, weight, missing }: { label: string; value?: number; weight: string; missing: string }) {
  return (
    <div className="grid grid-cols-[1fr_auto] items-center gap-x-4 gap-y-1.5">
      <span className="text-[14px]">
        {label} <span className="text-muted">({weight})</span>
      </span>
      <span className="text-[14px] font-medium">{value == null ? <span className="text-muted">{missing}</span> : `${Math.round(value)}`}</span>
      <div className="col-span-2 h-1.5 rounded-full bg-surface-2">
        {value != null && <div className="h-full rounded-full bg-nil" style={{ width: `${Math.max(2, Math.min(100, value))}%` }} />}
      </div>
    </div>
  );
}

export default function HostPage() {
  const { id } = useParams<{ id: string }>();
  const router = useRouter();
  const detail = useData(() => api.get<Detail>(`/v1/hosts/${id}`), [id], 5000);
  const telemetry = useData(() => api.get<Telemetry>(`/v1/hosts/${id}/telemetry?minutes=60`), [id], 15_000);
  const earnings = useData(() => api.get<Earnings>(`/v1/hosts/${id}/earnings`), [id], 30_000);
  const [renaming, setRenaming] = useState<string | null>(null);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [confirm, setConfirm] = useState(false);

  if (detail.error?.status === 404) return <Notice tone="error">This machine doesn&apos;t exist or isn&apos;t yours.</Notice>;
  if (!detail.data) {
    return (
      <div className="grid gap-4">
        <Skeleton className="h-10 w-64" />
        <Skeleton className="h-40" />
      </div>
    );
  }
  const { host: h, online, gpus, benchmark, reputation, jobs, incidents } = detail.data;

  async function controls(body: Record<string, unknown>, label: string) {
    setBusy(label);
    setError("");
    try {
      await api.patch(`/v1/hosts/${id}/controls`, body);
      await detail.reload();
      setRenaming(null);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Update failed");
    } finally {
      setBusy("");
    }
  }

  const points = (telemetry.data?.samples ?? []).map((s) => ({
    t: time(s.ts).slice(0, 5),
    cpu: Math.round(s.cpu_pct),
    mem: Math.round(s.mem_pct),
    gpu: s.gpus?.[0]?.utilization_pct != null ? Math.round(s.gpus[0].utilization_pct) : undefined,
  }));

  return (
    <>
      <PageHeader
        back={{ href: "/app/hosts", label: "Machines" }}
        title={
          renaming != null ? (
            <form
              className="flex gap-2"
              onSubmit={(e) => {
                e.preventDefault();
                controls({ name: renaming }, "rename");
              }}
            >
              <Input value={renaming} onChange={(e) => setRenaming(e.target.value)} aria-label="Machine name" autoFocus className="h-10 text-[18px]" />
              <Button type="submit" loading={busy === "rename"}>
                Save
              </Button>
              <Button type="button" variant="ghost" onClick={() => setRenaming(null)}>
                Cancel
              </Button>
            </form>
          ) : (
            h.name
          )
        }
        description={
          <span className="flex flex-wrap items-center gap-3">
            {online ? <StateBadge state={h.paused ? "paused" : h.status} /> : <StateBadge state="offline" label={`Offline, last seen ${ago(h.last_heartbeat_at)}`} />}
            <TierBadge tier={h.tier} long />
            <span>{h.region}</span>
            {h.runtime && <span>{h.runtime}{h.runtime_healthy ? "" : " (not reachable)"}</span>}
          </span>
        }
        actions={
          renaming == null && (
            <>
              <Button variant="ghost" onClick={() => setRenaming(h.name)}>
                Rename
              </Button>
              {h.paused ? (
                <Button onClick={() => controls({ paused: false }, "pause")} loading={busy === "pause"}>
                  Resume
                </Button>
              ) : (
                <Button variant="secondary" onClick={() => controls({ paused: true }, "pause")} loading={busy === "pause"}>
                  Pause
                </Button>
              )}
              <Button variant="danger" onClick={() => setConfirm(true)}>
                Remove
              </Button>
            </>
          )
        }
      />
      {error && <Notice tone="error" className="mb-4">{error}</Notice>}
      {h.status === "probation" && h.probation_until && (
        <Notice className="mb-6">
          On probation until {dateTime(h.probation_until)}. During probation the machine takes spot work only; it moves up once
          its uptime and reputation hold.
        </Notice>
      )}
      {online && !h.runtime_healthy && (
        <Notice tone="warn" className="mb-6">
          The agent is connected but can&apos;t reach its model runtime, so it won&apos;t receive jobs. Start {h.runtime ?? "Ollama"} on the machine.
        </Notice>
      )}

      <Panel className="mb-6">
        <div className="grid grid-cols-2 gap-6 md:grid-cols-4">
          <Stat label="Earned today" value={earnings.data ? money(earnings.data.summary.today) : "-"} />
          <Stat label="This month" value={earnings.data ? money(earnings.data.summary.month_to_date) : "-"} />
          <Stat label="All time" value={earnings.data ? money(earnings.data.summary.lifetime) : "-"} />
          <Stat label="Requests served" value={earnings.data ? number(earnings.data.summary.requests) : "-"} />
        </div>
      </Panel>

      <div className="mb-6 grid gap-6 lg:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)]">
        <Panel title="Load, last hour">
          {points.length > 1 ? (
            <div className="h-[220px]">
              <ResponsiveContainer width="100%" height="100%">
                <LineChart data={points} margin={{ left: -20, right: 8, top: 8 }}>
                  <CartesianGrid stroke="var(--line)" vertical={false} />
                  <XAxis dataKey="t" tick={{ fill: "var(--muted)", fontSize: 12 }} tickLine={false} axisLine={false} minTickGap={32} />
                  <YAxis domain={[0, 100]} tick={{ fill: "var(--muted)", fontSize: 12 }} tickLine={false} axisLine={false} unit="%" />
                  <Tooltip contentStyle={{ background: "var(--surface)", border: "1px solid var(--line)", borderRadius: 8, fontSize: 13 }} />
                  <Line type="monotone" dataKey="cpu" name="CPU" stroke="var(--nil)" dot={false} strokeWidth={2} />
                  <Line type="monotone" dataKey="mem" name="Memory" stroke="var(--tier2)" dot={false} strokeWidth={2} />
                  <Line type="monotone" dataKey="gpu" name="GPU" stroke="var(--marigold)" dot={false} strokeWidth={2} connectNulls />
                </LineChart>
              </ResponsiveContainer>
            </div>
          ) : (
            <p className="py-6 text-muted">Samples appear here while the machine is online.</p>
          )}
        </Panel>

        <Panel title={`Reputation: ${h.reputation}`} description="Recomputed hourly. Below 40 the machine stops receiving new work.">
          {reputation ? (
            <div className="grid gap-4">
              <Meter label="Uptime" weight="40%" value={reputation.uptime_pct} missing="-" />
              <Meter label="Request success" weight="25%" value={reputation.correctness_pct} missing="no traffic yet" />
              <Meter label="Benchmark stability" weight="15%" value={reputation.benchmark_stability} missing="needs 2 runs" />
              <Meter label="Age" weight="10%" value={Math.min(100, (reputation.age_days / 30) * 100)} missing="-" />
              <p className="text-[12px] text-muted">
                Incidents in the last 30 days: {reputation.incident_rate ?? 0}. Computed {ago(reputation.computed_at)}.
              </p>
            </div>
          ) : (
            <p className="text-muted">The first score is computed within an hour of joining.</p>
          )}
        </Panel>
      </div>

      <div className="mb-6 grid gap-6 lg:grid-cols-2">
        <Panel title="Hardware" flush>
          <table className="table">
            <tbody>
              {(gpus ?? []).map((g) => (
                <tr key={g.id}>
                  <td>
                    <div className="font-medium">{g.model}</div>
                    <div className="font-mono text-[12px] text-muted">{g.uuid}</div>
                  </td>
                  <td className="text-right">{g.vram_gb} GB</td>
                  <td className="text-right text-[13px] text-muted">{g.status === "available" ? "Free" : "In use"}</td>
                </tr>
              ))}
              {benchmark && (
                <tr>
                  <td colSpan={3} className="text-[13px] text-muted">
                    Benchmark {ago(benchmark.ran_at)}:{" "}
                    {[
                      benchmark.vram_bw_gbps && `memory ${benchmark.vram_bw_gbps.toFixed(1)} GB/s`,
                      benchmark.disk_read_mbps && `disk read ${Math.round(benchmark.disk_read_mbps)} MB/s`,
                      benchmark.latency_pop_ms && `latency ${benchmark.latency_pop_ms.toFixed(1)} ms`,
                      !benchmark.score_compute && "GPU compute not measured on this hardware",
                    ]
                      .filter(Boolean)
                      .join(", ")}
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </Panel>

        <Panel title="Jobs" description="You see which model runs and how busy it is, never the prompts." flush>
          {jobs.length === 0 ? (
            <Empty title="No jobs yet">
              {online ? "The scheduler sends work here as customers deploy models that fit this GPU." : "Jobs are assigned while the machine is online."}
            </Empty>
          ) : (
            <table className="table">
              <tbody>
                {jobs.map((j) => (
                  <tr key={j.id}>
                    <td>
                      <div className="font-medium">{j.model_name}</div>
                      {j.detail && <div className="text-[12px] text-muted">{j.detail}</div>}
                    </td>
                    <td>
                      <StateBadge state={j.state} />
                    </td>
                    <td className="text-right text-[13px]">{number(j.requests)} requests</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Panel>
      </div>

      {incidents.length > 0 && (
        <Panel title="Trust incidents" className="mb-6" flush>
          <table className="table">
            <tbody>
              {incidents.map((i, n) => (
                <tr key={n}>
                  <td className="font-medium capitalize">{i.kind.replace("_", " ")}</td>
                  <td>{i.severity}</td>
                  <td className="text-muted">{i.action ?? "recorded"}</td>
                  <td className="text-right text-muted">{dateTime(i.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Panel>
      )}

      <Confirm
        open={confirm}
        title={`Remove ${h.name}?`}
        body="Running jobs move to other machines and the agent's credential is revoked. To add the machine again, create a new install command. Earnings history is kept."
        confirmLabel="Remove machine"
        busy={busy === "remove"}
        onCancel={() => setConfirm(false)}
        onConfirm={async () => {
          setBusy("remove");
          try {
            await api.del(`/v1/hosts/${id}`);
            router.replace("/app/hosts");
          } catch (e) {
            setError(e instanceof ApiError ? e.message : "Could not remove the machine");
            setConfirm(false);
          } finally {
            setBusy("");
          }
        }}
      />
    </>
  );
}
