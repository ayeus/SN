"use client";

import { useState } from "react";
import { api, ApiError } from "@/lib/api";
import { useData } from "@/lib/hooks";
import { ago } from "@/lib/format";
import type { ApiKey, Deployment } from "@/lib/types";
import { Button, Confirm, CopyField, Empty, Field, Input, Notice, PageHeader, Panel, Select, Skeleton } from "@/components/ui";

export default function KeysPage() {
  const keys = useData(() => api.get<{ api_keys: ApiKey[] }>("/v1/api-keys"), []);
  const deps = useData(() => api.get<{ deployments: Deployment[] }>("/v1/deployments"), []);
  const depName = (id?: string) => deps.data?.deployments.find((d) => d.id === id)?.name ?? "a stopped deployment";

  const [name, setName] = useState("");
  const [scope, setScope] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [created, setCreated] = useState<{ name: string; secret: string } | null>(null);
  const [revoking, setRevoking] = useState<ApiKey | null>(null);
  const [revokeBusy, setRevokeBusy] = useState(false);

  return (
    <>
      <PageHeader title="API keys" description="Keys authenticate calls to your deployments. A key scoped to one deployment can call nothing else." />

      {created && (
        <Notice tone="success" className="mb-6">
          <p className="mb-3 font-medium">Copy the key “{created.name}” now. It won&apos;t be shown again.</p>
          <CopyField value={created.secret} label="API key" secret />
        </Notice>
      )}

      <Panel title="Create a key" className="mb-6">
        <form
          className="grid items-end gap-4 md:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto]"
          onSubmit={async (e) => {
            e.preventDefault();
            setBusy(true);
            setError("");
            try {
              const r = await api.post<{ secret: string }>(
                "/v1/api-keys",
                { name: name.trim(), deployment_id: scope || undefined },
                { idempotent: true },
              );
              setCreated({ name: name.trim(), secret: r.secret });
              setName("");
              await keys.reload();
            } catch (err) {
              setError(err instanceof ApiError ? err.message : "Could not create key");
            } finally {
              setBusy(false);
            }
          }}
        >
          <Field label="Name" htmlFor="key-name" hint="Where it's used, e.g. production-backend">
            <Input id="key-name" required maxLength={100} value={name} onChange={(e) => setName(e.target.value)} />
          </Field>
          <Field label="Can call" htmlFor="key-scope">
            <Select id="key-scope" value={scope} onChange={(e) => setScope(e.target.value)}>
              <option value="">All deployments</option>
              {(deps.data?.deployments ?? []).map((d) => (
                <option key={d.id} value={d.id}>
                  Only {d.name}
                </option>
              ))}
            </Select>
          </Field>
          <Button type="submit" loading={busy} disabled={!name.trim()}>
            Create key
          </Button>
        </form>
        {error && <Notice tone="error" className="mt-4">{error}</Notice>}
      </Panel>

      <Panel title="Active keys" flush>
        {!keys.data ? (
          <div className="p-5">
            <Skeleton className="h-10" />
          </div>
        ) : keys.data.api_keys.length === 0 ? (
          <Empty title="No keys yet">Each deployment comes with its own key, and you can create more here.</Empty>
        ) : (
          <div className="overflow-x-auto">
            <table className="table min-w-[640px]">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Key</th>
                  <th>Can call</th>
                  <th>Last used</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {keys.data.api_keys.map((k) => (
                  <tr key={k.id}>
                    <td className="font-medium">{k.name}</td>
                    <td className="font-mono text-[13px] text-muted">{k.prefix}…</td>
                    <td>{k.scope === "deployment" ? `Only ${depName(k.deployment_id)}` : "All deployments"}</td>
                    <td className="text-muted">{ago(k.last_used_at)}</td>
                    <td className="text-right">
                      <Button variant="danger" size="sm" onClick={() => setRevoking(k)}>
                        Revoke
                      </Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>

      <Confirm
        open={!!revoking}
        title={`Revoke “${revoking?.name}”?`}
        body="Requests using this key start failing immediately. This can't be undone."
        confirmLabel="Revoke key"
        busy={revokeBusy}
        onCancel={() => setRevoking(null)}
        onConfirm={async () => {
          if (!revoking) return;
          setRevokeBusy(true);
          try {
            await api.del(`/v1/api-keys/${revoking.id}`);
            await keys.reload();
            setRevoking(null);
          } catch (err) {
            setError(err instanceof ApiError ? err.message : "Could not revoke key");
            setRevoking(null);
          } finally {
            setRevokeBusy(false);
          }
        }}
      />
    </>
  );
}
