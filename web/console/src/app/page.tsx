"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { BRAND } from "@/lib/brand";
import { api } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { useData } from "@/lib/hooks";
import { compact, convert, money, number, TIERS } from "@/lib/format";
import type { Model, NetworkStats, Pricing, Tier } from "@/lib/types";
import { ButtonLink } from "@/components/ui";
import { EarningsCalculator } from "@/components/EarningsCalculator";

function BaseUrlDiff() {
  const [origin, setOrigin] = useState("https://your-endpoint");
  useEffect(() => {
    setOrigin(window.location.origin);
  }, []);
  return (
    <figure className="overflow-hidden rounded-xl border border-line bg-[#15172b] text-[#e7e8f3] shadow-[0_24px_60px_-30px_rgba(35,36,106,0.55)]">
      <figcaption className="border-b border-white/10 px-4 py-2.5 font-mono text-[12px] text-[#9ea3bf]">app.py</figcaption>
      <pre className="overflow-x-auto px-4 py-4 font-mono text-[13.5px] leading-7">
        <code>
          <span className="text-[#9ea3bf]">from openai import OpenAI</span>
          {"\n\n"}
          client = OpenAI({"\n"}
          <span className="block bg-[#c0392b]/20 text-[#f3b1a8]">-    base_url=&quot;https://api.openai.com/v1&quot;,</span>
          <span className="block bg-[#1e8a5a]/25 text-[#a8e6c5]">+    base_url=&quot;{origin}/v1&quot;,</span>
          {"     "}api_key=&quot;sk_live_…&quot;,{"\n"}){"\n\n"}
          <span className="text-[#9ea3bf]"># everything else stays the same</span>
          {"\n"}client.chat.completions.create(model=<span className="text-[#f2c46b]">&quot;qwen-chat&quot;</span>, …)
        </code>
      </pre>
    </figure>
  );
}

function NetworkLine({ stats }: { stats: NetworkStats | null }) {
  if (!stats) return <div className="h-12" />;
  if (stats.gpus_online === 0) {
    return (
      <p className="text-[14px] text-muted">
        No GPUs are online on this network yet.{" "}
        <Link href="/signup?intent=host" className="font-medium text-nil underline-offset-4 hover:underline">
          Connect the first one
        </Link>
        .
      </p>
    );
  }
  const items = [
    { v: number(stats.gpus_online), l: stats.gpus_online === 1 ? "GPU online now" : "GPUs online now" },
    { v: `${number(stats.vram_gb_online)} GB`, l: "of GPU memory" },
    { v: compact(stats.tokens_24h), l: "tokens served today" },
  ];
  return (
    <dl className="flex flex-wrap gap-x-10 gap-y-3">
      {items.map((i) => (
        <div key={i.l}>
          <dt className="sr-only">{i.l}</dt>
          <dd>
            <span className="text-[20px] font-semibold">{i.v}</span> <span className="text-[14px] text-muted">{i.l}</span>
          </dd>
        </div>
      ))}
    </dl>
  );
}

