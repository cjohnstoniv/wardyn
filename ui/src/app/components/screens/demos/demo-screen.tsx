/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// DemoScreen (/demos) — hands-on demo sandboxes. Each card launches an
// interactive, workspace-free, LLM-free CC1 sandbox via the existing
// POST /api/v1/runs (interactive + inline_policy), embeds the same AttachTerminal
// + LiveApprovals the import Record step uses, and lets a brand-new user PROVE
// Wardyn's egress confinement before onboarding any repo or key. Pure
// composition — no backend changes. The keyless demos are gated on barrierReady
// ONLY (never llmReady, never workspaces) — they run the sandbox, not an agent,
// so no model is needed. The one needsModel demo is additionally hidden until
// llmReady (and its Start stays gated even if a caller renders it anyway).
//
// Reusable pieces (also consumed by the Getting-Started per-demo wizard steps in
// setup/demos-step.tsx): the `useDemoRuns` hook (launch/poll/store), the
// `DemoRunControls` run UI, and `StepList`.
import * as React from "react";
import { Link } from "react-router-dom";
import { Loader2, Play, ScrollText, ShieldAlert, Square, TriangleAlert } from "lucide-react";
import { toast } from "sonner";
import { runs as api } from "../../../lib/api/runs";
import { setup as setupApi } from "../../../lib/api/setup";
import { audit, egressFromAudit } from "../../../lib/api/audit";
import { getErrorMessage, relativeTime } from "../../../lib/format";
import { lsGet, lsSet } from "../../../lib/storage";
import { usePoll } from "../../../lib/use-poll";
import { isTerminalRunState, type AuditEvent, type RunState, type SetupStatus } from "../../../lib/types";
import { deriveReadiness } from "../onboarding/intro";
import { AttachTerminal } from "../../attach-terminal";
import { LiveApprovals } from "../../wardyn/live-approvals";
import { CopyPill } from "../import-workspace/record-pane";
import { Button } from "../../ui/button";
import { Chip } from "../../wardyn/primitives";
import { DEMOS, markDemoLaunched, type Demo, type DemoStep } from "./demo-catalog";

// Resume seam: {demoId: runId} of demos the operator started, so a page reload
// re-attaches to a still-RUNNING sandbox instead of orphaning it.
const STORE_KEY = "wardyn-demo-runs";
function loadStore(): Record<string, string> {
  try {
    const parsed = JSON.parse(lsGet(STORE_KEY) ?? "{}");
    return parsed && typeof parsed === "object" ? (parsed as Record<string, string>) : {};
  } catch {
    return {};
  }
}
function saveStore(map: Record<string, string>): void {
  lsSet(STORE_KEY, Object.keys(map).length ? JSON.stringify(map) : null);
}
function forgetStored(demoId: string): void {
  const store = loadStore();
  if (demoId in store) {
    delete store[demoId];
    saveStore(store);
  }
}

type TrackedRun = { id: string; state: RunState };

