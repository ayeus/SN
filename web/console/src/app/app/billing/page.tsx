"use client";

import { useState } from "react";
import { api, ApiError } from "@/lib/api";
import { BRAND } from "@/lib/brand";
import { useData } from "@/lib/hooks";
import { compact, dateTime, money, number } from "@/lib/format";
import type { LedgerEntry, Money, Wallet } from "@/lib/types";
import { Button, Empty, Field, Input, Notice, PageHeader, Panel, Skeleton, Stat, cx } from "@/components/ui";

type UsageRow = { key: string; requests: number; errors: number; input_tokens: number; output_tokens: number; cost: Money };
type Invoice = { id: string; number: string; period_start: string; period_end: string; subtotal: Money; tax_amount: Money; tax_name: string; total: Money; status: string };

declare global {
  interface Window {
    Razorpay?: new (opts: Record<string, unknown>) => { open: () => void };
  }
}

function loadRazorpay(): Promise<void> {
  if (window.Razorpay) return Promise.resolve();
  return new Promise((resolve, reject) => {
    const s = document.createElement("script");
    s.src = "https://checkout.razorpay.com/v1/checkout.js";
    s.onload = () => resolve();
    s.onerror = () => reject(new Error("Could not load Razorpay checkout"));
    document.body.appendChild(s);
  });
}

const KIND: Record<string, string> = { topup: "Top-up", debit: "Usage", credit: "Credit", refund: "Refund", signup_credit: "Welcome credit" };

