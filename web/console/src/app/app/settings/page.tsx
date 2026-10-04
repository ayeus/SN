"use client";

import { useRouter } from "next/navigation";
import { useEffect, useState } from "react";
import { api, ApiError, setSession } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { useData } from "@/lib/hooks";
import type { Region } from "@/lib/types";
import { Button, Confirm, Field, Input, Notice, PageHeader, Panel, Select } from "@/components/ui";

export default function SettingsPage() {
  const { me, reload } = useAuth();
  const router = useRouter();
  const regions = useData(() => api.get<{ regions: Region[] }>("/v1/regions", false), []);
  const [name, setName] = useState("");
  const [region, setRegion] = useState("");
  const [msg, setMsg] = useState<{ tone: "success" | "error"; text: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const [confirm, setConfirm] = useState(false);
  const isAdmin = me?.role === "admin";

  useEffect(() => {
    if (me) {
      setName(me.organization.name);
      setRegion(me.organization.default_region);
    }
  }, [me]);

  if (!me) return null;

  return (
    <>
      <PageHeader title="Settings" />
      {msg && <Notice tone={msg.tone} className="mb-6">{msg.text}</Notice>}

      <Panel title="Organisation" className="mb-6" description={isAdmin ? undefined : "Only organisation admins can change these."}>
        <form
          className="grid max-w-[560px] gap-4"
          onSubmit={async (e) => {
            e.preventDefault();
            setBusy(true);
            setMsg(null);
            try {
              await api.patch(`/v1/orgs/${me.organization.id}`, { name, default_region: region });
              await reload();
              setMsg({ tone: "success", text: "Saved." });
            } catch (err) {
              setMsg({ tone: "error", text: err instanceof ApiError ? err.message : "Could not save" });
            } finally {
              setBusy(false);
            }
          }}
        >
          <Field label="Name" htmlFor="org-name">
            <Input id="org-name" value={name} onChange={(e) => setName(e.target.value)} disabled={!isAdmin} />
          </Field>
          <Field label="Default region" htmlFor="org-region" hint="Used when a deployment doesn't specify one.">
            <Select id="org-region" value={region} onChange={(e) => setRegion(e.target.value)} disabled={!isAdmin}>
              {(regions.data?.regions ?? []).map((r) => (
                <option key={r.code} value={r.code}>
                  {r.name}
                </option>
              ))}
            </Select>
          </Field>
          {isAdmin && (
            <Button type="submit" loading={busy} className="justify-self-start">
              Save changes
            </Button>
          )}
        </form>
      </Panel>

      <Panel title="Account">
        <dl className="grid max-w-[560px] grid-cols-[140px_1fr] gap-y-2 text-[14px]">
          <dt className="text-muted">Name</dt>
          <dd>{me.user.name}</dd>
          <dt className="text-muted">Email</dt>
          <dd>{me.user.email}</dd>
          <dt className="text-muted">Role</dt>
          <dd className="capitalize">{me.role}</dd>
        </dl>
        <div className="mt-6">
          <Button variant="danger" onClick={() => setConfirm(true)}>
            Sign out everywhere
          </Button>
          <p className="mt-2 text-[13px] text-muted">Ends every session on every device, including this one. API keys keep working.</p>
        </div>
      </Panel>

      <Confirm
        open={confirm}
        title="Sign out everywhere?"
        body="You'll need to sign in again on every device."
        confirmLabel="Sign out everywhere"
        onCancel={() => setConfirm(false)}
        onConfirm={async () => {
          try {
            await api.post("/v1/auth/logout-all");
          } finally {
            setSession(null);
            router.replace("/login");
          }
        }}
      />
    </>
  );
}
