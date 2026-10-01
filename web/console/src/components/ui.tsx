"use client";

import Link from "next/link";
import { forwardRef, useEffect, useRef } from "react";
import { Check, Copy } from "lucide-react";
import { useCopy } from "@/lib/hooks";
import { TIERS } from "@/lib/format";
import type { Tier } from "@/lib/types";

const cx = (...c: (string | false | null | undefined)[]) => c.filter(Boolean).join(" ");

// ─── Buttons ─────────────────────────────────────────────────

type ButtonProps = React.ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: "primary" | "secondary" | "ghost" | "danger";
  size?: "sm" | "md";
  loading?: boolean;
};

const buttonClass = (variant: ButtonProps["variant"] = "primary", size: ButtonProps["size"] = "md") =>
  cx(
    "inline-flex items-center justify-center gap-2 rounded-lg font-medium transition-colors disabled:opacity-50 disabled:cursor-not-allowed whitespace-nowrap",
    size === "sm" ? "h-8 px-3 text-[13px]" : "h-10 px-4 text-[14px]",
    variant === "primary" && "bg-nil text-on-nil hover:bg-nil-hover",
    variant === "secondary" && "bg-surface text-ink border border-line hover:bg-surface-2",
    variant === "ghost" && "text-muted hover:text-ink hover:bg-surface-2",
    variant === "danger" && "bg-surface text-danger border border-line hover:bg-danger-soft",
  );

export function Button({ variant, size, loading, className, children, disabled, ...rest }: ButtonProps) {
  return (
    <button className={cx(buttonClass(variant, size), className)} disabled={disabled || loading} {...rest}>
      {loading && <span className="h-3.5 w-3.5 rounded-full border-2 border-current border-r-transparent animate-spin" />}
      {children}
    </button>
  );
}

export function ButtonLink({
  href,
  variant,
  size,
  className,
  children,
}: {
  href: string;
  variant?: ButtonProps["variant"];
  size?: ButtonProps["size"];
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <Link href={href} className={cx(buttonClass(variant, size), className)}>
      {children}
    </Link>
  );
}

// ─── Forms ───────────────────────────────────────────────────

export function Field({
  label,
  hint,
  error,
  children,
  htmlFor,
}: {
  label: string;
  hint?: React.ReactNode;
  error?: string;
  children: React.ReactNode;
  htmlFor?: string;
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor={htmlFor} className="text-[13px] font-medium text-ink">
        {label}
      </label>
      {children}
      {error ? (
        <p className="text-[13px] text-danger">{error}</p>
      ) : hint ? (
        <p className="text-[13px] text-muted">{hint}</p>
      ) : null}
    </div>
  );
}

const inputClass =
  "h-10 w-full rounded-lg border border-line bg-surface px-3 text-[14px] text-ink placeholder:text-muted/70 focus:border-nil focus:outline-none focus:ring-2 focus:ring-nil/20 aria-[invalid=true]:border-danger";

export const Input = forwardRef<HTMLInputElement, React.InputHTMLAttributes<HTMLInputElement>>(function Input(
  { className, ...rest },
  ref,
) {
  return <input ref={ref} className={cx(inputClass, className)} {...rest} />;
});

export function Select({ className, children, ...rest }: React.SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <select className={cx(inputClass, "pr-8", className)} {...rest}>
      {children}
    </select>
  );
}

// ─── Layout ──────────────────────────────────────────────────

