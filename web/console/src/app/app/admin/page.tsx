"use client";

import { useState } from "react";
import { api, ApiError } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { useData } from "@/lib/hooks";
import { ago, dateTime } from "@/lib/format";
import type { Host, Tier } from "@/lib/types";
import { Button, Confirm, Field, Input, Notice, PageHeader, Panel, Select, StateBadge, TierBadge } from "@/components/ui";

type FleetRow = { host: Host; online: boolean; owner_email?: string; gpus: number; active_jobs: number };
type Incident = { id: string; host_name: string; kind: string; severity: string; action?: string; resolved: boolean; created_at: string };

export default function AdminPage() {
  const { me } = useAuth();
  const fleet = useData(me?.is_platform_admin ? () => api.get<{ hosts: FleetRow[] }>("/v1/admin/fleet") : null, [me?.is_platform_admin], 10_000);
  const incidents = useData(me?.is_platform_admin ? () => api.get<{ incidents: Incident[] }>("/v1/admin/incidents") : null, [me?.is_platform_admin], 30_000);
  const [msg, setMsg] = useState<{ tone: "success" | "error"; text: string } | null>(null);
  const [ban, setBan] = useState<FleetRow | null>(null);
  const [credit, setCredit] = useState({ org: "", amount: "", note: "" });

  if (!me?.is_platform_admin) return <Notice tone="error">This area is for platform operators.</Notice>;

  async function run(fn: () => Promise<unknown>, ok: string) {
    setMsg(null);
    try {
      await fn();
      setMsg({ tone: "success", text: ok });
      await fleet.reload();
    } catch (e) {
      setMsg({ tone: "error", text: e instanceof ApiError ? e.message : "Action failed" });
    }
  }

  const rows = fleet.data?.hosts ?? [];
  return (
    <>
      <PageHeader title="Operations" description="Every host on the network. Actions here are written to the audit log." />
      {msg && <Notice tone={msg.tone} className="mb-6">{msg.text}</Notice>}

      <Panel title={`Fleet: ${rows.filter((r) => r.online).length} of ${rows.length} online`} flush className="mb-6">
        <div className="overflow-x-auto">
          <table className="table min-w-[980px]">
            <thead>
              <tr>
                <th>Host</th>
                <th>Owner</th>
                <th>Status</th>
                <th>Tier</th>
                <th className="text-right">Rep.</th>
                <th className="text-right">Jobs</th>
                <th className="text-right">Seen</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.host.id}>
                  <td>
                    <div className="font-medium">{r.host.name}</div>
                    <div className="font-mono text-[12px] text-muted">{r.host.id.slice(0, 8)}</div>
                  </td>
                  <td className="text-[13px]">{r.owner_email ?? "-"}</td>
                  <td>{r.online ? <StateBadge state={r.host.status} /> : <StateBadge state="offline" />}</td>
                  <td>
                    <Select
                      aria-label="Tier"
                      className="h-8 w-[76px] text-[13px]"
                      value={r.host.tier}
                      onChange={(e) =>
                        run(() => api.post(`/v1/admin/hosts/${r.host.id}/tier`, { tier: e.target.value as Tier }), `Moved ${r.host.name} to ${e.target.value.toUpperCase()}.`)
                      }
                    >
                      <option value="t1">T1</option>
                      <option value="t2">T2</option>
                      <option value="t3">T3</option>
                    </Select>
                  </td>
                  <td className="text-right">{r.host.reputation}</td>
                  <td className="text-right">{r.active_jobs}</td>
                  <td className="text-right text-muted">{r.online ? "now" : ago(r.host.last_heartbeat_at)}</td>
                  <td className="text-right">
                    <div className="flex justify-end gap-1">
                      <Button size="sm" variant="secondary" onClick={() => run(() => api.post(`/v1/admin/hosts/${r.host.id}/drain`), `Draining ${r.host.name}.`)}>
                        Drain
                      </Button>
                      {r.host.status !== "banned" && (
                        <Button size="sm" variant="danger" onClick={() => setBan(r)}>
                          Ban
                        </Button>
                      )}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Panel>

      <div className="grid gap-6 lg:grid-cols-2">
        <Panel title="Trust incidents" flush>
          {(incidents.data?.incidents ?? []).length === 0 ? (
            <p className="p-5 text-muted">No incidents.</p>
          ) : (
            <table className="table">
              <tbody>
                {incidents.data!.incidents.map((i) => (
                  <tr key={i.id}>
                    <td className="font-medium">{i.host_name}</td>
                    <td>
                      {i.kind} ({i.severity})
                    </td>
                    <td className="text-muted">{i.action ?? "open"}</td>
                    <td className="text-right text-[13px] text-muted">{dateTime(i.created_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Panel>

        <Panel title="Manual credit" description="Credits or refunds an organisation's wallet in its own currency.">
          <form
            className="grid gap-3"
            onSubmit={(e) => {
              e.preventDefault();
              run(() => api.post(`/v1/admin/orgs/${credit.org.trim()}/credit`, { amount: credit.amount, note: credit.note }, { idempotent: true }), "Credit issued.");
            }}
          >
            <Field label="Organisation ID" htmlFor="c-org">
              <Input id="c-org" value={credit.org} onChange={(e) => setCredit({ ...credit, org: e.target.value })} required />
            </Field>
            <Field label="Amount" htmlFor="c-amt">
              <Input id="c-amt" type="number" min="0.01" step="0.01" value={credit.amount} onChange={(e) => setCredit({ ...credit, amount: e.target.value })} required />
            </Field>
            <Field label="Reason" htmlFor="c-note">
              <Input id="c-note" value={credit.note} onChange={(e) => setCredit({ ...credit, note: e.target.value })} />
            </Field>
            <Button type="submit" className="justify-self-start">
              Issue credit
            </Button>
          </form>
        </Panel>
      </div>

      <Confirm
        open={!!ban}
        title={`Ban ${ban?.host.name}?`}
        body={
          <>
            The host&apos;s credential is revoked, its jobs are failed over to other hosts, and this hardware can&apos;t re-enrol. <TierBadge tier={ban?.host.tier ?? "t3"} />
          </>
        }
        confirmLabel="Ban host"
        onCancel={() => setBan(null)}
        onConfirm={() => {
          const r = ban;
          setBan(null);
          if (r) run(() => api.post(`/v1/admin/hosts/${r.host.id}/ban`, { reason: "banned from operations console" }), `${r.host.name} banned.`);
        }}
      />
    </>
  );
}