// useDemoRuns — owns the live-run map, the reload re-attach, the poll, and
// start/end for the demo sandboxes. Shared by the /demos grid and the
// Getting-Started per-demo steps. `onStarted(demoId)` fires when a demo is
// successfully launched (the wizard step marks itself done + records the launch).
export function useDemoRuns(onStarted?: (demoId: string) => void) {
  const [runs, setRuns] = React.useState<Record<string, TrackedRun>>({});
  const [starting, setStarting] = React.useState<string | null>(null);

  // The poll reads the latest tracked runs through a ref (usePoll fires on a
  // timer, long after the render that created its closure).
  const runsRef = React.useRef(runs);
  React.useEffect(() => {
    runsRef.current = runs;
  }, [runs]);

  // Mount: re-attach any still-RUNNING demo from a prior visit.
  React.useEffect(() => {
    let active = true;
    const stored = loadStore();
    Promise.all(
      Object.entries(stored).map(async ([demoId, runId]) => {
        const r = await api.getRun(runId).catch(() => undefined);
        return [demoId, r] as const;
      }),
    ).then((results) => {
      if (!active) return;
      const next: Record<string, TrackedRun> = {};
      const keep: Record<string, string> = {};
      for (const [demoId, r] of results) {
        if (r && !isTerminalRunState(r.state)) {
          next[demoId] = { id: r.id, state: r.state };
          keep[demoId] = r.id;
        }
      }
      setRuns(next);
      saveStore(keep);
    });

    return () => {
      active = false;
    };
  }, []);

  // Poll tracked, not-yet-settled runs until RUNNING (and clear on a terminal state).
  const anyPending = Object.values(runs).some((r) => !isTerminalRunState(r.state));
  const refresh = React.useCallback(async () => {
    for (const [demoId, tracked] of Object.entries(runsRef.current)) {
      if (isTerminalRunState(tracked.state)) continue;
      const fresh = await api.getRun(tracked.id).catch(() => undefined);
      if (!fresh) {
        setRuns((m) => {
          const n = { ...m };
          delete n[demoId];
          return n;
        });
        forgetStored(demoId);
        continue;
      }
      setRuns((m) => ({ ...m, [demoId]: { id: fresh.id, state: fresh.state } }));
      if (isTerminalRunState(fresh.state)) {
        forgetStored(demoId); // never re-attach a dead run on reload
        // Keep a FAILED run in view so the operator sees it + the run link; drop the rest.
        if (fresh.state !== "FAILED") {
          setRuns((m) => {
            const n = { ...m };
            delete n[demoId];
            return n;
          });
        }
      }
    }
  }, []);
  usePoll(refresh, 2000, !anyPending);

  const start = React.useCallback(
    async (demo: Demo) => {
      setStarting(demo.id);
      try {
        // Every demo comes up idle for the operator to drive in the attached
        // terminal — keyless demos run plain curl; the harness demo runs `claude`
        // (its policy grants Anthropic egress, and the connected model is injected
        // proxy-side). Same interactive shape, so "watch it live" is always honest.
        const run = await api.createRun({
          agent: "claude-code",
          interactive: true,
          inline_policy: demo.policy,
        });
        setRuns((m) => ({ ...m, [demo.id]: { id: run.id, state: run.state } }));
        const store = loadStore();
        store[demo.id] = run.id;
        saveStore(store);
        markDemoLaunched(demo.id); // durable per-demo "was launched" signal
        onStarted?.(demo.id);
      } catch (e) {
        toast.error("Couldn't start the demo", { description: getErrorMessage(e) });
      } finally {
        setStarting(null);
      }
    },
    [onStarted],
  );

  const end = React.useCallback(async (demo: Demo, runId: string) => {
    try {
      await api.killRun(runId);
    } catch {
      /* best-effort — a terminal run 409s, which is fine here */
    }
    setRuns((m) => {
      const n = { ...m };
      delete n[demo.id];
      return n;
    });
    forgetStored(demo.id);
  }, []);

  return { runs, starting, start, end };
}

export function DemoScreen() {
  const [status, setStatus] = React.useState<SetupStatus | null>(null);
  const [loading, setLoading] = React.useState(true);

  // Mount: readiness probe (the barrier gate). The runner owns its own re-attach.
  React.useEffect(() => {
    let active = true;
    setupApi
      .getSetupStatus()
      .then((s) => active && setStatus(s))
      .catch(() => {
        /* leave readiness unknown — the gate defaults closed */
      })
      .finally(() => active && setLoading(false));
    return () => {
      active = false;
    };
  }, []);

  const readiness = status ? deriveReadiness(status) : null;
  const barrierReady = readiness?.barrierReady ?? false;
  const llmReady = readiness?.llmReady ?? false;
  // Keyless demos always show; the harness demo only once a model is connected.
  const visibleDemos = DEMOS.filter((d) => !d.needsModel || llmReady);

  return (
    <div className="mx-auto w-full max-w-[900px] px-6 py-8">
      <header className="mb-6">
        <h1>Demo sandboxes</h1>
        <p className="mt-1 text-muted-foreground">
          Prove Wardyn's confinement hands-on — a throwaway sandbox with no repo or workspace. Start
          one, run the commands in the attached terminal, and watch the policy hold. The keyless
          demos need no model; the agent demo (shown once you connect one) runs a real Claude Code
          agent under the same policy, its model injected proxy-side.
        </p>
      </header>

      {!loading && !barrierReady && (
        <div
          className="mb-6 flex items-start gap-2 rounded-xl border border-warning/30 bg-warning-subtle px-4 py-3 text-sm text-warning"
          data-testid="demos-not-ready"
        >
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          <p>
            Demos need the sandbox runner — finish the{" "}
            <Link to="/setup" className="font-medium underline underline-offset-2">
              Environment step
            </Link>{" "}
            first.
          </p>
        </div>
      )}

      <DemoRunner barrierReady={barrierReady} llmReady={llmReady} loading={loading} demos={visibleDemos} />
    </div>
  );
}

