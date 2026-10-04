"use client";

import Link from "next/link";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { Suspense, useEffect, useState } from "react";
import {
  Boxes,
  CreditCard,
  Gauge,
  KeyRound,
  LayoutGrid,
  LogOut,
  Menu,
  MessageSquare,
  PlusCircle,
  Server,
  Settings,
  ShieldCheck,
  Wallet as WalletIcon,
  X,
} from "lucide-react";
import { Logo } from "./Logo";
import { useAuth } from "@/lib/auth";
import { api } from "@/lib/api";
import { money } from "@/lib/format";
import { useData } from "@/lib/hooks";
import type { Wallet } from "@/lib/types";
import { Notice, cx } from "./ui";

// Shown once after sign-up, on whichever page sign-up sent the user to.
function WelcomeNotice() {
  const welcome = useSearchParams().get("welcome");
  const [dismissed, setDismissed] = useState(false);
  if (!welcome || dismissed) return null;
  return (
    <Notice tone={welcome === "none" ? "warn" : "success"} className="mb-6 flex items-start justify-between gap-4">
      {welcome === "none" ? (
        <span>
          Your account is ready. No welcome credit was added because too many accounts were created from this network
          today.{" "}
          <Link href="/app/billing" className="font-medium underline">
            Add funds
          </Link>{" "}
          to deploy a model.
        </span>
      ) : (
        <span>Your account is ready and {welcome} of credit is in your wallet.</span>
      )}
      <button onClick={() => setDismissed(true)} className="text-[13px] text-muted hover:text-ink" aria-label="Dismiss">
        Dismiss
      </button>
    </Notice>
  );
}

type Item = { href: string; label: string; icon: React.ComponentType<{ size?: number }>; exact?: boolean };

const DEPLOY: Item[] = [
  { href: "/app", label: "Overview", icon: Gauge, exact: true },
  { href: "/app/models", label: "Models", icon: LayoutGrid },
  { href: "/app/deployments", label: "Deployments", icon: Boxes },
  { href: "/app/playground", label: "Playground", icon: MessageSquare },
  { href: "/app/keys", label: "API keys", icon: KeyRound },
  { href: "/app/billing", label: "Billing", icon: CreditCard },
];

const HOST: Item[] = [
  { href: "/app/hosts", label: "Machines", icon: Server, exact: true },
  { href: "/app/hosts/new", label: "Add a machine", icon: PlusCircle },
  { href: "/app/hosts/earnings", label: "Earnings", icon: WalletIcon },
];

