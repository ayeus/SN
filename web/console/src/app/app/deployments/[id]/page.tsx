"use client";

import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { useEffect, useRef, useState } from "react";
import { Area, AreaChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { api, ApiError } from "@/lib/api";
import { useData } from "@/lib/hooks";
import { compact, money, number, time } from "@/lib/format";
import type { Deployment, DeploymentEvent, Money, Replica } from "@/lib/types";
import { Button, ButtonLink, Confirm, CopyField, Notice, PageHeader, Panel, Skeleton, Stat, StateBadge, TierBadge, cx } from "@/components/ui";
import { PowerRail } from "@/components/PowerRail";
import { Snippets } from "@/components/Snippets";

type Detail = {
  deployment: Deployment;
  replicas: Replica[];
  usage_24h: { requests: number; errors: number; input_tokens: number; output_tokens: number; cost: Money };
  cost_last_hour: Money;
  base_url: string;
  model: string;
};

type Point = { t: string; requests: number; errors: number; p50_ms: number; p95_ms: number; output_tokens: number };

const LIVE = new Set(["pending", "scheduling", "pulling", "loading", "warming", "stopping"]);

function useEvents(id: string, poll: boolean) {
  const [events, setEvents] = useState<DeploymentEvent[]>([]);
  const cursor = useRef(0);
  useEffect(() => {
    cursor.current = 0;
    setEvents([]);
    let stop = false;
    const load = async () => {
      try {
        const r = await api.get<{ events: DeploymentEvent[] }>(`/v1/deployments/${id}/logs?after=${cursor.current}`);
        if (stop || !r.events.length) return;
        cursor.current = r.events[r.events.length - 1].id;
        setEvents((e) => [...e, ...r.events].slice(-300));
      } catch {
        // The panel shows what it has; the next poll retries.
      }
    };
    load();
    const t = window.setInterval(load, poll ? 1500 : 8000);
    return () => {
      stop = true;
      window.clearInterval(t);
    };
  }, [id, poll]);
  return events;
}

export default function DeploymentPage() {
  const { id } = useParams<{ id: string }>();
  const router = useRouter();
  const [live, setLive] = useState(true);
  const detail = useData(() => api.get<Detail>(`/v1/deployments/${id}`), [id], live ? 1500 : 8000);
  const events = useEvents(id, live);
  const [window_, setWindow] = useState<"1h" | "24h" | "7d">("24h");
  const metrics = useData(() => api.get<{ points: Point[] }>(`/v1/deployments/${id}/metrics?window=${window_}`), [id, window_], 30_000);

  const [secret, setSecret] = useState<string | null>(null);
  useEffect(() => {
    const k = `new-key:${id}`;
    const s = sessionStorage.getItem(k);
    if (s) {
      setSecret(s);
      sessionStorage.removeItem(k);
    }
  }, [id]);

  const [confirmStop, setConfirmStop] = useState(false);
  const [busy, setBusy] = useState("");
  const [actionError, setActionError] = useState("");

  const d = detail.data?.deployment;
  useEffect(() => {
    if (d) setLive(LIVE.has(d.state));
  }, [d?.state]); // eslint-disable-line react-hooks/exhaustive-deps

  if (detail.error?.status === 404) {
    return (
      <Notice tone="error">
        This deployment doesn&apos;t exist or belongs to another organisation. <Link href="/app/deployments" className="underline">Back to deployments</Link>
      </Notice>
    );
  }
  if (!detail.data || !d) {
    return (
      <div className="grid gap-4">
        <Skeleton className="h-10 w-72" />
        <Skeleton className="h-28" />
        <Skeleton className="h-64" />
      </div>
    );
  }
  const data = detail.data;
  const liveReplica = data.replicas.find((r) => r.state !== "serving" && r.state !== "stopped" && r.state !== "failed");

  async function act(action: "pause" | "resume" | "retry") {
    setBusy(action);
    setActionError("");
    try {
      await api.patch(`/v1/deployments/${id}`, { action });
      await detail.reload();
      setLive(true);
    } catch (e) {
      setActionError(e instanceof ApiError ? e.message : "Action failed");
    } finally {
      setBusy("");
    }
  }

  async function stop() {
    setBusy("stop");
    try {
      await api.del(`/v1/deployments/${id}`);
      setConfirmStop(false);
      await detail.reload();
      setLive(true);
    } catch (e) {
      setActionError(e instanceof ApiError ? e.message : "Stop failed");
    } finally {
      setBusy("");
    }
  }

  const stoppable = d.desired_state !== "stopped";
  return (
    <>
      <PageHeader
        back={{ href: "/app/deployments", label: "Deployments" }}
        title={d.name}
        description={
          <span className="flex flex-wrap items-center gap-3">
            <StateBadge state={d.state} />
            <span>{d.model_name}</span>
            <TierBadge tier={d.tier} long />
            <span>{d.region}</span>
          </span>
        }
        actions={
          <>
            {(d.state === "serving" || d.state === "degraded") && (
              <ButtonLink href={`/app/playground?deployment=${encodeURIComponent(d.name)}`} variant="secondary">
                Try it
              </ButtonLink>
            )}
            {d.desired_state === "running" && d.state !== "failed" && (
              <Button variant="secondary" loading={busy === "pause"} onClick={() => act("pause")}>
                Pause
              </Button>
            )}
            {d.desired_state === "paused" && (
              <Button loading={busy === "resume"} onClick={() => act("resume")}>
                Resume
              </Button>
            )}
            {d.state === "failed" && d.desired_state === "running" && (
              <Button loading={busy === "retry"} onClick={() => act("retry")}>
                Retry
              </Button>
            )}
            {stoppable && (
              <Button variant="danger" onClick={() => setConfirmStop(true)}>
                Stop
              </Button>
            )}
          </>
        }
      />

      {actionError && <Notice tone="error" className="mb-4">{actionError}</Notice>}

      {secret && (
        <Notice tone="success" className="mb-6">
          <p className="mb-3 font-medium">Copy this API key now. It won&apos;t be shown again.</p>
          <CopyField value={secret} label="API key" secret />
          <p className="mt-2 text-[13px] text-muted">It only works for this deployment. Create more keys under API keys.</p>
        </Notice>
      )}

      <Panel className="mb-6">
        <PowerRail
          state={d.state}
          detail={
            d.state === "failed" || d.state === "pending" || d.state === "degraded"
              ? d.last_error
              : liveReplica?.detail ?? (d.state === "serving" ? "Endpoint is live." : undefined)
          }
        />
        {d.state === "failed" && (
          <p className="mt-3 text-[14px] text-danger">{d.last_error ?? "The deployment failed."} Fix the cause, then Retry.</p>
        )}
        {(d.state === "paused" || d.state === "stopped") && d.last_error && <p className="mt-3 text-[14px] text-muted">{d.last_error}</p>}
      </Panel>

      <div className="mb-6 grid gap-6 lg:grid-cols-[minmax(0,1.3fr)_minmax(0,1fr)]">
        <Panel title="Call it" description="Point any OpenAI client at this base URL and use the deployment name as the model.">
          <div className="grid gap-2">
            <CopyField value={data.base_url} label="Base URL" />
            <CopyField value={data.model} label="Model" />
          </div>
          <div className="mt-5">
            <Snippets baseUrl={data.base_url} model={data.model} apiKey={secret ?? undefined} />
          </div>
        </Panel>

        <Panel title="Usage today">
          <div className="grid grid-cols-2 gap-6">
            <Stat label="Requests" value={number(data.usage_24h.requests)} sub={data.usage_24h.errors ? `${data.usage_24h.errors} failed` : undefined} />
            <Stat label="Tokens" value={compact(data.usage_24h.input_tokens + data.usage_24h.output_tokens)} sub={`${compact(data.usage_24h.output_tokens)} generated`} />
            <Stat label="Cost today" value={money(data.usage_24h.cost)} />
            <Stat label="Cost, last hour" value={money(data.cost_last_hour, { precise: true })} sub="Live rate" />
          </div>
        </Panel>
      </div>

      <Panel
        className="mb-6"
        title="Traffic"
        actions={
          <div className="flex gap-1" role="tablist" aria-label="Time window">
            {(["1h", "24h", "7d"] as const).map((w) => (
              <button
                key={w}
                role="tab"
                aria-selected={window_ === w}
                onClick={() => setWindow(w)}
                className={cx("h-7 rounded-md px-2.5 text-[13px]", window_ === w ? "bg-nil-soft font-medium text-nil" : "text-muted hover:bg-surface-2")}
              >
                {w}
              </button>
            ))}
          </div>
        }
      >
        {metrics.data && metrics.data.points.length > 0 ? (
          <div className="h-[220px]">
            <ResponsiveContainer width="100%" height="100%">
              <AreaChart data={metrics.data.points.map((p) => ({ ...p, label: time(p.t).slice(0, 5) }))} margin={{ left: -16, right: 8, top: 8 }}>
                <CartesianGrid stroke="var(--line)" vertical={false} />
                <XAxis dataKey="label" tick={{ fill: "var(--muted)", fontSize: 12 }} tickLine={false} axisLine={false} />
                <YAxis tick={{ fill: "var(--muted)", fontSize: 12 }} tickLine={false} axisLine={false} allowDecimals={false} />
                <Tooltip
                  contentStyle={{ background: "var(--surface)", border: "1px solid var(--line)", borderRadius: 8, fontSize: 13 }}
                  formatter={(v, name) => (name === "p95_ms" ? [`${Math.round(Number(v))} ms`, "P95 latency"] : [v, "Requests"])}
                />
                <Area type="monotone" dataKey="requests" stroke="var(--nil)" fill="var(--nil-soft)" strokeWidth={2} />
              </AreaChart>
            </ResponsiveContainer>
          </div>
        ) : (
          <p className="py-6 text-muted">No requests in this window yet. Send one from the Playground or with the snippet above.</p>
        )}
      </Panel>

      <div className="grid gap-6 lg:grid-cols-2">
        <Panel title="Replicas" description="Hosts are never identified to customers; you see tier, region and GPU class." flush>
          {data.replicas.length === 0 ? (
            <p className="p-5 text-muted">No replica is placed yet.</p>
          ) : (
            <div className="overflow-x-auto">
              <table className="table">
                <thead>
                  <tr>
                    <th>Status</th>
                    <th>GPU</th>
                    <th>Where</th>
                    <th className="text-right">Requests</th>
                  </tr>
                </thead>
                <tbody>
                  {data.replicas.map((r) => (
                    <tr key={r.id}>
                      <td>
                        <StateBadge state={r.state} />
                        {r.last_error && r.state === "failed" && <div className="mt-1 max-w-[260px] text-[12px] text-danger">{r.last_error}</div>}
                      </td>
                      <td className="text-[13px]">{r.gpu_model ?? "—"}</td>
                      <td>
                        <span className="flex items-center gap-2">
                          <TierBadge tier={r.tier} /> <span className="text-[13px] text-muted">{r.region}</span>
                        </span>
                      </td>
                      <td className="text-right">{number(r.total_requests)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>

        <Panel title="Activity" flush>
          <ol className="max-h-[360px] overflow-y-auto px-5 py-3 text-[13px]" aria-live="polite">
            {events.length === 0 && <li className="py-2 text-muted">No activity yet.</li>}
            {[...events].reverse().map((e) => (
              <li key={e.id} className="flex gap-3 border-b border-line py-2 last:border-0">
                <time className="shrink-0 font-mono text-muted" dateTime={e.created_at}>
                  {time(e.created_at)}
                </time>
                <span className={cx(e.kind === "error" && "text-danger", e.kind === "state" && "font-medium")}>{e.message}</span>
              </li>
            ))}
          </ol>
        </Panel>
      </div>

      <Confirm
        open={confirmStop}
        title={`Stop ${d.name}?`}
        body="Replicas shut down and the endpoint stops answering. Its API keys are revoked. Usage history and invoices are kept."
        confirmLabel="Stop deployment"
        busy={busy === "stop"}
        onConfirm={stop}
        onCancel={() => setConfirmStop(false)}
      />
      {d.state === "stopped" && (
        <p className="mt-6 text-[13px] text-muted">
          This deployment is stopped. <button className="underline" onClick={() => router.push("/app/models")}>Deploy again</button>
        </p>
      )}
    </>
  );
}
