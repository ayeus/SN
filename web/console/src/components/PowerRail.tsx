import type { DeploymentState } from "@/lib/types";
import { cx } from "./ui";

// The deployment state machine from UML §4, drawn as a power-on sequence.
const STAGES = [
  { key: "scheduling", label: "Scheduling", hint: "Finding a GPU" },
  { key: "pulling", label: "Pulling", hint: "Downloading weights" },
  { key: "loading", label: "Loading", hint: "Into GPU memory" },
  { key: "warming", label: "Warming", hint: "First generation" },
  { key: "serving", label: "Serving", hint: "Endpoint live" },
] as const;

const ORDER: Record<string, number> = { pending: 0, scheduling: 0, pulling: 1, loading: 2, warming: 3, serving: 4, degraded: 4 };

export function PowerRail({ state, detail, compact }: { state: DeploymentState; detail?: string | null; compact?: boolean }) {
  const idx = ORDER[state];
  const halted = idx === undefined; // paused, stopping, stopped, failed
  const done = state === "serving";

  return (
    <div>
      <ol className={cx("grid grid-cols-5", compact ? "gap-1" : "gap-1.5 sm:gap-2")} aria-label="Deployment progress">
        {STAGES.map((s, i) => {
          const reached = !halted && idx !== undefined && i <= idx;
          const current = !halted && i === idx && !done;
          const status = done && reached ? "done" : current ? "current" : reached ? "done" : "todo";
          return (
            <li key={s.key} className="min-w-0" aria-current={current ? "step" : undefined}>
              <div
                className={cx(
                  "h-1.5 rounded-full",
                  status === "done" && (done ? "bg-serving" : "bg-nil"),
                  status === "current" && "bg-marigold rail-live",
                  status === "todo" && "bg-surface-2",
                  halted && "bg-surface-2",
                )}
              />
              {!compact && (
                <div className="mt-2 min-w-0">
                  <div
                    className={cx(
                      "truncate text-[11px] font-medium sm:text-[13px]",
                      status === "todo" || halted ? "text-muted" : "text-ink",
                    )}
                  >
                    {s.label}
                  </div>
                  <div className="hidden truncate text-[12px] text-muted sm:block">{s.hint}</div>
                </div>
              )}
            </li>
          );
        })}
      </ol>
      {!compact && detail && <p className="mt-3 text-[13px] text-muted">{detail}</p>}
    </div>
  );
}
