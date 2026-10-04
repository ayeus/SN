"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { api, ApiError } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { useData } from "@/lib/hooks";
import { TIERS } from "@/lib/format";
import type { HostSummary, Region, Tier } from "@/lib/types";
import { Button, ButtonLink, CodeBlock, Field, Notice, PageHeader, Panel, Select, StateBadge, TierBadge, cx } from "@/components/ui";

type Issued = {
  registration_token: string;
  token_id: string;
  expires_at: string;
  tier: Tier;
  region: string;
  server_url: string;
  coordinator_url: string;
  commands: { linux_macos: string; windows: string; windows_wsl: string; from_source: string };
};

const TIER_HELP: Record<Tier, string> = {
  t3: "A personal laptop or desktop. Serves interruptible work on public models. You can pause it whenever you need the machine.",
  t2: "A college lab machine or dedicated workstation with at least a 16 GB GPU. Starts with a 7-day probation, then takes production work.",
  t1: "A data-centre node. Onboarded with our team: site audit, contract and network setup.",
};

const WHERE_TO_RUN: Record<keyof Issued["commands"], string> = {
  linux_macos: "Paste it into a terminal on the machine with the GPU.",
  windows: "Paste it into PowerShell on the machine with the GPU. It runs natively; no WSL needed.",
  windows_wsl: "Paste it into PowerShell. It runs the Linux agent inside WSL2, where Ollama must be installed too.",
  from_source: "Run it from a checkout of this repository on the machine with the GPU. Needs Rust.",
};

/** The host an install command points at, when it is only reachable from this machine. */
function loopbackOnly(url: string): boolean {
  try {
    const h = new URL(url).hostname;
    return h === "localhost" || h === "127.0.0.1" || h === "[::1]";
  } catch {
    return false;
  }
}

