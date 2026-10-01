"use client";

import { useSearchParams } from "next/navigation";
import { Suspense, useEffect, useRef, useState } from "react";
import { Send, Square } from "lucide-react";
import { api, getSession } from "@/lib/api";
import { useData } from "@/lib/hooks";
import type { Deployment } from "@/lib/types";
import { Button, ButtonLink, Empty, Field, Input, Notice, PageHeader, Panel, Select, cx } from "@/components/ui";

type Msg = { role: "user" | "assistant" | "system"; content: string };
type Stats = { ttftMs: number; totalMs: number; prompt?: number; completion?: number };

function Playground() {
  const params = useSearchParams();
  const deps = useData(() => api.get<{ deployments: Deployment[] }>("/v1/deployments"), [], 10_000);
  const serving = (deps.data?.deployments ?? []).filter((d) => d.state === "serving" || d.state === "degraded");

  const [target, setTarget] = useState(params.get("deployment") ?? "");
  const [system, setSystem] = useState("You are a helpful assistant.");
  const [temperature, setTemperature] = useState(0.7);
  const [maxTokens, setMaxTokens] = useState(512);
  const [messages, setMessages] = useState<Msg[]>([]);
  const [input, setInput] = useState("");
  const [streaming, setStreaming] = useState(false);
  const [stats, setStats] = useState<Stats | null>(null);
  const [error, setError] = useState("");
  const abort = useRef<AbortController | null>(null);
  const bottom = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!target && serving.length) setTarget(serving[0].name);
  }, [serving, target]);
  // Braces matter: scrollIntoView returns a Promise in current browsers, and an
  // effect that returns anything but a cleanup function crashes React.
  useEffect(() => {
    bottom.current?.scrollIntoView({ block: "end" });
  }, [messages]);

  async function send() {
    const text = input.trim();
    if (!text || !target || streaming) return;
    setError("");
    setInput("");
    const history: Msg[] = [...messages, { role: "user", content: text }];
    setMessages([...history, { role: "assistant", content: "" }]);
    setStreaming(true);
    const ctrl = new AbortController();
    abort.current = ctrl;
    const start = performance.now();
    let first = 0;

    try {
      const res = await fetch("/v1/chat/completions", {
        method: "POST",
        signal: ctrl.signal,
        headers: { "Content-Type": "application/json", Authorization: `Bearer ${getSession()?.access ?? ""}` },
        body: JSON.stringify({
          model: target,
          messages: [...(system.trim() ? [{ role: "system", content: system }] : []), ...history],
          temperature,
          max_tokens: maxTokens,
          stream: true,
          stream_options: { include_usage: true },
        }),
      });
      if (!res.ok || !res.body) {
        const body = await res.json().catch(() => ({}));
        throw new Error(body?.error?.message ?? `Request failed (${res.status})`);
      }
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buf = "";
      let usage: { prompt_tokens?: number; completion_tokens?: number } | undefined;
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        let idx;
        while ((idx = buf.indexOf("\n\n")) >= 0) {
          const event = buf.slice(0, idx);
          buf = buf.slice(idx + 2);
          for (const line of event.split("\n")) {
            if (!line.startsWith("data:")) continue;
            const data = line.slice(5).trim();
            if (data === "[DONE]") continue;
            try {
              const chunk = JSON.parse(data);
              if (chunk.usage) usage = chunk.usage;
              const delta: string = chunk.choices?.[0]?.delta?.content ?? "";
              if (delta) {
                if (!first) first = performance.now();
                setMessages((m) => {
                  const next = m.slice();
                  next[next.length - 1] = { role: "assistant", content: next[next.length - 1].content + delta };
                  return next;
                });
              }
            } catch {
              // Ignore a malformed event rather than abandoning the stream.
            }
          }
        }
      }
      const end = performance.now();
      setStats({ ttftMs: first ? first - start : end - start, totalMs: end - start, prompt: usage?.prompt_tokens, completion: usage?.completion_tokens });
    } catch (e) {
      if ((e as Error).name !== "AbortError") {
        setError((e as Error).message);
        setMessages((m) => (m[m.length - 1]?.content === "" ? m.slice(0, -1) : m));
      }
    } finally {
      setStreaming(false);
      abort.current = null;
    }
  }

  if (deps.data && serving.length === 0) {
    return (
      <>
        <PageHeader title="Playground" />
        <Panel>
          <Empty title="Nothing is serving yet" action={<ButtonLink href="/app/models">Deploy a model</ButtonLink>}>
            The playground talks to your own deployments through the same API your code will use. Deploy a model and
            it appears here once it is serving.
          </Empty>
        </Panel>
      </>
    );
  }

  const tps = stats?.completion && stats.totalMs > stats.ttftMs ? (stats.completion / ((stats.totalMs - stats.ttftMs) / 1000)).toFixed(1) : null;

  return (
    <>
      <PageHeader title="Playground" description="Chat with your deployments through the same OpenAI-compatible API your code uses." />
      <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_280px]">
        {/* The Panel body is the flex column: messages grow, the input stays at the bottom. */}
        <Panel flush bodyClassName="flex h-[min(640px,calc(100vh-220px))] min-h-[420px] flex-col">
          <div className="min-h-0 flex-1 overflow-y-auto px-5 py-5">
            {messages.length === 0 ? (
              <p className="text-muted">Send a message to {target || "your deployment"}.</p>
            ) : (
              <div className="grid gap-5">
                {messages.map((m, i) => (
                  <div key={i} className={cx("max-w-[80ch]", m.role === "user" && "justify-self-end")}>
                    <div className="mb-1 text-[12px] text-muted">{m.role === "user" ? "You" : target}</div>
                    <div
                      className={cx(
                        "whitespace-pre-wrap rounded-xl px-4 py-3 text-[15px] leading-relaxed",
                        m.role === "user" ? "bg-nil text-on-nil" : "bg-surface-2",
                      )}
                    >
                      {m.content || (streaming && i === messages.length - 1 ? <span className="text-muted dot-live">Thinking…</span> : "")}
                    </div>
                  </div>
                ))}
              </div>
            )}
            <div ref={bottom} />
          </div>
          {error && <Notice tone="error" className="mx-5 mb-3">{error}</Notice>}
          <form
            className="flex gap-2 border-t border-line p-4"
            onSubmit={(e) => {
              e.preventDefault();
              send();
            }}
          >
            <Input value={input} onChange={(e) => setInput(e.target.value)} placeholder="Message" aria-label="Message" disabled={!target} />
            {streaming ? (
              <Button type="button" variant="secondary" onClick={() => abort.current?.abort()} aria-label="Stop generating">
                <Square size={14} /> Stop
              </Button>
            ) : (
              <Button type="submit" disabled={!input.trim() || !target} aria-label="Send">
                <Send size={14} /> Send
              </Button>
            )}
          </form>
        </Panel>

        <div className="grid content-start gap-6">
          <Panel title="Settings">
            <div className="grid gap-4">
              <Field label="Deployment" htmlFor="target">
                <Select id="target" value={target} onChange={(e) => setTarget(e.target.value)}>
                  {serving.map((d) => (
                    <option key={d.id} value={d.name}>
                      {d.name}
                    </option>
                  ))}
                </Select>
              </Field>
              <Field label="System prompt" htmlFor="system">
                <textarea
                  id="system"
                  rows={3}
                  value={system}
                  onChange={(e) => setSystem(e.target.value)}
                  className="w-full rounded-lg border border-line bg-surface px-3 py-2 text-[14px] focus:border-nil focus:outline-none"
                />
              </Field>
              <Field label={`Temperature: ${temperature.toFixed(1)}`} htmlFor="temp">
                <input id="temp" type="range" min={0} max={2} step={0.1} value={temperature} onChange={(e) => setTemperature(+e.target.value)} className="accent-[var(--nil)]" />
              </Field>
              <Field label="Max tokens" htmlFor="max">
                <Input id="max" type="number" min={1} max={8192} value={maxTokens} onChange={(e) => setMaxTokens(Math.max(1, +e.target.value || 1))} />
              </Field>
              <Button variant="ghost" onClick={() => setMessages([])} disabled={streaming || !messages.length}>
                Clear conversation
              </Button>
            </div>
          </Panel>
          {stats && (
            <Panel title="Last response">
              <dl className="grid grid-cols-2 gap-3 text-[14px]">
                <dt className="text-muted">First token</dt>
                <dd>{Math.round(stats.ttftMs)} ms</dd>
                <dt className="text-muted">Total</dt>
                <dd>{(stats.totalMs / 1000).toFixed(2)} s</dd>
                {tps && (
                  <>
                    <dt className="text-muted">Speed</dt>
                    <dd>{tps} tokens/s</dd>
                  </>
                )}
                {stats.prompt != null && (
                  <>
                    <dt className="text-muted">Tokens</dt>
                    <dd>
                      {stats.prompt} in, {stats.completion} out
                    </dd>
                  </>
                )}
              </dl>
            </Panel>
          )}
        </div>
      </div>
    </>
  );
}

export default function PlaygroundPage() {
  return (
    <Suspense>
      <Playground />
    </Suspense>
  );
}
