/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The demo RUNNER — the reusable machinery behind a hands-on demo sandbox:
// launch/poll/re-attach (`useDemoRuns`), the Start / starting / live-terminal /
// terminated controls (`DemoRunControls`), the inline decisions panel
// (`DemoAuditPanel`), the numbered command walkthrough (`StepList`) and the
// open-egress danger note (`DemoCaution`). Each demo runs an interactive,
// workspace-free CC1 sandbox via the existing POST /api/v1/runs (interactive +
// inline_policy) and embeds the same AttachTerminal + LiveApprovals the import
// Record step uses, so a brand-new user can PROVE Wardyn's confinement before
// onboarding any repo or key. Pure composition — no backend changes.
//
// This was demo-screen.tsx, the /demos page. That page is gone (App.tsx
// redirects /demos into the funnel): Getting Started is the ONE demos surface,
// and setup/demos-step.tsx's DemoDetail is the single renderer these pieces
// compose into. What lived here purely to draw the grid — DemoScreen, its
// DemoRunner list and the per-demo DemoCard — went with the route; DemoDetail
// carries the `demo-card-<id>` testid the cards used to own.
//
// Gating: the keyless demos need barrierReady ONLY (never llmReady, never
// workspaces) — they run the sandbox, not an agent. The conditional ones
// (needsModel / needsSecret) are dropped from the funnel walk until their
// precondition is met (setup/steps.ts's stepOrder), and that IS their gate.
// `needsGitHubApp` is the THIRD shape and deliberately not a fourth drop: that
// card exists to teach a lane nothing local can fake, and a dropped card
// teaches nobody, so it keeps its place in the rail with a DISABLED Start —
// the same shape !barrierReady already renders.
import * as React from "react";
import { Link } from "react-router-dom";
import { Loader2, Play, ScrollText, ShieldAlert, Sparkles, Square } from "lucide-react";
import { toast } from "sonner";
import { runs as api } from "../../../lib/api/runs";
import { HttpError } from "../../../lib/api/core";
import { audit, demoAuditRows, egressFromAudit, type DemoAuditRow } from "../../../lib/api/audit";
import { getErrorMessage, relativeTime } from "../../../lib/format";
import { lsGet, lsSet } from "../../../lib/storage";
import { usePoll } from "../../../lib/use-poll";
import { isTerminalRunState, type AuditEvent, type RunState } from "../../../lib/types";
import { AttachTerminal } from "../../attach-terminal";
import { LiveApprovals } from "../../wardyn/live-approvals";
import { CopyPill } from "../workspace-detail/record-pane";
import { Button } from "../../ui/button";
import { Chip } from "../../wardyn/primitives";
import { markDemoLaunched, type Demo, type DemoStep } from "./demo-catalog";

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
// start/end for the demo sandboxes. Keyed by demo id (not scoped to one demo)
// so the resume seam still re-attaches a sandbox started from a DIFFERENT
// demo's step in this browser. `onStarted(demoId)` fires when a demo is
// successfully launched (the wizard step marks itself done + records the launch).
export function useDemoRuns(onStarted?: (demoId: string) => void) {
  const [runs, setRuns] = React.useState<Record<string, TrackedRun>>({});
  const [starting, setStarting] = React.useState<string | null>(null);
  // {demoId: message} for a run-create that was REFUSED — rendered on the card
  // (see DemoRunControls' demo-create-refused block), cleared by starting again.
  const [createErrors, setCreateErrors] = React.useState<Record<string, string>>({});

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
      // Never re-attach a dead run on reload — but keep it tracked in memory
      // for THIS session regardless of outcome (not just FAILED): a
      // terminated card still offers "Turn this into a policy" against
      // whatever the sandbox actually did. Starting again is what forgets it
      // (start() overwrites this entry with the new run).
      if (isTerminalRunState(fresh.state)) forgetStored(demoId);
    }
  }, []);
  usePoll(refresh, 2000, !anyPending);

  const start = React.useCallback(
    async (demo: Demo) => {
      setStarting(demo.id);
      setCreateErrors((m) => {
        if (!(demo.id in m)) return m;
        const n = { ...m };
        delete n[demo.id];
        return n;
      });
      try {
        // Every demo comes up idle for the operator to drive in the attached
        // terminal — keyless demos run plain curl; the harness demo runs `claude`
        // (its policy grants Anthropic egress, and the connected model is injected
        // proxy-side). Same interactive shape, so "watch it live" is always honest.
        //
        // W3-S1-1: the keyless demos' cards promise "no allowed
        // destinations / no key" — without task_mode="exec" the server still
        // folds the operator's site-wide model integration onto ANY run
        // (foldRunIntegration only skips it for task_mode=exec; see
        // internal/api/llmcred.go), silently widening egress to
        // api.anthropic.com and injecting a live key proxy-side. Only the
        // harness demo (needsModel) actually wants a model call.
        const run = await api.createRun({
          agent: "claude-code",
          interactive: true,
          inline_policy: demo.policy,
          task_mode: demo.needsModel ? undefined : "exec",
        });
        setRuns((m) => ({ ...m, [demo.id]: { id: run.id, state: run.state } }));
        const store = loadStore();
        store[demo.id] = run.id;
        saveStore(store);
        markDemoLaunched(demo.id); // durable per-demo "was launched" signal
        onStarted?.(demo.id);
      } catch (e) {
        // Card-anchored, not a toast. A create refusal is a POLICY DECISION for
        // at least one demo (sts-fail-closed's whole lesson is the 422 that
        // fires before any sandbox exists), and a decision the operator is
        // meant to READ cannot live in something that scrolls away — the take
        // and the e2e both need a stable place to point at.
        setCreateErrors((m) => ({ ...m, [demo.id]: getErrorMessage(e) }));
        // …and an EXPECTED 422 earns the demo its checkmark — but ONLY for the
        // card whose lesson IS the refusal (refusalCompletes). Every granted
        // secrets card is CC3-floored, so on a Fence-only host they all 422 with
        // "cannot enforce CC3"; that must render the error, never a false green.
        if (demo.refusalCompletes && e instanceof HttpError && e.status === 422) {
          markDemoLaunched(demo.id);
          onStarted?.(demo.id);
        }
      } finally {
        setStarting(null);
      }
    },
    [onStarted],
  );

  const end = React.useCallback(async (demo: Demo, runId: string) => {
    try {
      await api.killRun(runId);
    } catch (e) {
      // A 409 because the run is already terminal is a benign race (it ended
      // on its own between the operator's click and this request) — safe to
      // forget. Any OTHER failure — including the OTHER 409 shape, "run state
      // changed concurrently; not overwriting with KILLED", which means this
      // kill LOST a race and did NOT land — leaves the sandbox live
      // server-side. Forgetting it here would orphan a run the operator
      // believes was cancelled, so keep it tracked (and in localStorage) and
      // surface the failure instead.
      const alreadyTerminal = e instanceof HttpError && e.status === 409 && /already terminal/i.test(e.message);
      if (!alreadyTerminal) {
        toast.error("Couldn't end the demo", { description: getErrorMessage(e) });
        return;
      }
    }
    // Deliberately NOT dropped from `runs` here — the next poll tick picks up
    // the real terminal state (KILLED), and the card stays tracked so "Turn
    // this into a policy" has a runId to work from. Only starting again
    // forgets it (start() overwrites this entry).
    forgetStored(demo.id);
  }, []);

  return { runs, starting, start, end, createErrors };
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