export default function AddMachinePage() {
  const { me } = useAuth();
  const regions = useData(() => api.get<{ regions: Region[] }>("/v1/regions", false), []);
  const [tier, setTier] = useState<Tier>("t3");
  const [region, setRegion] = useState("IN-SOUTH");
  const [issued, setIssued] = useState<Issued | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [os, setOs] = useState<keyof Issued["commands"]>("linux_macos");

  // The server knows exactly which machine redeemed this install command, so
  // the page asks it rather than guessing from timestamps.
  const tokenStatus = useData<{ used: boolean; host_id: string | null; expired: boolean }>(
    issued ? () => api.get(`/v1/host-tokens/${issued.token_id}`) : null,
    [issued?.token_id],
    2500,
  );
  const hostId = tokenStatus.data?.host_id ?? null;
  const hosts = useData<{ hosts: HostSummary[] }>(hostId ? () => api.get("/v1/hosts") : null, [hostId], 3000);
  const joined = hosts.data?.hosts.find((h) => h.id === hostId);

  useEffect(() => {
    if (navigator.userAgent.includes("Windows")) setOs("windows");
  }, []);

  async function generate() {
    setBusy(true);
    setError("");
    try {
      const r = await api.post<Issued>("/v1/hosts/register-token", { tier, region }, { idempotent: true });
      setIssued(r);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Could not create an install command");
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <PageHeader
        back={{ href: "/app/hosts", label: "Machines" }}
        title="Add a machine"
        description="Install the agent on the machine with the GPU. It connects out to us, so no ports need opening on your network."
      />

      {!issued ? (
        <Panel title="What kind of machine is it?" className="mb-6">
          <fieldset className="grid gap-3 md:grid-cols-3">
            <legend className="sr-only">Tier</legend>
            {(["t3", "t2", "t1"] as Tier[]).map((t) => {
              const locked = t === "t1" && !me?.is_platform_admin;
              return (
                <label
                  key={t}
                  className={cx(
                    "flex cursor-pointer flex-col gap-2 rounded-xl border p-4",
                    tier === t ? "border-nil ring-2 ring-nil/20" : "border-line hover:border-muted/50",
                    locked && "cursor-not-allowed opacity-60",
                  )}
                >
                  <input type="radio" name="tier" className="sr-only" disabled={locked} checked={tier === t} onChange={() => setTier(t)} />
                  <span className="font-semibold">
                    {TIERS[t].short} {TIERS[t].name}
                  </span>
                  <span className="text-[13px] text-muted">{TIER_HELP[t]}</span>
                  {locked && <span className="text-[13px] font-medium text-nil">Contact us to onboard a data centre</span>}
                </label>
              );
            })}
          </fieldset>
          <div className="mt-5 grid max-w-[640px] gap-4 sm:grid-cols-2">
            <Field label="Region" htmlFor="region" hint="Where the machine physically is.">
              <Select id="region" value={region} onChange={(e) => setRegion(e.target.value)}>
                {(regions.data?.regions ?? [{ code: "IN-SOUTH", name: "India South (Chennai)", country: "IN" }]).map((r) => (
                  <option key={r.code} value={r.code}>
                    {r.name}
                  </option>
                ))}
              </Select>
            </Field>
          </div>

          <div className="mt-6 rounded-lg bg-surface-2 p-4 text-[14px]">
            <p className="font-medium">Before you run the command</p>
            <p className="mt-1 text-muted">
              The agent runs models through a local runtime. On Windows and Linux laptops, desktops and on Macs install{" "}
              <a href="https://ollama.com/download" target="_blank" rel="noreferrer" className="font-medium text-nil underline-offset-4 hover:underline">
                Ollama
              </a>{" "}
              and leave it running. Data-centre NVIDIA nodes can use vLLM instead. NVIDIA GPUs need driver R535 or newer.
            </p>
          </div>

          {error && <Notice tone="error" className="mt-4">{error}</Notice>}
          <Button className="mt-6" onClick={generate} loading={busy}>
            Create install command
          </Button>
        </Panel>
      ) : (
        <div className="mb-6 grid gap-6 lg:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)]">
          <Panel
            title="Run this on the machine"
            description={`Single use, valid until ${new Date(issued.expires_at).toLocaleString("en-IN", { dateStyle: "medium", timeStyle: "short" })}.`}
          >
            <div className="mb-2 flex gap-1" role="tablist" aria-label="Operating system">
              {(
                [
                  ["linux_macos", "macOS or Linux"],
                  ["windows", "Windows"],
                  ["windows_wsl", "Windows (WSL)"],
                  ["from_source", "From this repo"],
                ] as const
              ).map(([k, label]) => (
                <button
                  key={k}
                  role="tab"
                  aria-selected={os === k}
                  onClick={() => setOs(k)}
                  className={cx("h-8 rounded-md px-3 text-[13px] font-medium", os === k ? "bg-nil-soft text-nil" : "text-muted hover:bg-surface-2")}
                >
                  {label}
                </button>
              ))}
            </div>
            <CodeBlock code={issued.commands[os]} />
            <p className="mt-3 text-[13px] text-muted">{WHERE_TO_RUN[os]}</p>
            <p className="mt-3 text-[13px] text-muted">
              The machine enrols as <TierBadge tier={issued.tier} long /> in {issued.region}.{" "}
              {os === "from_source"
                ? "The agent runs in that terminal until you stop it."
                : "The agent then keeps running in the background and starts again at every login, so the terminal can be closed."}{" "}
              You won&apos;t need this token again.
            </p>
            {loopbackOnly(issued.coordinator_url) ? (
              <Notice tone="warn" className="mt-4">
                This command points at <span className="font-mono">{new URL(issued.coordinator_url).host}</span>, so it only works on
                this computer. To add a different machine, connect this computer to a network, or open the console using an
                address the other machine can reach, then create a new command.
              </Notice>
            ) : (
              <p className="mt-3 text-[13px] text-muted">
                The machine must be able to reach <span className="font-mono">{new URL(issued.server_url).host}</span> and{" "}
                <span className="font-mono">{new URL(issued.coordinator_url).host}</span>: the same Wi-Fi or LAN, or a shared VPN.
              </p>
            )}
          </Panel>

          <Panel title="Your machine">
            {joined ? (
              <div className="grid gap-3">
                <StateBadge state={joined.online ? "online" : joined.status} label={joined.online ? "Connected" : undefined} />
                <div className="text-[18px] font-semibold">{joined.name}</div>
                <div className="text-[14px] text-muted">
                  {joined.gpus[0] ? `${joined.gpus[0].model}, ${joined.gpus[0].vram_gb} GB` : "Detecting GPU"}
                </div>
                <div className="text-[14px] text-muted">
                  Status: <StateBadge state={joined.status} />
                </div>
                {!joined.runtime_healthy && (
                  <Notice tone="warn">The agent can&apos;t reach its model runtime yet. Start Ollama on the machine and it will pick up work.</Notice>
                )}
                <ButtonLink href={`/app/hosts/${joined.id}`} className="mt-2 self-start">
                  Open machine
                </ButtonLink>
              </div>
            ) : (
              <div className="grid gap-3">
                <StateBadge state="pending" label="Waiting for the machine to connect" />
                <p className="text-[14px] text-muted">
                  This updates on its own once the agent registers, usually within a minute of running the command.
                </p>
                <Button variant="ghost" className="justify-self-start" onClick={() => setIssued(null)}>
                  Start over
                </Button>
              </div>
            )}
          </Panel>
        </div>
      )}

      <p className="text-[13px] text-muted">
        Running several machines? <Link href="/app/hosts" className="underline">See all your machines</Link>.
      </p>
    </>
  );
}
