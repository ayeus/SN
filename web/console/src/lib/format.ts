import type { Money, Tier } from "./types";

const LOCALE = "en-IN";

/** Formats a money value. Small per-request amounts keep more precision. */
export function money(m: Money | undefined | null, opts: { precise?: boolean } = {}): string {
  if (!m) return "—";
  const value = Number(m.amount);
  const currency = m.currency || "USD";
  const abs = Math.abs(value);
  const digits = opts.precise || (abs > 0 && abs < 1) ? (abs < 0.01 ? 4 : 2) : 2;
  try {
    return new Intl.NumberFormat(LOCALE, {
      style: "currency",
      currency,
      minimumFractionDigits: digits,
      maximumFractionDigits: Math.max(digits, abs > 0 && abs < 0.0001 ? 6 : digits),
    }).format(value);
  } catch {
    return `${value.toFixed(digits)} ${currency}`;
  }
}

/** Converts a USD amount to a display currency using the published FX rate. */
export function convert(usd: Money, currency: string, rateFromUsd: string | undefined): Money {
  const rate = Number(rateFromUsd ?? "1") || 1;
  const value = Number(usd.amount) * (currency === "USD" ? 1 : rate);
  return { amount: value.toString(), currency, micros: Math.round(value * 1e6) };
}

export function compact(n: number): string {
  return new Intl.NumberFormat(LOCALE, { notation: "compact", maximumFractionDigits: 1 }).format(n);
}

export function number(n: number): string {
  return new Intl.NumberFormat(LOCALE).format(n);
}

export function ago(iso?: string | null): string {
  if (!iso) return "never";
  const s = Math.round((Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 5) return "just now";
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

export function dateTime(iso?: string): string {
  if (!iso) return "—";
  return new Date(iso).toLocaleString(LOCALE, { dateStyle: "medium", timeStyle: "short" });
}

export function time(iso: string): string {
  return new Date(iso).toLocaleTimeString(LOCALE, { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

export const TIERS: Record<Tier, { short: string; name: string; who: string; sla: string }> = {
  t1: { short: "T1", name: "Data centre", who: "Data centres and enterprise servers", sla: "99.5% SLA" },
  t2: { short: "T2", name: "Lab & workstation", who: "College labs and vetted workstations", sla: "99% SLA" },
  t3: { short: "T3", name: "Spot", who: "Personal laptops and desktops", sla: "Best effort, interruptible" },
};
