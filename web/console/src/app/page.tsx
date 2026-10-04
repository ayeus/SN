"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { Activity, Cpu, MapPin, PlugZap, ShieldCheck, Workflow } from "lucide-react";
import { BRAND } from "@/lib/brand";
import { api } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { useData } from "@/lib/hooks";
import { compact, number, TIERS } from "@/lib/format";
import type { Model, NetworkStats, Tier } from "@/lib/types";
import { ButtonLink, TierBadge, cx } from "@/components/ui";
import { Logo } from "@/components/Logo";
import { PowerRail } from "@/components/PowerRail";

const WRAP = "mx-auto w-full max-w-[1200px] px-5 sm:px-8";

/** The product's promise as code: one changed line. The URL is this site's own. */
function BaseUrlDiff() {
  const [origin, setOrigin] = useState("https://your-endpoint");
  useEffect(() => setOrigin(window.location.origin), []);
  return (
    <pre className="overflow-x-auto px-5 py-4 font-mono text-[13px] leading-7 text-[#e7e8f3]">
      <code>
        <span className="text-[#9ea3bf]">from openai import OpenAI</span>
        {"\n\n"}client = OpenAI({"\n"}
        <span className="block bg-[#c0392b]/20 text-[#f3b1a8]">-    base_url=&quot;https://api.openai.com/v1&quot;,</span>
        <span className="block bg-[#1e8a5a]/25 text-[#a8e6c5]">+    base_url=&quot;{origin}/v1&quot;,</span>
        {"     "}api_key=&quot;sk_live_...&quot;,{"\n"})
      </code>
    </pre>
  );
}

/** Live supply, straight from /v1/network/stats. Shows honest zeros. */
function NetworkNow({ stats }: { stats: NetworkStats | null }) {
  const tiers: Tier[] = ["t1", "t2", "t3"];
  return (
    <div className="border-t border-white/10 px-5 py-4">
      <div className="mb-3 flex items-center justify-between text-[13px]">
        <span className="font-medium text-[#e7e8f3]">Network right now</span>
        <span className="flex items-center gap-2 text-[#9ea3bf]">
          <span
            aria-hidden
            className={cx("h-2 w-2 rounded-full", stats && stats.gpus_online > 0 ? "bg-[#3cc07f] dot-live" : "bg-[#9ea3bf]/50")}
          />
          {!stats ? "Checking" : stats.gpus_online > 0 ? "Live" : "No GPUs online yet"}
        </span>
      </div>
      <dl className="grid grid-cols-3 gap-3">
        {tiers.map((t) => (
          <div key={t} className="rounded-xl bg-white/[0.06] px-3 py-2.5">
            <dt className="text-[12px] text-[#9ea3bf]">
              {TIERS[t].short} {TIERS[t].name}
            </dt>
            <dd className="mt-0.5 text-[20px] font-semibold text-white">
              {stats ? number(stats.gpus_by_tier[t] ?? 0) : "-"}
              <span className="ml-1 text-[12px] font-normal text-[#9ea3bf]">GPUs</span>
            </dd>
          </div>
        ))}
      </dl>
      {stats && stats.tokens_24h > 0 && (
        <p className="mt-3 text-[13px] text-[#9ea3bf]">
          {compact(stats.tokens_24h)} tokens served in the last 24 hours across {number(stats.deployments_serving)} live{" "}
          {stats.deployments_serving === 1 ? "deployment" : "deployments"}.
        </p>
      )}
    </div>
  );
}