export function PageHeader({
  title,
  description,
  actions,
  back,
}: {
  title: React.ReactNode;
  description?: React.ReactNode;
  actions?: React.ReactNode;
  back?: { href: string; label: string };
}) {
  return (
    <header className="mb-8 flex flex-wrap items-end justify-between gap-4">
      <div className="min-w-0">
        {back && (
          <Link href={back.href} className="mb-2 inline-block text-[13px] text-muted hover:text-ink">
            ← {back.label}
          </Link>
        )}
        <h1 className="text-[26px] font-semibold leading-tight tracking-[-0.01em]">{title}</h1>
        {description && <p className="mt-1.5 max-w-[68ch] text-muted">{description}</p>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </header>
  );
}

export function Panel({
  title,
  description,
  actions,
  children,
  className,
  bodyClassName,
  flush,
}: {
  title?: React.ReactNode;
  description?: React.ReactNode;
  actions?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
  bodyClassName?: string;
  flush?: boolean;
}) {
  return (
    // min-w-0: a grid or flex child defaults to min-width:auto, so one long
    // line of code would push the whole page wider than a phone screen.
    <section className={cx("min-w-0 rounded-xl border border-line bg-surface", className)}>
      {(title || actions) && (
        <div className="flex flex-wrap items-start justify-between gap-3 border-b border-line px-5 py-4">
          <div>
            {title && <h2 className="text-[15px] font-semibold">{title}</h2>}
            {description && <p className="mt-0.5 text-[13px] text-muted">{description}</p>}
          </div>
          {actions}
        </div>
      )}
      <div className={cx(flush ? "" : "p-5", bodyClassName)}>{children}</div>
    </section>
  );
}

export function Stat({ label, value, sub }: { label: string; value: React.ReactNode; sub?: React.ReactNode }) {
  return (
    <div>
      <div className="text-[13px] text-muted">{label}</div>
      <div className="mt-1 text-[22px] font-semibold leading-tight tracking-[-0.01em]">{value}</div>
      {sub && <div className="mt-0.5 text-[13px] text-muted">{sub}</div>}
    </div>
  );
}

export function Empty({ title, children, action }: { title: string; children?: React.ReactNode; action?: React.ReactNode }) {
  return (
    <div className="flex flex-col items-start gap-3 px-5 py-10">
      <h3 className="text-[16px] font-semibold">{title}</h3>
      {children && <div className="max-w-[60ch] text-muted">{children}</div>}
      {action}
    </div>
  );
}

export function Notice({
  tone = "info",
  children,
  className,
}: {
  tone?: "info" | "warn" | "error" | "success";
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div
      role={tone === "error" ? "alert" : "status"}
      className={cx(
        "rounded-lg border px-4 py-3 text-[14px]",
        tone === "info" && "border-line bg-surface-2 text-ink",
        tone === "warn" && "border-marigold/40 bg-marigold-soft text-ink",
        tone === "error" && "border-danger/30 bg-danger-soft text-ink",
        tone === "success" && "border-serving/30 bg-serving-soft text-ink",
        className,
      )}
    >
      {children}
    </div>
  );
}

export function Skeleton({ className }: { className?: string }) {
  return <div className={cx("animate-pulse rounded-md bg-surface-2", className)} />;
}

// ─── Status ──────────────────────────────────────────────────

export function TierBadge({ tier, long }: { tier: Tier; long?: boolean }) {
  const t = TIERS[tier];
  const color = tier === "t1" ? "text-tier1" : tier === "t2" ? "text-tier2" : "text-tier3";
  return (
    <span className={cx("inline-flex items-center gap-1.5 rounded-md border border-line px-1.5 py-0.5 text-[12px] font-medium", color)}>
      <span className="font-semibold">{t.short}</span>
      {long && <span className="text-muted">{t.name}</span>}
    </span>
  );
}

const STATE_TONE: Record<string, "live" | "good" | "bad" | "idle"> = {
  pending: "live",
  scheduling: "live",
  pulling: "live",
  loading: "live",
  warming: "live",
  stopping: "live",
  serving: "good",
  active: "good",
  online: "good",
  degraded: "bad",
  failed: "bad",
  banned: "bad",
  demoted: "bad",
  paused: "idle",
  stopped: "idle",
  offline: "idle",
  draining: "live",
  probation: "good",
  benchmarking: "live",
  registered: "live",
};

const STATE_LABEL: Record<string, string> = {
  pending: "Waiting for capacity",
  scheduling: "Scheduling",
  pulling: "Pulling weights",
  loading: "Loading",
  warming: "Warming up",
  serving: "Serving",
  degraded: "Degraded",
  paused: "Paused",
  stopping: "Stopping",
  stopped: "Stopped",
  failed: "Failed",
  active: "Active",
  offline: "Offline",
  draining: "Draining",
  probation: "Probation",
  demoted: "Demoted",
  banned: "Banned",
  benchmarking: "Benchmarking",
  registered: "Registered",
  online: "Online",
};

export function StateBadge({ state, label }: { state: string; label?: string }) {
  const tone = STATE_TONE[state] ?? "idle";
  return (
    <span className="inline-flex items-center gap-2 text-[13px] font-medium">
      <span
        aria-hidden
        className={cx(
          "h-2 w-2 rounded-full",
          tone === "live" && "bg-marigold dot-live",
          tone === "good" && "bg-serving",
          tone === "bad" && "bg-danger",
          tone === "idle" && "bg-muted/50",
        )}
      />
      {label ?? STATE_LABEL[state] ?? state}
    </span>
  );
}

// ─── Code and secrets ────────────────────────────────────────

export function CopyField({ value, label, secret }: { value: string; label?: string; secret?: boolean }) {
  const [copied, copy] = useCopy();
  return (
    <div className="flex min-w-0 items-center gap-2 rounded-lg border border-line bg-surface-2 py-1.5 pl-3 pr-1.5">
      {label && <span className="shrink-0 text-[13px] text-muted">{label}</span>}
      <code className={cx("min-w-0 flex-1 truncate font-mono text-[13px]", secret && "select-all")}>{value}</code>
      <button
        type="button"
        onClick={() => copy(value)}
        className="inline-flex h-7 shrink-0 items-center gap-1.5 rounded-md px-2 text-[12px] font-medium text-muted hover:bg-surface hover:text-ink"
        aria-label={`Copy ${label ?? "value"}`}
      >
        {copied === value ? <Check size={14} className="text-serving" /> : <Copy size={14} />}
        {copied === value ? "Copied" : "Copy"}
      </button>
    </div>
  );
}

export function CodeBlock({ code, label, wrap }: { code: string; label?: string; wrap?: boolean }) {
  const [copied, copy] = useCopy();
  return (
    <div className="min-w-0 overflow-hidden rounded-lg border border-line bg-[#15172b] text-[#e7e8f3]">
      <div className="flex items-center justify-between gap-2 border-b border-white/10 py-1 pl-4 pr-1.5">
        <span className="truncate font-mono text-[12px] text-[#9ea3bf]">{label ?? ""}</span>
        <button
          type="button"
          onClick={() => copy(code)}
          className="inline-flex h-7 shrink-0 items-center gap-1.5 rounded-md px-2 text-[12px] text-[#b9bbd4] hover:bg-white/10"
          aria-label={label ? `Copy ${label}` : "Copy code"}
        >
          {copied === code ? <Check size={14} /> : <Copy size={14} />}
          {copied === code ? "Copied" : "Copy"}
        </button>
      </div>
      <pre
        className={cx(
          "p-4 font-mono text-[13px] leading-relaxed",
          wrap ? "whitespace-pre-wrap break-all" : "overflow-x-auto",
        )}
      >
        <code>{code}</code>
      </pre>
    </div>
  );
}

// ─── Confirmation ────────────────────────────────────────────

export function Confirm({
  open,
  title,
  body,
  confirmLabel,
  tone = "danger",
  busy,
  onConfirm,
  onCancel,
}: {
  open: boolean;
  title: string;
  body: React.ReactNode;
  confirmLabel: string;
  tone?: "danger" | "primary";
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (open && !d.open) d.showModal();
    if (!open && d.open) d.close();
  }, [open]);
  return (
    <dialog
      ref={ref}
      onCancel={(e) => {
        e.preventDefault();
        onCancel();
      }}
      className="m-auto w-[min(440px,calc(100vw-32px))] rounded-xl border border-line bg-surface p-0 text-ink backdrop:bg-black/40"
    >
      <div className="p-6">
        <h2 className="text-[17px] font-semibold">{title}</h2>
        <div className="mt-2 text-muted">{body}</div>
        <div className="mt-6 flex justify-end gap-2">
          <Button variant="secondary" onClick={onCancel}>
            Cancel
          </Button>
          <Button variant={tone === "danger" ? "danger" : "primary"} loading={busy} onClick={onConfirm}>
            {confirmLabel}
          </Button>
        </div>
      </div>
    </dialog>
  );
}

export { cx };