export default function BillingPage() {
  const wallet = useData(() => api.get<Wallet>("/v1/billing/wallet"), [], 15_000);
  const ledger = useData(() => api.get<{ entries: LedgerEntry[] }>("/v1/billing/ledger?limit=25"), [], 15_000);
  const [groupBy, setGroupBy] = useState<"deployment" | "day">("deployment");
  const usage = useData(() => api.get<{ rows: UsageRow[] }>(`/v1/usage?group_by=${groupBy}`), [groupBy]);
  const invoices = useData(() => api.get<{ invoices: Invoice[] }>("/v1/invoices"), []);

  const [amount, setAmount] = useState("1000");
  const [busy, setBusy] = useState<"" | "pay" | "test" | "invoice">("");
  const [msg, setMsg] = useState<{ tone: "success" | "error" | "info"; text: string } | null>(null);
  const w = wallet.data;

  async function topup(method: "razorpay" | "test") {
    setBusy(method === "test" ? "test" : "pay");
    setMsg(null);
    try {
      const r = await api.post<{ method: string; order_id?: string; amount?: number; currency?: string; key_id?: string }>(
        "/v1/billing/topup",
        { amount, method },
        { idempotent: true },
      );
      if (r.method === "test") {
        setMsg({ tone: "success", text: `Added ${amount} ${w?.currency} of test credit. This is not real money.` });
      } else {
        await loadRazorpay();
        new window.Razorpay!({
          key: r.key_id,
          order_id: r.order_id,
          amount: r.amount,
          currency: r.currency,
          name: BRAND.name,
          description: "Wallet top-up",
          handler: () => setMsg({ tone: "info", text: "Payment received. Your balance updates as soon as the payment is confirmed." }),
        }).open();
      }
      await Promise.all([wallet.reload(), ledger.reload()]);
    } catch (e) {
      setMsg({ tone: "error", text: e instanceof ApiError ? e.message : "Top-up failed" });
    } finally {
      setBusy("");
    }
  }

  async function generateLastMonth() {
    const now = new Date();
    const start = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() - 1, 1));
    const end = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1));
    const iso = (d: Date) => d.toISOString().slice(0, 10);
    setBusy("invoice");
    try {
      await api.post("/v1/invoices/generate", { period_start: iso(start), period_end: iso(end) }, { idempotent: true });
      await invoices.reload();
    } catch (e) {
      setMsg({ tone: "error", text: e instanceof ApiError ? e.message : "Could not generate the invoice" });
    } finally {
      setBusy("");
    }
  }

  return (
    <>
      <PageHeader title="Billing" description="A prepaid wallet in your billing currency. Usage is charged per token as requests complete." />

      {msg && <Notice tone={msg.tone} className="mb-6">{msg.text}</Notice>}

      <div className="mb-6 grid gap-6 lg:grid-cols-[minmax(0,1.2fr)_minmax(0,1fr)]">
        <Panel>
          {!w ? (
            <Skeleton className="h-16" />
          ) : (
            <div className="grid grid-cols-2 gap-6 sm:grid-cols-3">
              <Stat label="Balance" value={<span className={cx(w.low_balance && "text-danger")}>{money(w.balance, { balance: true })}</span>} sub={w.low_balance ? "Running low" : undefined} />
              <Stat label="Spent today" value={money(w.spend_24h)} />
              <Stat label="Last 30 days" value={money(w.spend_30d)} />
            </div>
          )}
          <p className="mt-5 text-[13px] text-muted">
            Deployments pause automatically when the balance reaches zero, so you never owe more than you topped up.
          </p>
        </Panel>

        <Panel title="Add funds">
          {w && !w.topup.razorpay && !w.topup.test_credit ? (
            <p className="text-muted">Online payments aren&apos;t enabled on this installation yet. Contact support to add credit.</p>
          ) : (
            <div className="grid gap-4">
              <Field label={`Amount (${w?.currency ?? ""})`} htmlFor="amount" hint={w?.currency === "INR" ? "UPI, cards and netbanking via Razorpay. ₹100 to ₹5,00,000." : undefined}>
                <Input id="amount" type="number" min={1} step="1" value={amount} onChange={(e) => setAmount(e.target.value)} />
              </Field>
              <div className="flex flex-wrap gap-2">
                {w?.topup.razorpay && (
                  <Button onClick={() => topup("razorpay")} loading={busy === "pay"}>
                    Pay with Razorpay
                  </Button>
                )}
                {w?.topup.test_credit && (
                  <Button variant="secondary" onClick={() => topup("test")} loading={busy === "test"}>
                    Add test credit
                  </Button>
                )}
              </div>
              {w?.topup.test_credit && !w.topup.razorpay && (
                <p className="text-[13px] text-muted">Development installation: test credit is free and not real money.</p>
              )}
            </div>
          )}
        </Panel>
      </div>

      <Panel
        className="mb-6"
        title="Usage, last 30 days"
        flush
        actions={
          <div className="flex gap-1" role="tablist" aria-label="Group by">
            {(["deployment", "day"] as const).map((g) => (
              <button
                key={g}
                role="tab"
                aria-selected={groupBy === g}
                onClick={() => setGroupBy(g)}
                className={cx("h-7 rounded-md px-2.5 text-[13px]", groupBy === g ? "bg-nil-soft font-medium text-nil" : "text-muted hover:bg-surface-2")}
              >
                By {g}
              </button>
            ))}
          </div>
        }
      >
        {!usage.data ? (
          <div className="p-5">
            <Skeleton className="h-10" />
          </div>
        ) : usage.data.rows.length === 0 ? (
          <p className="p-5 text-muted">No usage in the last 30 days.</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="table min-w-[600px]">
              <thead>
                <tr>
                  <th>{groupBy === "day" ? "Day" : "Deployment"}</th>
                  <th className="text-right">Requests</th>
                  <th className="text-right">Input tokens</th>
                  <th className="text-right">Output tokens</th>
                  <th className="text-right">Cost</th>
                </tr>
              </thead>
              <tbody>
                {usage.data.rows.map((r) => (
                  <tr key={r.key}>
                    <td className="font-medium">{r.key}</td>
                    <td className="text-right">{number(r.requests)}</td>
                    <td className="text-right">{compact(r.input_tokens)}</td>
                    <td className="text-right">{compact(r.output_tokens)}</td>
                    <td className="text-right">{money(r.cost)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>

      <div className="grid gap-6 lg:grid-cols-2">
        <Panel title="Wallet activity" flush>
          {!ledger.data ? (
            <div className="p-5">
              <Skeleton className="h-10" />
            </div>
          ) : ledger.data.entries.length === 0 ? (
            <p className="p-5 text-muted">No wallet activity yet.</p>
          ) : (
            <div className="max-h-[420px] overflow-y-auto">
              <table className="table">
                <tbody>
                  {ledger.data.entries.map((e) => (
                    <tr key={e.entry_id}>
                      <td>
                        <div className="font-medium">{KIND[e.kind] ?? e.kind}</div>
                        <div className="text-[12px] text-muted">{dateTime(e.created_at)}</div>
                      </td>
                      <td className={cx("text-right", Number(e.delta.amount) > 0 ? "text-serving" : "")}>
                        {Number(e.delta.amount) > 0 ? "+" : ""}
                        {money(e.delta, { precise: true })}
                      </td>
                      <td className="text-right text-muted">{money(e.balance_after, { balance: true })}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>

        <Panel
          title="Invoices"
          flush
          actions={
            <Button size="sm" variant="secondary" onClick={generateLastMonth} loading={busy === "invoice"}>
              Invoice last month
            </Button>
          }
        >
          {!invoices.data ? (
            <div className="p-5">
              <Skeleton className="h-10" />
            </div>
          ) : invoices.data.invoices.length === 0 ? (
            <Empty title="No invoices yet">Invoices are issued monthly with GST for Indian organisations.</Empty>
          ) : (
            <table className="table">
              <thead>
                <tr>
                  <th>Invoice</th>
                  <th>Period</th>
                  <th className="text-right">Tax</th>
                  <th className="text-right">Total</th>
                </tr>
              </thead>
              <tbody>
                {invoices.data.invoices.map((i) => (
                  <tr key={i.id}>
                    <td className="font-mono text-[13px]">{i.number}</td>
                    <td className="text-[13px] text-muted">
                      {i.period_start.slice(0, 10)} to {i.period_end.slice(0, 10)}
                    </td>
                    <td className="text-right text-[13px]">
                      {money(i.tax_amount)} <span className="text-muted">{i.tax_name}</span>
                    </td>
                    <td className="text-right font-medium">{money(i.total)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Panel>
      </div>
    </>
  );
}