export default function Landing() {
  const { me } = useAuth();
  const stats = useData(() => api.get<NetworkStats>("/v1/network/stats", false), [], 15_000);
  const pricing = useData(() => api.get<Pricing>("/v1/pricing", false), []);
  const models = useData(() => api.get<{ models: Model[] }>("/v1/models", false), []);
  const inrRate = pricing.data?.fx_from_usd?.INR;
  const spot = (pricing.data?.spot_price_percent ?? 55) / 100;

  const tierFrom = (t: Tier) => {
    const prices = (pricing.data?.gpu_skus ?? []).filter((s) => s.tier === t).map((s) => Number(s.price_per_hour_inr.amount));
    return prices.length ? Math.min(...prices) : null;
  };

  return (
    <div className="min-h-screen">
      <header className="mx-auto flex max-w-[1180px] items-center justify-between px-5 py-5 sm:px-8">
        <Link href="/" className="text-[17px] font-semibold tracking-[-0.01em]">
          {BRAND.name}
        </Link>
        <nav className="flex items-center gap-1 sm:gap-2">
          <a href="#pricing" className="hidden rounded-md px-3 py-2 text-[14px] text-muted hover:text-ink sm:block">
            Pricing
          </a>
          <a href="#hosts" className="hidden rounded-md px-3 py-2 text-[14px] text-muted hover:text-ink sm:block">
            Host a GPU
          </a>
          {me ? (
            <ButtonLink href="/app">Open console</ButtonLink>
          ) : (
            <>
              <Link href="/login" className="rounded-md px-3 py-2 text-[14px] text-muted hover:text-ink">
                Sign in
              </Link>
              <ButtonLink href="/signup">Create account</ButtonLink>
            </>
          )}
        </nav>
      </header>

      <main>
        <section className="mx-auto grid max-w-[1180px] items-center gap-12 px-5 pb-16 pt-10 sm:px-8 lg:grid-cols-[1.05fr_1fr] lg:pt-16">
          <div>
            <h1 className="text-[40px] font-semibold leading-[1.08] tracking-[-0.025em] sm:text-[52px]">
              Open-source models on Indian GPUs, behind the API you already use.
            </h1>
            <p className="mt-5 max-w-[56ch] text-[17px] leading-relaxed text-muted">
              Deploy Llama, Qwen or Mistral in minutes and call it with the OpenAI SDK. Pay per token in rupees, starting
              with ₹500 of free credit. Choose data-centre reliability or spot pricing on community hardware.
            </p>
            <div className="mt-8 flex flex-wrap gap-3">
              <ButtonLink href={me ? "/app/models" : "/signup"}>Deploy a model</ButtonLink>
              <ButtonLink href={me ? "/app/hosts/new" : "/signup?intent=host"} variant="secondary">
                Connect a GPU
              </ButtonLink>
            </div>
            <div className="mt-10">
              <NetworkLine stats={stats.data} />
            </div>
          </div>
          <BaseUrlDiff />
        </section>

        <section className="border-y border-line bg-surface">
          <div className="mx-auto max-w-[1180px] px-5 py-16 sm:px-8">
            <h2 className="text-[28px] font-semibold tracking-[-0.015em]">Choose how much reliability you pay for</h2>
            <p className="mt-2 max-w-[64ch] text-muted">
              Every machine on the network sits in one of three tiers. The tier decides the SLA, what may run there and
              the price. Sensitive workloads and bring-your-own weights never run on personal machines.
            </p>
            <div className="mt-8 overflow-x-auto">
              <table className="table min-w-[640px]">
                <thead>
                  <tr>
                    <th>Tier</th>
                    <th>Who supplies it</th>
                    <th>Reliability</th>
                    <th>GPUs online</th>
                    <th className="text-right">From</th>
                  </tr>
                </thead>
                <tbody>
                  {(["t1", "t2", "t3"] as Tier[]).map((t) => (
                    <tr key={t}>
                      <td className="font-medium">
                        {TIERS[t].short} {TIERS[t].name}
                      </td>
                      <td className="text-muted">{TIERS[t].who}</td>
                      <td>{TIERS[t].sla}</td>
                      <td>{stats.data ? number(stats.data.gpus_by_tier[t] ?? 0) : "—"}</td>
                      <td className="text-right">{tierFrom(t) != null ? `₹${tierFrom(t)}/GPU-hour` : "—"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        </section>

        <section id="pricing" className="mx-auto max-w-[1180px] px-5 py-16 sm:px-8">
          <h2 className="text-[28px] font-semibold tracking-[-0.015em]">Pay per token</h2>
          <p className="mt-2 max-w-[64ch] text-muted">
            Managed endpoints are billed per million tokens, per second of use, from a prepaid wallet with GST invoices.
            Spot capacity costs {Math.round(spot * 100)}% of the on-demand price.
          </p>
          <div className="mt-8 overflow-x-auto rounded-xl border border-line bg-surface">
            <table className="table min-w-[720px]">
              <thead>
                <tr>
                  <th>Model</th>
                  <th>Licence</th>
                  <th className="text-right">Input / 1M</th>
                  <th className="text-right">Output / 1M</th>
                  <th className="text-right">Spot output / 1M</th>
                </tr>
              </thead>
              <tbody>
                {(models.data?.models ?? []).map((m) => {
                  const inPrice = inrRate ? convert(m.price_in_per_1m, "INR", inrRate) : m.price_in_per_1m;
                  const outPrice = inrRate ? convert(m.price_out_per_1m, "INR", inrRate) : m.price_out_per_1m;
                  const spotOut = { ...outPrice, amount: String(Number(outPrice.amount) * spot) };
                  return (
                    <tr key={m.id}>
                      <td>
                        <div className="font-medium">{m.name}</div>
                        <div className="text-[13px] text-muted">{m.params_b}B parameters</div>
                      </td>
                      <td className="text-muted">{m.license}</td>
                      <td className="text-right">{money(inPrice)}</td>
                      <td className="text-right">{money(outPrice)}</td>
                      <td className="text-right">{m.tiers_allowed.includes("t3") ? money(spotOut) : <span className="text-muted">not on spot</span>}</td>
                    </tr>
                  );
                })}
                {models.data && models.data.models.length === 0 && (
                  <tr>
                    <td colSpan={5} className="text-muted">
                      The model catalogue is empty. Run the seed data to load it.
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>
        </section>

        <section id="hosts" className="border-t border-line bg-surface">
          <div className="mx-auto max-w-[1180px] px-5 py-16 sm:px-8">
            <h2 className="text-[28px] font-semibold tracking-[-0.015em]">Your GPU earns while you don&apos;t use it</h2>
            <p className="mt-2 max-w-[64ch] text-muted">
              Install one agent and your machine joins the network. It only makes outbound connections, runs jobs in
              isolation, and you keep {pricing.data?.host_share_percent ?? 75}% of what customers pay for its time.
            </p>
            <div className="mt-8">{pricing.data && <EarningsCalculator pricing={pricing.data} />}</div>
            <div className="mt-8">
              <ButtonLink href={me ? "/app/hosts/new" : "/signup?intent=host"}>Connect a GPU</ButtonLink>
            </div>
          </div>
        </section>
      </main>

      <footer className="mx-auto flex max-w-[1180px] flex-wrap justify-between gap-4 px-5 py-10 text-[13px] text-muted sm:px-8">
        <span>
          © {new Date().getFullYear()} {BRAND.company}
        </span>
        <span>Prices exclude GST. Data stays in the region you choose.</span>
      </footer>
    </div>
  );
}