function Tile({
  icon: Icon,
  title,
  children,
  className,
  tone = "glass",
}: {
  icon: React.ComponentType<{ size?: number }>;
  title: string;
  children: React.ReactNode;
  className?: string;
  tone?: "glass" | "nil" | "marigold";
}) {
  return (
    <div
      className={cx(
        "flex min-w-0 flex-col gap-3 rounded-2xl p-6",
        tone === "glass" && "glass",
        tone === "nil" && "bg-nil text-on-nil",
        tone === "marigold" && "border border-marigold/40 bg-marigold-soft",
        className,
      )}
    >
      <span
        aria-hidden
        className={cx(
          "grid h-10 w-10 place-items-center rounded-xl",
          tone === "nil" ? "bg-white/15" : tone === "marigold" ? "bg-marigold/25 text-tier3" : "bg-nil-soft text-nil",
        )}
      >
        <Icon size={20} />
      </span>
      <h3 className="text-[18px] font-semibold tracking-[-0.01em]">{title}</h3>
      <div className={cx("text-[15px] leading-relaxed", tone === "nil" ? "text-on-nil/80" : "text-muted")}>{children}</div>
    </div>
  );
}

export default function Landing() {
  const { me } = useAuth();
  const stats = useData(() => api.get<NetworkStats>("/v1/network/stats", false), [], 15_000);
  const models = useData(() => api.get<{ models: Model[] }>("/v1/models", false), []);
  const deployHref = me ? "/app/models" : "/signup";
  const hostHref = me ? "/app/hosts/new" : "/signup?intent=host";

  return (
    <div className="min-h-[100dvh]">
      <header className="sticky top-0 z-30 border-b border-line/70 bg-bg/70 backdrop-blur-xl">
        <div className={cx(WRAP, "flex h-16 items-center justify-between")}>
          <Link href="/" aria-label={`${BRAND.name} home`}>
            <Logo />
          </Link>
          <nav className="flex items-center gap-1" aria-label="Main">
            <a href="#how" className="hidden rounded-lg px-3 py-2 text-[14px] text-muted hover:text-ink md:block">
              How it works
            </a>
            <a href="#models" className="hidden rounded-lg px-3 py-2 text-[14px] text-muted hover:text-ink md:block">
              Models
            </a>
            <a href="#hosts" className="hidden rounded-lg px-3 py-2 text-[14px] text-muted hover:text-ink md:block">
              Host a GPU
            </a>
            {me ? (
              <ButtonLink href="/app" className="ml-2">
                Open console
              </ButtonLink>
            ) : (
              <>
                <Link href="/login" className="rounded-lg px-3 py-2 text-[14px] text-muted hover:text-ink">
                  Sign in
                </Link>
                <ButtonLink href="/signup" className="ml-1">
                  Create account
                </ButtonLink>
              </>
            )}
          </nav>
        </div>
      </header>

      <main>
        <section className={cx(WRAP, "grid items-center gap-12 pb-20 pt-14 lg:grid-cols-[1.25fr_1fr] lg:pt-20")}>
          <div className="min-w-0">
            <h1 className="rise text-[40px] font-semibold leading-[1.06] tracking-[-0.03em] sm:text-[50px]" style={{ "--i": 0 } as React.CSSProperties}>
              Open-source models on Indian GPUs, behind the API you already use.
            </h1>
            <p className="rise mt-5 max-w-[52ch] text-[17px] leading-relaxed text-muted" style={{ "--i": 1 } as React.CSSProperties}>
              Deploy Llama, Qwen or Mistral in minutes and call it with the OpenAI SDK you already have.
            </p>
            <div className="rise mt-8 flex flex-wrap gap-3" style={{ "--i": 2 } as React.CSSProperties}>
              <ButtonLink href={deployHref} className="h-11 px-5 text-[15px]">
                Deploy a model
              </ButtonLink>
              <ButtonLink href={hostHref} variant="secondary" className="h-11 px-5 text-[15px]">
                Connect a GPU
              </ButtonLink>
            </div>
          </div>

          <figure
            className="rise lift min-w-0 overflow-hidden rounded-2xl border border-white/10 bg-[#15172b]"
            style={{ "--i": 3 } as React.CSSProperties}
          >
            <figcaption className="flex items-center gap-2 border-b border-white/10 px-5 py-3 font-mono text-[12px] text-[#9ea3bf]">
              app.py
              <span className="ml-auto rounded-md bg-white/10 px-2 py-0.5 font-sans text-[11px] text-[#e7e8f3]">one line changes</span>
            </figcaption>
            <BaseUrlDiff />
            <NetworkNow stats={stats.data} />
          </figure>
        </section>

        <section id="how" className={cx(WRAP, "pb-24")}>
          <h2 className="max-w-[20ch] text-[30px] font-semibold leading-tight tracking-[-0.02em] sm:text-[36px]">
            From a model name to a production endpoint
          </h2>
          <div className="mt-8 grid gap-4 md:grid-cols-6">
            <div className="glass min-w-0 rounded-2xl p-6 md:col-span-4">
              <span aria-hidden className="grid h-10 w-10 place-items-center rounded-xl bg-nil-soft text-nil">
                <Workflow size={20} />
              </span>
              <h3 className="mt-3 text-[18px] font-semibold tracking-[-0.01em]">You watch it power on</h3>
              <p className="mt-2 max-w-[56ch] text-[15px] leading-relaxed text-muted">
                Pick a model and a tier. The scheduler reserves a GPU, the host pulls and loads the weights, and the
                console shows every stage as it happens. A warm GPU is serving in under a minute.
              </p>
              <div className="mt-6 rounded-xl border border-line bg-surface p-5">
                <PowerRail state="warming" />
              </div>
            </div>
            <Tile icon={Activity} title="Every request is counted" tone="nil" className="md:col-span-2">
              Requests, tokens, latency and errors for each deployment, live in the console and broken down by day.
            </Tile>
            <Tile icon={PlugZap} title="Nothing to rewrite" className="md:col-span-2">
              The endpoint speaks the OpenAI wire format, streaming included. Change the base URL and keep your client,
              your prompts and your code.
            </Tile>
            <Tile icon={MapPin} title="Stays in India" className="md:col-span-2">
              Pin a deployment to Indian regions and it never schedules outside them. Built for teams that answer to
              DPDP.
            </Tile>
            <Tile icon={ShieldCheck} title="You choose the trust level" tone="marigold" className="md:col-span-2">
              Data-centre nodes with an SLA, vetted lab machines, or interruptible spot capacity on personal hardware.
            </Tile>
          </div>
        </section>

        <section className={cx(WRAP, "pb-24")}>
          <h2 className="text-[30px] font-semibold tracking-[-0.02em] sm:text-[36px]">Three tiers of hardware</h2>
          <p className="mt-3 max-w-[62ch] text-[16px] text-muted">
            The tier sets the SLA and what may run there. Sensitive workloads and your own weights never run on
            personal machines.
          </p>
          <div className="glass mt-8 overflow-x-auto rounded-2xl">
            <table className="table min-w-[680px]">
              <thead>
                <tr>
                  <th>Tier</th>
                  <th>Who supplies it</th>
                  <th>Reliability</th>
                  <th className="text-right">Online now</th>
                </tr>
              </thead>
              <tbody>
                {(["t1", "t2", "t3"] as Tier[]).map((t) => (
                  <tr key={t}>
                    <td>
                      <TierBadge tier={t} long />
                    </td>
                    <td className="text-muted">{TIERS[t].who}</td>
                    <td>{TIERS[t].sla}</td>
                    <td className="text-right">{stats.data ? `${number(stats.data.gpus_by_tier[t] ?? 0)} GPUs` : "-"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>

        <section id="models" className={cx(WRAP, "pb-24")}>
          <h2 className="text-[30px] font-semibold tracking-[-0.02em] sm:text-[36px]">Model catalogue</h2>
          <p className="mt-3 max-w-[62ch] text-[16px] text-muted">
            Open-source models ready to deploy, with the GPU memory each one needs and the tiers it can run on.
          </p>
          <div className="glass mt-8 overflow-x-auto rounded-2xl">
            <table className="table min-w-[720px]">
              <thead>
                <tr>
                  <th>Model</th>
                  <th>Licence</th>
                  <th className="text-right">Context</th>
                  <th className="text-right">GPU memory</th>
                  <th>Runs on</th>
                </tr>
              </thead>
              <tbody>
                {(models.data?.models ?? []).map((m) => (
                  <tr key={m.id}>
                    <td>
                      <div className="flex items-center gap-2.5 font-medium">
                        <Cpu size={15} className="text-muted" aria-hidden />
                        {m.name}
                      </div>
                      <div className="ml-[25px] text-[13px] text-muted">{m.params_b}B parameters</div>
                    </td>
                    <td className="text-muted">{m.license}</td>
                    <td className="text-right">{m.context_length ? `${Math.round(m.context_length / 1024)}K` : "-"}</td>
                    <td className="text-right">{m.min_vram_gb} GB</td>
                    <td>
                      <span className="flex flex-wrap gap-1.5">
                        {m.tiers_allowed.map((t) => (
                          <TierBadge key={t} tier={t} />
                        ))}
                      </span>
                    </td>
                  </tr>
                ))}
                {models.data && models.data.models.length === 0 && (
                  <tr>
                    <td colSpan={5} className="text-muted">
                      The model catalogue is empty. Run the seed data to load it.
                    </td>
                  </tr>
                )}
                {models.error && (
                  <tr>
                    <td colSpan={5} className="text-danger">
                      Could not load the catalogue: {models.error.message}
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>
        </section>

        <section id="hosts" className={cx(WRAP, "pb-24")}>
          <h2 className="max-w-[22ch] text-[30px] font-semibold leading-tight tracking-[-0.02em] sm:text-[36px]">
            Put an idle GPU to work
          </h2>
          <p className="mt-3 max-w-[62ch] text-[16px] text-muted">
            One agent, outbound connections only, no ports to open. It works from behind home and campus networks.
          </p>
          <ol className="mt-8 grid gap-4 md:grid-cols-3">
            {[
              ["Create an install command", "Sign in, choose the kind of machine, and the console gives you one command with a single-use token."],
              ["Run it on the machine", "The agent detects the GPU, benchmarks it and connects out to the network. Ollama runs the models."],
              ["Watch it take work", "The machine appears in your console within a minute. Pause it whenever you need the GPU back."],
            ].map(([title, body], i) => (
              <li key={title} className="glass rounded-2xl p-6">
                <span aria-hidden className="grid h-8 w-8 place-items-center rounded-full bg-nil-soft text-[14px] font-semibold text-nil">
                  {i + 1}
                </span>
                <h3 className="mt-3 text-[17px] font-semibold tracking-[-0.01em]">{title}</h3>
                <p className="mt-2 text-[15px] leading-relaxed text-muted">{body}</p>
              </li>
            ))}
          </ol>
        </section>

        <section className={cx(WRAP, "pb-24")}>
          <div className="lift relative overflow-hidden rounded-3xl bg-nil px-8 py-14 text-on-nil sm:px-14">
            <div aria-hidden className="pointer-events-none absolute -right-24 -top-24 h-72 w-72 rounded-full bg-marigold/25 blur-3xl" />
            <h2 className="relative max-w-[18ch] text-[32px] font-semibold leading-tight tracking-[-0.025em] sm:text-[40px]">
              Your first endpoint is a few minutes away.
            </h2>
            <div className="relative mt-8 flex flex-wrap gap-3">
              <Link
                href={deployHref}
                className="press inline-flex h-11 items-center rounded-[10px] bg-on-nil px-5 text-[15px] font-medium text-nil transition-transform"
              >
                Deploy a model
              </Link>
              <Link
                href={hostHref}
                className="press inline-flex h-11 items-center rounded-[10px] border border-on-nil/30 px-5 text-[15px] font-medium text-on-nil transition-transform hover:bg-white/10"
              >
                Connect a GPU
              </Link>
            </div>
          </div>
        </section>
      </main>

      <footer className="border-t border-line">
        <div className={cx(WRAP, "flex flex-wrap items-center justify-between gap-4 py-8 text-[13px] text-muted")}>
          <Logo size={22} />
          <span>
            © {new Date().getFullYear()} {BRAND.company}
          </span>
        </div>
      </footer>
    </div>
  );
}