export function AppShell({ children }: { children: React.ReactNode }) {
  const { me, loading, logout } = useAuth();
  const pathname = usePathname();
  const router = useRouter();
  const [open, setOpen] = useState(false);
  const hostMode = pathname.startsWith("/app/hosts");
  const wallet = useData(me ? () => api.get<Wallet>("/v1/billing/wallet") : null, [me?.organization.id], 30_000);

  useEffect(() => {
    if (!loading && !me) router.replace(`/login?next=${encodeURIComponent(pathname)}`);
  }, [loading, me, pathname, router]);
  useEffect(() => {
    setOpen(false);
  }, [pathname]);

  if (loading || !me) {
    return <div className="grid min-h-[100dvh] place-items-center text-muted">Loading your workspace…</div>;
  }

  const items = hostMode ? HOST : DEPLOY;
  const isActive = (i: Item) => (i.exact ? pathname === i.href : pathname === i.href || pathname.startsWith(i.href + "/"));

  const nav = (
    <nav className="flex h-full flex-col gap-6 px-3 py-5">
      <Link href="/app" className="px-2 text-[17px] font-semibold tracking-[-0.01em]">
        <Logo />
      </Link>

      <div className="grid grid-cols-2 rounded-lg bg-surface-2 p-1 text-[13px] font-medium" role="tablist" aria-label="Workspace">
        <Link
          href="/app"
          role="tab"
          aria-selected={!hostMode}
          className={cx("rounded-md py-1.5 text-center", !hostMode ? "bg-surface text-ink shadow-sm" : "text-muted hover:text-ink")}
        >
          Deploy
        </Link>
        <Link
          href="/app/hosts"
          role="tab"
          aria-selected={hostMode}
          className={cx("rounded-md py-1.5 text-center", hostMode ? "bg-surface text-ink shadow-sm" : "text-muted hover:text-ink")}
        >
          Host
        </Link>
      </div>

      <ul className="flex flex-col gap-0.5">
        {items.map((i) => (
          <li key={i.href}>
            <Link
              href={i.href}
              aria-current={isActive(i) ? "page" : undefined}
              className={cx(
                "flex h-9 items-center gap-2.5 rounded-lg px-2.5 text-[14px]",
                isActive(i) ? "bg-nil-soft font-medium text-nil shadow-[inset_2px_0_0_var(--nil)]" : "text-muted hover:bg-surface-2 hover:text-ink",
              )}
            >
              <i.icon size={16} />
              {i.label}
            </Link>
          </li>
        ))}
      </ul>

      <div className="mt-auto flex flex-col gap-3">
        {!hostMode && wallet.data && (
          <Link href="/app/billing" className="rounded-lg border border-line px-3 py-2.5 hover:bg-surface-2">
            <div className="text-[12px] text-muted">Wallet balance</div>
            <div className={cx("text-[15px] font-semibold", wallet.data.low_balance && "text-danger")}>
              {money(wallet.data.balance, { balance: true })}
            </div>
          </Link>
        )}
        <ul className="flex flex-col gap-0.5">
          {me.is_platform_admin && (
            <li>
              <Link href="/app/admin" className="flex h-9 items-center gap-2.5 rounded-lg px-2.5 text-[14px] text-muted hover:bg-surface-2 hover:text-ink">
                <ShieldCheck size={16} /> Operations
              </Link>
            </li>
          )}
          <li>
            <Link href="/app/settings" className="flex h-9 items-center gap-2.5 rounded-lg px-2.5 text-[14px] text-muted hover:bg-surface-2 hover:text-ink">
              <Settings size={16} /> Settings
            </Link>
          </li>
          <li>
            <button
              onClick={async () => {
                await logout();
                router.replace("/login");
              }}
              className="flex h-9 w-full items-center gap-2.5 rounded-lg px-2.5 text-left text-[14px] text-muted hover:bg-surface-2 hover:text-ink"
            >
              <LogOut size={16} /> Sign out
            </button>
          </li>
        </ul>
        <div className="flex items-center gap-2.5 border-t border-line px-2.5 pt-3 text-[12px] text-muted">
          <span aria-hidden className="grid h-8 w-8 shrink-0 place-items-center rounded-full bg-nil text-[12px] font-semibold text-on-nil">
            {me.user.name
              .split(/\s+/)
              .map((w) => w[0])
              .slice(0, 2)
              .join("")
              .toUpperCase()}
          </span>
          <div className="min-w-0">
            <div className="truncate font-medium text-ink">{me.user.name}</div>
            <div className="truncate">{me.organization.name}</div>
          </div>
        </div>
      </div>
    </nav>
  );

  return (
    <div className="min-h-[100dvh] lg:grid lg:grid-cols-[248px_1fr]">
      <aside className="sticky top-0 hidden h-[100dvh] border-r border-line bg-[var(--glass)] backdrop-blur-xl lg:block">{nav}</aside>

      <div className="flex items-center justify-between border-b border-line bg-surface px-4 py-3 lg:hidden">
        <Link href="/app" className="font-semibold">
          <Logo />
        </Link>
        <button onClick={() => setOpen(true)} aria-label="Open navigation" className="rounded-md p-1.5 hover:bg-surface-2">
          <Menu size={20} />
        </button>
      </div>
      {open && (
        <div className="fixed inset-0 z-40 lg:hidden">
          <div className="absolute inset-0 bg-black/40" onClick={() => setOpen(false)} />
          <aside className="absolute inset-y-0 left-0 w-[272px] bg-surface">
            <button onClick={() => setOpen(false)} aria-label="Close navigation" className="absolute right-3 top-4 rounded-md p-1.5 hover:bg-surface-2">
              <X size={18} />
            </button>
            {nav}
          </aside>
        </div>
      )}

      <main className="min-w-0 px-4 py-8 sm:px-8 lg:px-10">
        <div className="mx-auto max-w-[1180px]">
          <Suspense>
            <WelcomeNotice />
          </Suspense>
          {children}
        </div>
      </main>
    </div>
  );
}