// DemoRunControls — the Start / pending / running-terminal / failed block, and
// the "Turn this into a policy" payoff a terminated run offers (the SAME
// runId-driven ProfileReview sheet workspace-detail.tsx mounts for a recorded
// session — POST /runs/{id}/profile synthesizes a least-privilege policy from
// whatever the sandbox actually did). Without `onTurnIntoPolicy` that button
// simply doesn't render, so its owner is whoever can host the sheet.
export function DemoRunControls({
  demo,
  run,
  starting,
  barrierReady,
  githubAppReady = true,
  createError,
  loading,
  onStart,
  onEnd,
  onTurnIntoPolicy,
}: {
  demo: Demo;
  run?: TrackedRun;
  starting: boolean;
  barrierReady: boolean;
  /** Whether a GitHub App is configured (SetupStatus.secrets.github_app). Only
   *  a `needsGitHubApp` demo reads it. Defaults TRUE — fail OPEN, the same
   *  rationale operator-context documents: a status that hasn't loaded, or a
   *  component mounted without it, must never invent a gate. */
  githubAppReady?: boolean;
  /** Message from a REFUSED run-create, rendered on the card. */
  createError?: string;
  loading: boolean;
  onStart: () => void;
  onEnd: (runId: string) => void;
  onTurnIntoPolicy?: (runId: string) => void;
}) {
  // The teach-and-gate lane: the card stays, its Start does not open.
  const appGated = !!demo.needsGitHubApp && !githubAppReady;
  const startClosed = !barrierReady || appGated || loading || starting;
  const running = run?.state === "RUNNING";
  const failed = run?.state === "FAILED";
  const pending = !!run && !running && !failed && !isTerminalRunState(run.state);
  // Any OTHER terminal outcome (COMPLETED/STOPPED/KILLED/ARCHIVED) — the run
  // stays tracked (useDemoRuns keeps it, not just FAILED) so its own audit
  // trail can be replayed into a policy, same as a recorded workspace session.
  const terminated = !!run && !failed && isTerminalRunState(run.state);

  if (running && run) {
    return (
      <div className="mt-4 space-y-2">
        {/* Shorter than the 70vh default ON PURPOSE: a demo's whole story is
            the terminal AND the approvals strip under it — a command hangs, a
            row appears, a human decides, the command resumes. At 70vh the strip
            lives below the fold, so the decision happens off screen and the
            resume looks like magic. 42vh keeps both halves in one 1080p frame. */}
        {/* 36vh (not 42) so the policy above, the terminal, and the audit
            panel below all fit one 1080p frame while a demo runs — the cockpit
            view. A few command lines read fine at this height. */}
        <AttachTerminal runId={run.id} heightClass="h-[36vh]" />
        <LiveApprovals
          runId={run.id}
          idleHint="Off-policy egress you trigger surfaces here to approve or deny, live."
          // Demo sandboxes are workspace-free by construction (see this
          // file's own header comment) — Always renders disabled, and that
          // disabled state IS the demo's lesson (the "once-or-for-good" card
          // teaches it explicitly).
          hasWorkspace={false}
        />
        <DemoAuditPanel runId={run.id} section={demo.section} />
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
  if (terminated && run) {
    return (
      <div className="mt-4 flex flex-wrap items-center gap-2" data-testid="demo-terminated">
        <p className="text-sm text-muted-foreground">
          Demo ended — its record is still here.
        </p>
        <div className="ml-auto flex items-center gap-2">
          {onTurnIntoPolicy && (
            <Button
              size="sm"
              variant="outline"
              onClick={() => onTurnIntoPolicy(run.id)}
              data-testid={`demo-turn-into-policy-${demo.id}`}
            >
              <Sparkles className="size-3.5" /> Turn this into a policy
            </Button>
          )}
          <Button size="sm" variant="ghost" onClick={onStart} disabled={startClosed}>
            {starting ? <Loader2 className="size-4 animate-spin" /> : <Play className="size-4" />}
            Start again
          </Button>
        </div>
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
      {createError && (
        <div
          className="flex items-start gap-2 rounded-lg border border-danger/40 bg-danger-subtle px-3 py-2.5 text-xs text-danger"
          data-testid="demo-create-refused"
        >
          <ShieldAlert className="mt-0.5 size-4 shrink-0" />
          <p className="min-w-0 leading-snug">
            Wardyn refused to create this run — nothing started, so there is nothing to clean up.{" "}
            <span className="break-words font-mono">{createError}</span>
          </p>
        </div>
      )}
      <Button
        onClick={onStart}
        disabled={startClosed}
        data-testid={`demo-start-${demo.id}`}
      >
        {starting ? <Loader2 className="size-4 animate-spin" /> : <Play className="size-4" />}
        {failed ? "Start again" : "Start demo"}
      </Button>
    </div>
  );
}

// DemoAuditPanel — the run's decisions, inline and live, so the operator sees
// one land on the record WITHOUT leaving the demo for the Audit screen. Polls
// /audit every 2s while mounted (i.e. while the demo sandbox is running).
//
// Egress-section demos keep the ORIGINAL egress-only projection/look
// (egressFromAudit) untouched. Secrets-section demos widen it via
// demoAuditRows: an api_key grant's mint/injection lands as secret.read /
// credential.mint rows (outcome + secret/grant target, no domain — they don't
// fit EgressDecision at all), so those demos need the credential-shaped row
// alongside any real egress decision on the same timeline.
export function DemoAuditPanel({ runId, section }: { runId: string; section?: Demo["section"] }) {
  const [events, setEvents] = React.useState<AuditEvent[]>([]);
  const refresh = React.useCallback(async () => {
    const list = await audit.listAudit(runId).catch(() => null);
    if (list) setEvents(list);
  }, [runId]);
  React.useEffect(() => {
    void refresh();
  }, [refresh]);
  usePoll(refresh, 2000, false);

  const rows: DemoAuditRow[] =
    section === "secrets"
      ? demoAuditRows(events)
      : egressFromAudit(events).map((d) => ({ kind: "egress" as const, ...d }));
  // Newest first, so a just-triggered decision lands at the top.
  const sorted = rows.slice().sort((a, b) => b.time.localeCompare(a.time));

  return (
    <div className="rounded-lg border border-border bg-surface-2/40 p-3" data-testid="demo-audit-panel">
      <div className="mb-2 flex items-center gap-2">
        <ScrollText className="size-4 shrink-0 text-primary" />
        <span className="text-sm font-medium text-foreground">
          {section === "secrets" ? "Audit — decisions" : "Audit — egress decisions"}
        </span>
        <Chip tone="success" dot pulse className="ml-auto">
          live
        </Chip>
      </div>
      {sorted.length === 0 ? (
        <p className="text-xs text-muted-foreground">
          {section === "secrets"
            ? "No decisions yet — run a command in the terminal above and each one lands here, on the record."
            : "No egress decisions yet — run a command in the terminal above and each allow/deny lands here, on the record."}
        </p>
      ) : (
        <ul className="space-y-1" data-testid="demo-audit-rows">
          {sorted.map((d) =>
            d.kind === "credential" ? (
              <li key={d.id} className="flex items-center gap-2 text-xs">
                <Chip tone={d.outcome === "success" ? "success" : "danger"} dot>
                  {d.action}
                </Chip>
                <span className="min-w-0 truncate font-mono text-foreground">{d.target}</span>
                <span className="ml-auto shrink-0 tabular-nums text-muted-foreground">
                  {relativeTime(d.time)}
                </span>
              </li>
            ) : (
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
            ),
          )}
        </ul>
      )}
    </div>
  );
}

// Numbered instructions — a copy pill for the command steps, the explanation
// for all. A cmd containing the literal token "{grant_id}" (the
// authorized-not-issued demo's mint command) renders that token AS-IS in the
// pre-launch "what you'll run" preview (no runId yet) and substitutes the
// run's real grant id once a run is live — fetched via runsApi.getGrants
// (runs.ts; grants exist pre-mint, so this resolves before the operator ever
// needs to paste the command).
export function StepList({ steps, runId }: { steps: DemoStep[]; runId?: string }) {
  const needsGrantId = React.useMemo(() => steps.some((s) => s.cmd?.includes("{grant_id}")), [steps]);
  const [grantId, setGrantId] = React.useState<string | null>(null);
  React.useEffect(() => {
    if (!runId || !needsGrantId) {
      setGrantId(null);
      return;
    }
    let active = true;
    api
      .getGrants(runId)
      .then((grants) => {
        if (active && grants[0]) setGrantId(grants[0].id);
      })
      .catch(() => {
        /* leave the literal token rendered — a transient fetch failure isn't fatal here */
      });
    return () => {
      active = false;
    };
  }, [runId, needsGrantId]);

  const rendered = (cmd: string) => (grantId ? cmd.split("{grant_id}").join(grantId) : cmd);

  return (
    <ol className="mt-3 space-y-2" data-testid="demo-steps">
      {steps.map((s, i) => (
        <li key={i} className="flex gap-2 text-sm text-muted-foreground">
          <span className="mt-0.5 flex size-5 shrink-0 items-center justify-center rounded-full bg-muted text-[0.6875rem] font-medium text-foreground">
            {i + 1}
          </span>
          <div className="min-w-0 space-y-1">
            {s.cmd && <CopyPill text={rendered(s.cmd)} />}
            <p className="leading-snug">{s.text}</p>
          </div>
        </li>
      ))}
    </ol>
  );
}
