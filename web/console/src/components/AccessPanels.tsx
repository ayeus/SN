"use client";

import { useState } from "react";
import { api, ApiError } from "@/lib/api";
import { useData } from "@/lib/hooks";
import { ago, dateTime } from "@/lib/format";
import { Button, Confirm, CopyField, Field, Input, Notice, Panel, Select, StateBadge } from "@/components/ui";

type Invite = {
  id: string;
  state: "pending" | "used" | "expired" | "revoked";
  workspace: string | null;
  role: string;
  email: string | null;
  note: string | null;
  used_by: string | null;
  expires_at: string;
  created_at: string;
};

type Person = {
  id: string;
  email: string;
  name: string;
  is_operator: boolean;
  disabled_at: string | null;
  workspaces: string;
  machines: number;
  created_at: string;
};

type Handed = { title: string; link: string; note: string };

function until(iso: string): string {
  const s = Math.round((new Date(iso).getTime() - Date.now()) / 1000);
  if (s <= 0) return "expired";
  if (s < 3600) return `${Math.max(1, Math.floor(s / 60))}m left`;
  if (s < 86400) return `${Math.floor(s / 3600)}h left`;
  return `${Math.floor(s / 86400)}d left`;
}

// A link that exists only now: the platform keeps its hash, not the link.
function HandedLink({ handed, onDone }: { handed: Handed; onDone: () => void }) {
  return (
    <Notice tone="success" className="mb-5">
      <div className="mb-2 font-medium">{handed.title}</div>
      <CopyField value={handed.link} label="Link" />
      <p className="mt-2 text-[13px] text-muted">{handed.note}</p>
      <Button size="sm" variant="secondary" className="mt-3" onClick={onDone}>
        Done
      </Button>
    </Notice>
  );
}

