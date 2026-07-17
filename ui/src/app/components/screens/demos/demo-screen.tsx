/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// DemoScreen (/demos) — hands-on demo sandboxes. Each card launches an
// interactive, workspace-free, LLM-free CC1 sandbox via the existing
// POST /api/v1/runs (interactive + inline_policy), embeds the same AttachTerminal
// + LiveApprovals the import Record step uses, and lets a brand-new user PROVE
// Wardyn's egress confinement before onboarding any repo or key. Pure
// composition — no backend changes. Gated on barrierReady ONLY (never llmReady,
// never workspaces): a demo runs the sandbox, not an agent, so no model is needed.
import * as React from "react";
import { Link } from "react-router-dom";
import { Loader2, Play, ShieldAlert, Square, TriangleAlert } from "lucide-react";
import { toast } from "sonner";
import { runs as api } from "../../../lib/api/runs";
import { setup as setupApi } from "../../../lib/api/setup";
import { getErrorMessage } from "../../../lib/format";
import { lsGet, lsSet } from "../../../lib/storage";
import { usePoll } from "../../../lib/use-poll";
import { isTerminalRunState, type RunState, type SetupStatus } from "../../../lib/types";
import { deriveReadiness } from "../onboarding/intro";
import { AttachTerminal } from "../../attach-terminal";
import { LiveApprovals } from "../../wardyn/live-approvals";
import { CopyPill } from "../import-workspace/record-pane";
import { Button } from "../../ui/button";
import { Chip } from "../../wardyn/primitives";
import { DEMOS, type Demo, type DemoStep } from "./demo-catalog";

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

export function DemoScreen() {
  const [status, setStatus] = React.useState<SetupStatus | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [runs, setRuns] = React.useState<Record<string, TrackedRun>>({});
  const [starting, setStarting] = React.useState<string | null>(null);

  // The poll reads the latest tracked runs through a ref (usePoll fires on a
  // timer, long after the render that created its closure).
  const runsRef = React.useRef(runs);
  React.useEffect(() => {
    runsRef.current = runs;
  }, [runs]);

  const barrierReady = status ? deriveReadiness(status).barrierReady : false;

  // Mount: readiness probe (gate) + re-attach any still-RUNNING demo from a prior visit.
  React.useEffect(() => {
    let active = true;
    setupApi
      .getSetupStatus()
      .then((s) => active && setStatus(s))
      .catch(() => {
        /* leave readiness unknown — the gate defaults closed */
      })
      .finally(() => active && setLoading(false));

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

  const start = async (demo: Demo) => {
    setStarting(demo.id);
    try {
      const run = await api.createRun({
        agent: "claude-code",
        interactive: true,
        inline_policy: demo.policy,
      });
      setRuns((m) => ({ ...m, [demo.id]: { id: run.id, state: run.state } }));
      const store = loadStore();
      store[demo.id] = run.id;
      saveStore(store);
    } catch (e) {
      toast.error("Couldn't start the demo", { description: getErrorMessage(e) });
    } finally {
      setStarting(null);
    }
  };

  const end = async (demo: Demo, runId: string) => {
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
  };

  return (
    <div className="mx-auto w-full max-w-[900px] px-6 py-8">
      <header className="mb-6">
        <h1>Demo sandboxes</h1>
        <p className="mt-1 text-muted-foreground">
          Prove Wardyn's egress confinement hands-on — a throwaway sandbox with no repo, key, or
          workspace. Start one, paste the commands into the terminal, and watch the policy hold.
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

      <div className="space-y-4">
        {DEMOS.map((demo) => (
          <DemoCard
            key={demo.id}
            demo={demo}
            run={runs[demo.id]}
            starting={starting === demo.id}
            barrierReady={barrierReady}
            loading={loading}
            onStart={() => start(demo)}
            onEnd={(runId) => end(demo, runId)}
          />
        ))}
      </div>
    </div>
  );
}

function DemoCard({
  demo,
  run,
  starting,
  barrierReady,
  loading,
  onStart,
  onEnd,
}: {
  demo: Demo;
  run?: TrackedRun;
  starting: boolean;
  barrierReady: boolean;
  loading: boolean;
  onStart: () => void;
  onEnd: (runId: string) => void;
}) {
  const running = run?.state === "RUNNING";
  const failed = run?.state === "FAILED";
  const pending = !!run && !running && !failed && !isTerminalRunState(run.state);

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

      {demo.caution && (
        <div
          className="mt-3 flex items-start gap-2 rounded-lg border border-danger/40 bg-danger-subtle px-3 py-2.5 text-xs text-danger"
          data-testid="demo-caution"
        >
          <ShieldAlert className="mt-0.5 size-4 shrink-0" />
          <p className="leading-snug">{demo.caution}</p>
        </div>
      )}

      <StepList steps={demo.steps} />

      {running && run ? (
        <div className="mt-4 space-y-2">
          <AttachTerminal runId={run.id} />
          <LiveApprovals
            runId={run.id}
            idleHint="Off-policy egress you trigger surfaces here to approve or deny, live."
          />
          <Button size="sm" variant="outline" onClick={() => onEnd(run.id)}>
            <Square className="size-3.5" /> End demo
          </Button>
        </div>
      ) : pending && run ? (
        <div className="mt-4 flex items-center gap-2 text-sm text-muted-foreground" data-testid="demo-starting">
          <Loader2 className="size-4 animate-spin" />
          Starting the sandbox — first time may pull an image…
          <Button size="sm" variant="ghost" onClick={() => onEnd(run.id)}>
            Cancel
          </Button>
        </div>
      ) : (
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
            disabled={!barrierReady || loading || starting}
            data-testid={`demo-start-${demo.id}`}
          >
            {starting ? <Loader2 className="size-4 animate-spin" /> : <Play className="size-4" />}
            {failed ? "Start again" : "Start demo"}
          </Button>
        </div>
      )}
    </div>
  );
}

// Numbered instructions — a copy pill for the command steps, the explanation for all.
function StepList({ steps }: { steps: DemoStep[] }) {
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
