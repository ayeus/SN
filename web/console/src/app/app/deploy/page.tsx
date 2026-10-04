"use client";

import { useRouter, useSearchParams } from "next/navigation";
import { Suspense, useEffect, useMemo, useState } from "react";
import { api, ApiError } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { useData } from "@/lib/hooks";
import { TIERS } from "@/lib/format";
import type { Deployment, Model, NetworkStats, Region, Tier } from "@/lib/types";
import { Button, Field, Input, Notice, PageHeader, Panel, Select, TierBadge, cx } from "@/components/ui";

type Capacity = { region: string; online_gpus: number; tiers: { tier: Tier; allowed: boolean; eligible_hosts: number; message?: string }[] };

const STEPS = ["Compute", "Model", "Configure", "Review"] as const;

const slug = (s: string) => s.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "").slice(0, 60);

function Wizard() {
  const router = useRouter();
  const params = useSearchParams();
  const { me } = useAuth();

  const models = useData(() => api.get<{ models: Model[] }>("/v1/models"), []);
  const regions = useData(() => api.get<{ regions: Region[] }>("/v1/regions", false), []);
  const network = useData(() => api.get<NetworkStats>("/v1/network/stats", false), [], 10_000);

  const [step, setStep] = useState(0);
  const [modelId, setModelId] = useState(params.get("model") ?? "");
  const [tier, setTier] = useState<Tier>("t3");
  const [region, setRegion] = useState(me?.organization.default_region ?? "IN-SOUTH");
  const [replicas, setReplicas] = useState(1);
  const [residentIN, setResidentIN] = useState(true);
  const [name, setName] = useState("");
  const [nameTouched, setNameTouched] = useState(false);
  const [fields, setFields] = useState<Record<string, string>>({});
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const model = models.data?.models.find((m) => m.id === modelId);
  useEffect(() => {
    if (model && !nameTouched) setName(slug(model.name.replace(/-instruct.*$/, "")));
  }, [model, nameTouched]);

  const capacity = useData<Capacity>(
    modelId ? () => api.get(`/v1/capacity?model_id=${modelId}&region=${region}`) : null,
    [modelId, region],
    10_000,
  );
  const cap = capacity.data?.tiers.find((t) => t.tier === tier);

  const tierStats = useMemo(
    () =>
      (["t1", "t2", "t3"] as Tier[]).map((t) => ({
        tier: t,
        online: network.data?.gpus_by_tier?.[t] ?? 0,
        free: network.data?.gpus_free_by_tier?.[t] ?? 0,
      })),
    [network.data],
  );

  const canNext =
    (step === 0 && !!tier && !!region) ||
    (step === 1 && !!model && model.tiers_allowed.includes(tier)) ||
    (step === 2 && /^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$/.test(name)) ||
    step === 3;

  async function deploy() {
    if (!model) return;
    setBusy(true);
    setError("");
    setFields({});
    try {
      const r = await api.post<{ deployment: Deployment; secret: string }>(
        "/v1/deployments",
        { model_id: model.id, name, tier, region, min_replicas: replicas, max_replicas: replicas, resident_in: residentIN },
        { idempotent: true },
      );
      // The key is shown once, on the next page, and never travels in a URL.
      sessionStorage.setItem(`new-key:${r.deployment.id}`, r.secret);
      router.push(`/app/deployments/${r.deployment.id}`);
    } catch (e) {
      if (e instanceof ApiError) {
        setFields(e.fields);
        setError(e.message);
        if (e.fields.name) setStep(2);
        else if (e.fields.tier || e.fields.region) setStep(0);
      } else setError("Deployment failed");
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <PageHeader title="Deploy a model" back={{ href: "/app/models", label: "Models" }} />

      <ol className="mb-6 grid grid-cols-4 gap-2" aria-label="Steps">
        {STEPS.map((s, i) => (
          <li key={s}>
            <button
              type="button"
              disabled={i > step}
              onClick={() => setStep(i)}
              aria-current={i === step ? "step" : undefined}
              className="w-full text-left disabled:cursor-default"
            >
              <div className={cx("h-1 rounded-full", i < step ? "bg-nil" : i === step ? "bg-marigold" : "bg-surface-2")} />
              <div className={cx("mt-2 text-[13px] font-medium", i <= step ? "text-ink" : "text-muted")}>
                {i + 1}. {s}
              </div>
            </button>
          </li>
        ))}
      </ol>

      {error && !Object.keys(fields).length && (
        <Notice tone="error" className="mb-4">
          {error}
        </Notice>
      )}

      {step === 0 && (
        <Panel title="Choose where it runs" description="Higher tiers are more reliable; spot runs on personal machines and can be interrupted.">
          <fieldset className="grid gap-3 md:grid-cols-3">
            <legend className="sr-only">Tier</legend>
            {tierStats.map((t) => {
              const c = capacity.data?.tiers.find((x) => x.tier === t.tier);
              const blocked = model ? !model.tiers_allowed.includes(t.tier) : false;
              return (
                <label
                  key={t.tier}
                  className={cx(
                    "flex cursor-pointer flex-col gap-2 rounded-xl border p-4",
                    tier === t.tier ? "border-nil ring-2 ring-nil/20" : "border-line hover:border-muted/50",
                    blocked && "cursor-not-allowed opacity-50",
                  )}
                >
                  <input type="radio" name="tier" className="sr-only" disabled={blocked} checked={tier === t.tier} onChange={() => setTier(t.tier)} />
                  <div className="flex items-center justify-between">
                    <span className="font-semibold">
                      {TIERS[t.tier].short} {TIERS[t.tier].name}
                    </span>
                  </div>
                  <span className="text-[13px] text-muted">{TIERS[t.tier].who}</span>
                  <span className="text-[13px]">{TIERS[t.tier].sla}</span>
                  <span className="text-[13px] text-muted">
                    {blocked
                      ? "Not available for this model"
                      : c
                        ? `${c.eligible_hosts} ${c.eligible_hosts === 1 ? "host" : "hosts"} can run ${model?.name ?? "it"} now`
                        : `${t.free} of ${t.online} GPUs free`}
                  </span>
                </label>
              );
            })}
          </fieldset>

          <div className="mt-6 grid gap-4 md:grid-cols-3">
            <Field label="Region" htmlFor="region" error={fields.region}>
              <Select id="region" value={region} onChange={(e) => setRegion(e.target.value)}>
                {(regions.data?.regions ?? [{ code: region, name: region, country: "IN" }]).map((r) => (
                  <option key={r.code} value={r.code}>
                    {r.name}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="Replicas" htmlFor="replicas" hint="Each replica runs on its own host. Two or more survive a host failure.">
              <Input id="replicas" type="number" min={1} max={16} value={replicas} onChange={(e) => setReplicas(Math.min(16, Math.max(1, +e.target.value || 1)))} />
            </Field>
            <Field label="Data residency" htmlFor="resident">
              <label className="flex h-10 items-center gap-2 text-[14px]">
                <input id="resident" type="checkbox" checked={residentIN} onChange={(e) => setResidentIN(e.target.checked)} className="h-4 w-4 accent-[var(--nil)]" />
                Keep this deployment in India
              </label>
            </Field>
          </div>
          {cap && cap.eligible_hosts === 0 && cap.message && (
            <Notice tone="warn" className="mt-5">
              {cap.message} You can still deploy; it starts as soon as a matching host comes online.
            </Notice>
          )}
        </Panel>
      )}

      {step === 1 && (
        <Panel title="Choose a model" flush>
          <ul className="divide-y divide-line">
            {(models.data?.models ?? []).map((m) => {
              const allowed = m.tiers_allowed.includes(tier);
              return (
                <li key={m.id}>
                  <label className={cx("flex cursor-pointer items-center gap-4 px-5 py-4", !allowed && "cursor-not-allowed opacity-50", modelId === m.id && "bg-nil-soft")}>
                    <input type="radio" name="model" disabled={!allowed} checked={modelId === m.id} onChange={() => setModelId(m.id)} className="h-4 w-4 accent-[var(--nil)]" />
                    <div className="min-w-0 flex-1">
                      <div className="font-medium">{m.name}</div>
                      <div className="text-[13px] text-muted">
                        {allowed ? `${m.params_b}B parameters, ${m.license}` : `Not available on ${TIERS[tier].short}; needs ${m.tiers_allowed.map((t) => TIERS[t].short).join(" or ")}`}
                      </div>
                    </div>
                    <div className="text-right text-[13px] text-muted">{m.min_vram_gb} GB GPU</div>
                  </label>
                </li>
              );
            })}
          </ul>
        </Panel>
      )}

      {step === 2 && (
        <Panel title="Name your deployment" description="You pass this name as the model when you call the API.">
          <div className="max-w-[420px]">
            <Field label="Deployment name" htmlFor="name" error={fields.name} hint="Lowercase letters, digits and dashes.">
              <Input
                id="name"
                value={name}
                onChange={(e) => {
                  setNameTouched(true);
                  setName(slug(e.target.value));
                }}
                aria-invalid={!!fields.name}
              />
            </Field>
          </div>
        </Panel>
      )}

      {step === 3 && model && (
        <Panel title="Review">
          <dl className="grid gap-x-8 gap-y-3 text-[14px] sm:grid-cols-[180px_1fr]">
            <dt className="text-muted">Model</dt>
            <dd className="font-medium">{model.name}</dd>
            <dt className="text-muted">Deployment name</dt>
            <dd className="font-mono">{name}</dd>
            <dt className="text-muted">Runs on</dt>
            <dd>
              <TierBadge tier={tier} long /> in {region}, {replicas} {replicas === 1 ? "replica" : "replicas"}
              {residentIN ? ", India-resident" : ""}
            </dd>
            <dt className="text-muted">Needs</dt>
            <dd>A GPU with at least {model.min_vram_gb} GB of memory</dd>
          </dl>
          <p className="mt-6 max-w-[64ch] text-[13px] text-muted">
            You get an OpenAI-compatible endpoint and a one-time API key as soon as the deployment is created.
          </p>
        </Panel>
      )}

      <div className="mt-6 flex justify-between">
        <Button variant="ghost" onClick={() => setStep((s) => Math.max(0, s - 1))} disabled={step === 0}>
          Back
        </Button>
        {step < 3 ? (
          <Button onClick={() => setStep((s) => s + 1)} disabled={!canNext}>
            Continue
          </Button>
        ) : (
          <Button onClick={deploy} loading={busy}>
            Deploy {name}
          </Button>
        )}
      </div>
    </>
  );
}

export default function DeployPage() {
  return (
    <Suspense>
      <Wizard />
    </Suspense>
  );
}