// Invitations: the only way an account is created on a private network.
export function InvitesPanel({ myWorkspace }: { myWorkspace: string }) {
  const invites = useData(() => api.get<{ invites: Invite[] }>("/v1/admin/invites"), [], 30_000);
  const [form, setForm] = useState({ workspace: "new", email: "", note: "" });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [emailError, setEmailError] = useState("");
  const [handed, setHanded] = useState<Handed | null>(null);

  async function create(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    setEmailError("");
    try {
      const r = await api.post<{ link: string; expires_at: string }>("/v1/admin/invites", {
        workspace: form.workspace,
        email: form.email.trim(),
        note: form.note.trim(),
      });
      setHanded({
        title: form.note.trim() ? `Invitation for ${form.note.trim()}` : "Invitation created",
        link: r.link,
        note: `Send it to them yourself. It works once and expires ${dateTime(r.expires_at)}. It is not shown again.`,
      });
      setForm((f) => ({ ...f, email: "", note: "" }));
      await invites.reload();
    } catch (err) {
      if (err instanceof ApiError && err.fields.email) setEmailError(err.fields.email);
      else setError(err instanceof ApiError ? err.message : "Could not create the invitation");
    } finally {
      setBusy(false);
    }
  }

  async function revoke(id: string) {
    setError("");
    try {
      await api.del(`/v1/admin/invites/${id}`);
      await invites.reload();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not withdraw the invitation");
    }
  }

  const rows = invites.data?.invites ?? [];
  return (
    <Panel
      title="Invitations"
      description="Accounts are created from invitation links. Each link works once and lasts three days."
      className="mb-6"
    >
      {handed && <HandedLink handed={handed} onDone={() => setHanded(null)} />}
      {error && (
        <Notice tone="error" className="mb-5">
          {error}
        </Notice>
      )}
      <form onSubmit={create} className="grid items-start gap-4 md:grid-cols-[1.3fr_1fr_1fr_auto]">
        <Field label="They get" htmlFor="invite-workspace">
          <Select id="invite-workspace" value={form.workspace} onChange={(e) => setForm((f) => ({ ...f, workspace: e.target.value }))}>
            <option value="new">A workspace of their own</option>
            <option value="mine">A seat in {myWorkspace} (shared deployments and keys)</option>
          </Select>
        </Field>
        <Field label="Who it is for" htmlFor="invite-note" hint="A reminder for you. Optional.">
          <Input id="invite-note" maxLength={200} placeholder="Priya" value={form.note} onChange={(e) => setForm((f) => ({ ...f, note: e.target.value }))} />
        </Field>
        <Field label="Their email" htmlFor="invite-email" error={emailError} hint="Optional. Only this address can use the link.">
          <Input
            id="invite-email"
            type="email"
            value={form.email}
            onChange={(e) => setForm((f) => ({ ...f, email: e.target.value }))}
            aria-invalid={!!emailError}
          />
        </Field>
        <Button type="submit" loading={busy} className="md:mt-[26px]">
          Create link
        </Button>
      </form>

      {rows.length > 0 && (
        <div className="-mx-5 -mb-5 mt-6 overflow-x-auto border-t border-line">
          <table className="table min-w-[720px]">
            <thead>
              <tr>
                <th>For</th>
                <th>Gets</th>
                <th>Status</th>
                <th className="text-right">Created</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {rows.map((i) => (
                <tr key={i.id}>
                  <td>
                    <div className="font-medium">{i.note ?? i.email ?? "Anyone with the link"}</div>
                    {i.note && i.email && <div className="text-[13px] text-muted">{i.email}</div>}
                  </td>
                  <td className="text-[13px]">{i.workspace ? `A seat in ${i.workspace}` : "Their own workspace"}</td>
                  <td className="text-[13px]">
                    {i.state === "pending" && <span>Waiting, {until(i.expires_at)}</span>}
                    {i.state === "used" && <span className="text-muted">Used by {i.used_by ?? "a deleted account"}</span>}
                    {i.state === "expired" && <span className="text-muted">Expired unused</span>}
                    {i.state === "revoked" && <span className="text-muted">Withdrawn</span>}
                  </td>
                  <td className="text-right text-[13px] text-muted">{ago(i.created_at)}</td>
                  <td className="text-right">
                    {i.state === "pending" && (
                      <Button size="sm" variant="secondary" onClick={() => revoke(i.id)}>
                        Withdraw
                      </Button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Panel>
  );
}

// Everyone with an account, and the two things an operator does for them.
export function PeoplePanel({ myEmail }: { myEmail: string }) {
  const people = useData(() => api.get<{ people: Person[] }>("/v1/admin/people"), [], 30_000);
  const [error, setError] = useState("");
  const [handed, setHanded] = useState<Handed | null>(null);
  const [disable, setDisable] = useState<Person | null>(null);

  async function act(fn: () => Promise<void>) {
    setError("");
    try {
      await fn();
      await people.reload();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Action failed");
    }
  }

  const rows = people.data?.people ?? [];
  return (
    <>
      <Panel title={`People: ${rows.length}`} description="Everyone who can sign in to this network." flush className="mb-6">
        {(handed || error) && (
          <div className="px-5 pt-5">
            {handed && <HandedLink handed={handed} onDone={() => setHanded(null)} />}
            {error && (
              <Notice tone="error" className="mb-5">
                {error}
              </Notice>
            )}
          </div>
        )}
        <div className="overflow-x-auto">
          <table className="table min-w-[820px]">
            <thead>
              <tr>
                <th>Person</th>
                <th>Workspace</th>
                <th className="text-right">Machines</th>
                <th className="text-right">Joined</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {rows.map((p) => (
                <tr key={p.id}>
                  <td>
                    <div className="flex flex-wrap items-center gap-2 font-medium">
                      {p.name}
                      {p.is_operator && <StateBadge state="serving" label="Operator" />}
                      {p.disabled_at && <StateBadge state="failed" label="Disabled" />}
                    </div>
                    <div className="text-[13px] text-muted">{p.email}</div>
                  </td>
                  <td className="text-[13px]">{p.workspaces || "-"}</td>
                  <td className="text-right">{p.machines}</td>
                  <td className="text-right text-[13px] text-muted">{ago(p.created_at)}</td>
                  <td className="text-right">
                    <div className="flex justify-end gap-1">
                      <Button
                        size="sm"
                        variant="secondary"
                        onClick={() =>
                          act(async () => {
                            const r = await api.post<{ link: string; expires_at: string }>(`/v1/admin/people/${p.id}/reset-link`);
                            setHanded({
                              title: `Password reset link for ${p.name}`,
                              link: r.link,
                              note: `Send it to them yourself. It works once and expires ${dateTime(r.expires_at)}. It is not shown again.`,
                            });
                          })
                        }
                      >
                        Reset link
                      </Button>
                      {p.disabled_at ? (
                        <Button size="sm" variant="secondary" onClick={() => act(() => api.post(`/v1/admin/people/${p.id}/enable`).then(() => undefined))}>
                          Enable
                        </Button>
                      ) : (
                        p.email !== myEmail && (
                          <Button size="sm" variant="danger" onClick={() => setDisable(p)}>
                            Disable
                          </Button>
                        )
                      )}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Panel>

      <Confirm
        open={!!disable}
        title={`Disable ${disable?.name}?`}
        body={
          <>
            They are signed out now and cannot sign in again until you enable the account. If nobody else can sign in to their
            workspace, its API keys stop working for good. Their machines are not touched: drain or ban those in the fleet list
            below.
          </>
        }
        confirmLabel="Disable account"
        onCancel={() => setDisable(null)}
        onConfirm={() => {
          const p = disable;
          setDisable(null);
          if (p) act(() => api.post(`/v1/admin/people/${p.id}/disable`).then(() => undefined));
        }}
      />
    </>
  );
}