// DemoRunner — the /demos grid: renders a DemoCard per demo, driven by the shared
// hook. `demos` defaults to the full catalog (override to render a subset).
export function DemoRunner({
  barrierReady,
  llmReady = true,
  loading = false,
  onStarted,
  demos = DEMOS,
}: {
  barrierReady: boolean;
  /** Optional — callers that only ever pass keyless demos (e.g. the
   *  Getting-Started per-demo step) can omit it; it's a no-op for them since
   *  the Start gate below only consults it when demo.needsModel is set. */
  llmReady?: boolean;
  loading?: boolean;
  onStarted?: (demoId: string) => void;
  demos?: Demo[];
}) {
  const { runs, starting, start, end } = useDemoRuns(onStarted);
  return (
    <div className="space-y-4">
      {demos.map((demo) => (
        <DemoCard
          key={demo.id}
          demo={demo}
          run={runs[demo.id]}
          starting={starting === demo.id}
          barrierReady={barrierReady}
          llmReady={llmReady}
          loading={loading}
          onStart={() => start(demo)}
          onEnd={(runId) => end(demo, runId)}
        />
      ))}
    </div>
  );
}

function DemoCard({
  demo,
  run,
  starting,
  barrierReady,
  llmReady,
  loading,
  onStart,
  onEnd,
}: {
  demo: Demo;
  run?: TrackedRun;
  starting: boolean;
  barrierReady: boolean;
  llmReady: boolean;
  loading: boolean;
  onStart: () => void;
  onEnd: (runId: string) => void;
}) {
  const running = run?.state === "RUNNING";

  return (
    <div className="rounded-xl border border-border p-4" data-testid={`demo-card-${demo.id}`}>
      <div className="flex flex-wrap items-baseline gap-2">
        <h2 className="text-lg font-semibold text-foreground">{demo.title}</h2>
        {running && (
          <Chip tone="success" dot pulse className="ml-auto">
            Live
          </Chip>
        )}
      </div>
      <p className="mt-1 text-sm text-muted-foreground">{demo.teaches}</p>

      {demo.caution && <DemoCaution text={demo.caution} />}

      <StepList steps={demo.steps} />

      <DemoRunControls
        demo={demo}
        run={run}
        starting={starting}
        barrierReady={barrierReady}
        llmReady={llmReady}
        loading={loading}
        onStart={onStart}
        onEnd={onEnd}
      />
    </div>
  );
}

// DemoCaution — the honest CC1-open-egress danger note (demo 4).
export function DemoCaution({ text }: { text: string }) {
  return (
    <div
      className="mt-3 flex items-start gap-2 rounded-lg border border-danger/40 bg-danger-subtle px-3 py-2.5 text-xs text-danger"
      data-testid="demo-caution"
    >
      <ShieldAlert className="mt-0.5 size-4 shrink-0" />
      <p className="leading-snug">{text}</p>
    </div>
  );
}

// DemoRunControls — the Start / pending / running-terminal / failed block. Shared
// by the /demos DemoCard and the Getting-Started per-demo step.
export function DemoRunControls({
  demo,
  run,
  starting,
  barrierReady,
  llmReady = true,
  loading,
  onStart,
  onEnd,
}: {
  demo: Demo;
  run?: TrackedRun;
  starting: boolean;
  barrierReady: boolean;
  /** Optional (defaults true) — irrelevant unless demo.needsModel is set; see
   *  DemoRunner's llmReady prop. */
  llmReady?: boolean;
  loading: boolean;
  onStart: () => void;
  onEnd: (runId: string) => void;
}) {
  const running = run?.state === "RUNNING";
  const failed = run?.state === "FAILED";
  const pending = !!run && !running && !failed && !isTerminalRunState(run.state);

  if (running && run) {
    return (
      <div className="mt-4 space-y-2">
        <AttachTerminal runId={run.id} />
        <LiveApprovals
          runId={run.id}
          idleHint="Off-policy egress you trigger surfaces here to approve or deny, live."
        />
        <DemoAuditPanel runId={run.id} />
        <Button size="sm" variant="outline" onClick={() => onEnd(run.id)}>
          <Square className="size-3.5" /> End demo
        </Button>
      </div>
    );
  }
  if (pending && run) {
    return (
      <div className="mt-4 flex items-center gap-2 text-sm text-muted-foreground" data-testid="demo-starting">
        <Loader2 className="size-4 animate-spin" />
        Starting the sandbox — first time may pull an image…
        <Button size="sm" variant="ghost" onClick={() => onEnd(run.id)}>
          Cancel
        </Button>
      </div>
    );
  }
  return (
    <div className="mt-4 space-y-2">
      {failed && run && (
        <div
          className="flex items-start gap-2 rounded-lg border border-danger/40 bg-danger-subtle px-3 py-2.5 text-xs text-danger"
          data-testid="demo-failed"
        >
          <ShieldAlert className="mt-0.5 size-4 shrink-0" />
          <p>
            The sandbox failed to start.{" "}
            <Link to={`/runs/${run.id}`} className="font-medium underline underline-offset-2">
              View run details
            </Link>
            .
          </p>
        </div>
      )}
      <Button
        onClick={onStart}
        disabled={!barrierReady || (demo.needsModel && !llmReady) || loading || starting}
        data-testid={`demo-start-${demo.id}`}
      >
        {starting ? <Loader2 className="size-4 animate-spin" /> : <Play className="size-4" />}
        {failed ? "Start again" : "Start demo"}
      </Button>
    </div>
  );
}

// DemoAuditPanel — the run's egress decisions, inline and live, so the operator
// sees a denial land on the record WITHOUT leaving the demo for the Audit screen.
// Projects egress.allow/deny/pending audit rows via egressFromAudit; polls /audit
// every 2s while mounted (i.e. while the demo sandbox is running).
export function DemoAuditPanel({ runId }: { runId: string }) {
  const [events, setEvents] = React.useState<AuditEvent[]>([]);
  const refresh = React.useCallback(async () => {
    const list = await audit.listAudit(runId).catch(() => null);
    if (list) setEvents(list);
  }, [runId]);
  React.useEffect(() => {
    void refresh();
  }, [refresh]);
  usePoll(refresh, 2000, false);

  // Newest first, so a just-triggered denial lands at the top.
  const decisions = egressFromAudit(events)
    .slice()
    .sort((a, b) => b.time.localeCompare(a.time));

  return (
    <div className="rounded-lg border border-border bg-surface-2/40 p-3" data-testid="demo-audit-panel">
      <div className="mb-2 flex items-center gap-2">
        <ScrollText className="size-4 shrink-0 text-primary" />
        <span className="text-sm font-medium text-foreground">Audit — egress decisions</span>
        <Chip tone="success" dot pulse className="ml-auto">
          live
        </Chip>
      </div>
      {decisions.length === 0 ? (
        <p className="text-xs text-muted-foreground">
          No egress decisions yet — run a command in the terminal above and each allow/deny lands
          here, on the record.
        </p>
      ) : (
        <ul className="space-y-1" data-testid="demo-audit-rows">
          {decisions.map((d) => (
            <li key={d.id} className="flex items-center gap-2 text-xs">
              <Chip
                tone={d.decision === "deny" ? "danger" : d.decision === "allow" ? "success" : "warning"}
                dot
              >
                {d.decision}
              </Chip>
              <span className="min-w-0 truncate font-mono text-foreground">{d.domain}</span>
              <span className="ml-auto shrink-0 tabular-nums text-muted-foreground">
                {relativeTime(d.time)}
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// Numbered instructions — a copy pill for the command steps, the explanation for all.
export function StepList({ steps }: { steps: DemoStep[] }) {
  return (
    <ol className="mt-3 space-y-2" data-testid="demo-steps">
      {steps.map((s, i) => (
        <li key={i} className="flex gap-2 text-sm text-muted-foreground">
          <span className="mt-0.5 flex size-5 shrink-0 items-center justify-center rounded-full bg-muted text-[11px] font-medium text-foreground">
            {i + 1}
          </span>
          <div className="min-w-0 space-y-1">
            {s.cmd && <CopyPill text={s.cmd} />}
            <p className="leading-snug">{s.text}</p>
          </div>
        </li>
      ))}
    </ol>
  );
}
