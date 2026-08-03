/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Pure data layer for the Getting Started funnel. Holds the frozen step
// ids/labels (e2e tests target them), the phase grouping the rail renders, and
// the honest per-step badge/done derivation the orchestrator reads. One
// badge-semantics delta from the design spec is folded in (see the workspaces
// case below). No React here by design — data/derivation only.
import type { EgressRedirect, SetupStatus, Workspace } from "../../../lib/types";
import type { ProxyTestResult } from "../../../lib/api/health";
import type { Readiness } from "../onboarding/intro";
import { DEMOS } from "../demos/demo-catalog";
import { isUsable } from "../../../lib/workspace-status";
import { T } from "../../../lib/integrations";

// ------------------------------------------------------------
// Steps — ids/labels FROZEN (e2e tests target them). The single source of truth
// for the step contract; the orchestrator imports these rather than redefining.
// ------------------------------------------------------------
// The four hands-on demos are each their own funnel sub-step under "Demos" (so
// they render as separate items in the rail). Ids mirror the demo catalog so the
// orchestrator resolves a step's demo by id. FROZEN with the rest of the contract.
export const DEMO_STEP_IDS = [
  "sealed-box",
  "fail-then-approve",
  "held-at-the-door",
  "lines-that-cant-be-crossed",
] as const;
export type DemoStepId = (typeof DEMO_STEP_IDS)[number];

// 13 -> 9 collapse: `provider` (the two-level harness picker), `host_proxy`,
// `scm_provider`, `artifact_repo`, and `credentials` are GONE — their
// configuration now lives on /integrations (see ../integrations). `integrations`
// is the one new step: an embedded, thin view of that same page, so Getting
// Started never forks a second copy of that configuration surface.
//
// 9 -> 10: `corp_network` comes BACK as its own step, right before
// `integrations` — see corp-network-step.tsx and the PHASES comment below for
// why the ORDER, not a banner, is the actual fix.
export type SetupStepId = "environment" | "corp_network" | "integrations" | DemoStepId | "workspaces" | "review" | "launch";

// demo id → title, from the catalog (single source of truth for the demo steps'
// labels + headings, so they can't drift from what the demo pages show). Scoped
// to the FROZEN four funnel demo steps — the catalog also carries the harness
// demo (agent-in-the-box, /demos-only, gated on a connected model), which is NOT
// a Getting-started step and must never enter STEP_LABEL/STEP_HEADING/STEP_ORDER.
const DEMO_TITLES = Object.fromEntries(
  DEMOS.filter((d) => (DEMO_STEP_IDS as readonly string[]).includes(d.id)).map((d) => [d.id, d.title]),
) as Record<DemoStepId, string>;

// id→label lookup — the rail and the layout footer both need it; export once
// here instead of each rebuilding the same map (F5).
export const STEP_LABEL: Record<SetupStepId, string> = {
  environment: "Environment",
  corp_network: "Corporate network",
  integrations: "Integrations",
  ...DEMO_TITLES,
  workspaces: "Workspaces",
  review: "Review",
  launch: "Launch",
};

export const STEP_HEADING: Record<SetupStepId, string> = {
  environment: "Pick your barrier",
  // Same string as the rail label — the mock's own gsScaffold title for this
  // step, not a distinct noun-phrase like the other steps get.
  corp_network: "Corporate network",
  integrations: "Connect what's outside Wardyn",
  ...DEMO_TITLES,
  workspaces: "Onboard a workspace",
  review: "Review readiness",
  launch: "Launch your first run",
};

// ------------------------------------------------------------
// Phases (redesign) — groups the FROZEN steps above for the collapsible funnel
// layout. Translated 1:1 from the design's PHASES9 onto the real ids above.
// ------------------------------------------------------------
export interface PhaseDef {
  id: string;
  label: string;
  steps: SetupStepId[];
  /** Corporate phase collapses into one group row until expanded. */
  collapsible?: boolean;
}

// Walk order: essentials → demos → your work → finish. Corporate network sits
// right after Environment, BEFORE Integrations: if this machine reaches the
// internet through a corporate proxy or an internal registry mirror, that has
// to be set up before a model provider or a git host is added, or an
// unconfigured corporate network reads as a bad credential (the ORDER is the
// fix, not a banner — see corp-network-step.tsx's T.CORP_LEDE). Integrations
// itself still carries the model/SCM-host picker; it no longer owns host proxy
// or egress redirection — those moved to Corporate network (T.EMBED_SCOPE_NOTE
// on the embedded list explains the split to anyone who visited it before).
export const PHASES: PhaseDef[] = [
  { id: "essentials", label: "Essentials", steps: ["environment", "corp_network", "integrations"] },
  { id: "demos", label: "Demos", steps: [...DEMO_STEP_IDS] },
  { id: "work", label: "Your work", steps: ["workspaces"] },
  { id: "finish", label: "Finish", steps: ["review", "launch"] },
];

export const STEP_ORDER: SetupStepId[] = PHASES.flatMap((p) => p.steps);

// First step of the phase AFTER the given phase id (or null if it's the last) —
// powers the "skip this section" control for a collapsible phase. No phase is
// collapsible in the 9-step rail today; kept for the layout that reads it.
export function nextPhaseFirstStep(phaseId: string): SetupStepId | null {
  const i = PHASES.findIndex((p) => p.id === phaseId);
  return i >= 0 ? (PHASES[i + 1]?.steps[0] ?? null) : null;
}

// Steps that render an "Optional" chip in the shell (everything outside the two
// Essentials and two Finish steps). Exported so the layout and its test share
// one list instead of each hardcoding the same membership.
export const OPTIONAL_STEPS = new Set<SetupStepId>([
  // Corporate network is NOT optional (see corpNetworkGate below):
  // proof of internet access gates Next, on the theory that everything after
  // it — a model provider, a git host — looks broken when it's really the
  // network that's blocked. Only its own honest bypasses (no_runner) move you
  // past it without proof.
  // Integrations is OPTIONAL — every category it covers (model/harness, SCM
  // host) is itself skippable; Wardyn runs with none of them connected. The
  // barrier (Environment) is the sole hard requirement.
  "integrations",
  ...DEMO_STEP_IDS,
  "workspaces",
]);

// ------------------------------------------------------------
// Honest per-step badges (B4) — reflect reality, never a false "Done".
// stepBadges/stepDone carry one design delta (see the workspaces case below).
// ------------------------------------------------------------
export type StepBadge = { text: string; tone: "success" | "warning" | "neutral" | "info" };

// Corporate network's badge/gate facts. The first three are SiteConfig-derived
// (see corp-network-step.tsx's isProxyConfigured/proxyDetected, the SAME
// helpers the step body itself renders from, so the rail can never disagree
// with it); the rest are THIS-SESSION probe/visit facts the step body reports
// upward via a callback (see CorpNetworkStep's onGateChange) — lifted here,
// rather than left as the step's own local state, because the step unmounts
// on navigation and the gate must survive leaving and re-entering it. Never
// persisted: a stale "reached" surviving a page reload would be exactly the
// false reassurance this whole feature exists to prevent.
export interface CorpNetworkState {
  proxyConfigured: boolean;
  proxyDetected: boolean;
  redirectCount: number;
  /** Last real test-proxy probe this session (T.TEST_*, never cached/inferred) — undefined until the operator runs one. */
  proxyProbe?: ProxyTestResult;
  /** The operator opened the Egress redirection tab at least once this time through the step — the explicit "I looked, there's nothing to redirect" acknowledgement corpNetworkGate requires at zero redirects. */
  egressVisited: boolean;
  /** Each configured redirect's last test result this session, keyed by its `from` (a redirect has no server id) — a missing key means untested. Stale keys from a since-removed/edited redirect are harmless: corpNetworkGate only ever looks up a `from` from the CURRENT list. */
  redirectProbes: Record<string, ProxyTestResult>;
}
const CORP_NETWORK_UNSET: CorpNetworkState = {
  proxyConfigured: false,
  proxyDetected: false,
  redirectCount: 0,
  egressVisited: false,
  redirectProbes: {},
};

// THE gate, in the mock's own ladder (wardyn-proto.js's corpGate): Next
// unlocks ONLY on a probe that returned reached — or on no_runner, where
// Wardyn is structurally incapable of collecting the proof and so does not
// get to demand it. There is no click-past. `reason` is the specific,
// actionable sentence rendered beside the button it locks (setup-layout.tsx);
// on the two states that unlock WITHOUT the full builtin proof (no_runner, a
// custom-endpoint pass) the gate is on but the reason stays, as a NEUTRAL
// standing note — the operator continues, and the weaker footing stays said.
// Also the single source of truth corpNetworkBadge and stepDone's
// corp_network line both read, so the rail badge, the checkmark, and the
// Next button can never disagree about what this step actually proved.
export interface CorpNetworkGate {
  on: boolean;
  reason?: string;
  tone?: "warning" | "neutral";
}

// "a", "a and b", "a, b and c" — the mock's own join for naming every failing
// redirect at once (its corpGate builds the same list from Mono chunks).
function listJoin(names: string[]): string {
  if (names.length <= 1) return names[0] ?? "";
  return `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
}

// Ladder (order matters, and is the mock's): no_runner bypass -> intercepted
// (a blocked flavor with its own instruction — different person to call) ->
// blocked -> untested -> the Egress tab must be OPENED once regardless of row
// count (an empty list is an answer, but only after it's been seen; rows
// derived from legacy config can exist without a visit) -> failing rows named
// all at once -> untested rows -> a custom pass unlocks with its standing note.
export function corpNetworkGate(c: CorpNetworkState, redirects: EgressRedirect[]): CorpNetworkGate {
  const p = c.proxyProbe;
  if (p?.state === "no_runner") return { on: true, reason: T.NORUNNER_NOTE, tone: "neutral" };
  if (p?.state === "blocked") {
    return { on: false, reason: p.intercepted ? T.GATE_INTERCEPTED : T.GATE_BLOCKED, tone: "warning" };
  }
  if (p?.state !== "reached") return { on: false, reason: T.GATE_UNTESTED, tone: "warning" };
  if (!c.egressVisited) return { on: false, reason: T.GATE_EGRESS_UNSEEN, tone: "warning" };
  const bad = redirects.filter((red) => {
    const s = c.redirectProbes[red.from]?.state;
    return s === "blocked" || s === "bypass";
  });
  if (bad.length > 0) {
    return {
      on: false,
      reason: `Fix ${listJoin(bad.map((red) => red.from))} above — every configured redirect must prove reached before this step hands off. Or remove the rows.`,
      tone: "warning",
    };
  }
  if (redirects.some((red) => c.redirectProbes[red.from]?.state !== "reached")) {
    return { on: false, reason: T.GATE_EGRESS_UNTESTED, tone: "warning" };
  }
  if (p.custom) return { on: true, reason: T.GATE_CUSTOM_ON, tone: "neutral" };
  return { on: true };
}

// The badge is the CONNECTIVITY FACT, not a done-ness judgment (the mock's
// corpBadge): "Reached · proxy" states what the probe proved even while the
// egress side still holds Next — the gate note and the egress tab's own dot
// carry that, and stepDone (gate.on && reached) owns the checkmark. Amber
// until proven, never green for anything unproven, counts always DERIVED.
function corpNetworkBadge(c: CorpNetworkState): StepBadge {
  const p = c.proxyProbe;
  if (p?.state === "no_runner") return { text: "Untested · no runner", tone: "neutral" };
  if (p?.state === "blocked") {
    return { text: p.intercepted ? "Blocked · intercepted" : "Blocked", tone: "warning" };
  }
  if (p?.state !== "reached") {
    if (c.proxyDetected && !c.proxyConfigured) return { text: "Detected — not configured", tone: "warning" };
    return { text: "Untested", tone: "warning" };
  }
  // A custom pass must never read as the verified builtin one: info tone, no
  // redirect arithmetic — the claim is only "an endpoint of yours answered".
  if (p.custom) return { text: "Reached · custom endpoint", tone: "info" };
  const via = p.via ?? (c.proxyConfigured ? "proxy" : "direct");
  const plus = c.redirectCount > 0 ? ` + ${c.redirectCount} redirect${c.redirectCount === 1 ? "" : "s"}` : "";
  return { text: `Reached · ${via === "direct" ? "direct" : "proxy"}${plus}`, tone: "success" };
}

export function stepBadges(
  status: SetupStatus,
  r: Readiness,
  workspaces: Workspace[],
  // Count of connected integrations (any category) — see
  // lib/api/integrations.ts's allRows(). The orchestrator owns fetching the
  // integrations data (siteConfig + secret names) this count derives from;
  // this pure function only needs the resulting number.
  integrationsCount: number,
  corpNetwork: CorpNetworkState = CORP_NETWORK_UNSET,
): Record<SetupStepId, StepBadge> {
  const readyWorkspaces = workspaces.filter((w) => isUsable(w.status)).length;
  // Each demo sub-step is a "try it" step. The pure badge stays advisory (neutral
  // "Optional"); the orchestrator upgrades a demo to a green "Done · demo run" once
  // it's been launched (a per-browser signal that doesn't belong in this pure fn).
  const demoBadges = Object.fromEntries(
    DEMO_STEP_IDS.map((id) => [id, { text: "Optional", tone: "neutral" } as StepBadge]),
  ) as Record<DemoStepId, StepBadge>;
  return {
    environment: r.barrierReady
      ? { text: `Ready · ${r.barrierCount} of 3 barriers`, tone: "success" }
      : { text: "Needs setup", tone: "warning" },
    corp_network: corpNetworkBadge(corpNetwork),
    // Ladder: Optional -> Skipped (visited, left unconfigured — applied by the
    // orchestrator's generic visited-steps override, see setup-screen.tsx) ->
    // Ready · N connected.
    integrations:
      integrationsCount > 0
        ? { text: `Ready · ${integrationsCount} connected`, tone: "success" }
        : { text: "Optional", tone: "neutral" },
    ...demoBadges,
    // Count only READY workspaces, not merely onboarded ones — a workspace stuck
    // mid-import isn't attachable to a run yet, so it earns its own honest "In
    // progress" state instead of a premature green "Ready · N onboarded".
    workspaces: readyWorkspaces
      ? {
          // Honest count: when some onboarded workspaces aren't ready yet, say so
          // ("2 of 5") instead of an undercounting "2 onboarded".
          text:
            readyWorkspaces === workspaces.length
              ? `Ready · ${readyWorkspaces} onboarded`
              : `Ready · ${readyWorkspaces} of ${workspaces.length} onboarded`,
          tone: "success",
        }
      : workspaces.length
        ? { text: "In progress", tone: "info" }
        : { text: "Optional", tone: "neutral" },
    // Review rolls up every check. It's "warning" only when a real blocker exists
    // (a failing check), else a neutral/green summary — the readiness verdict, not
    // a per-topic nag (those live on their own steps now). The one hard requirement
    // is the BARRIER (backend `ready`); a model is optional, so it never blocks the
    // "ready" verdict here (the fast-path banner still needs one — it advertises a
    // one-click run — but that's a nudge, not a gate).
    review: status.checks.some((c) => c.status === "fail")
      ? { text: "Needs attention", tone: "warning" }
      : r.ready
        ? { text: "Ready to launch", tone: "success" }
        : { text: "Set up the barrier first", tone: "neutral" },
    launch: status.has_runs
      ? { text: "First run launched", tone: "success" }
      : r.ready
        ? { text: "Ready to launch", tone: "success" }
        : { text: "Set up the barrier first", tone: "neutral" },
  };
}

export function stepDone(
  status: SetupStatus,
  r: Readiness,
  workspaces: Workspace[],
  integrationsCount: number,
  corpNetwork: CorpNetworkState = CORP_NETWORK_UNSET,
  corpNetworkRedirects: EgressRedirect[] = [],
): Record<SetupStepId, boolean> {
  // Demos: advisory here (all false). The orchestrator ORs in the per-browser
  // "launched demos" set to earn each demo's checkmark — kept out of this pure fn
  // so its signature (and every steps.test call site) stays unchanged.
  const demoDone = Object.fromEntries(DEMO_STEP_IDS.map((id) => [id, false])) as Record<
    DemoStepId,
    boolean
  >;
  return {
    // Environment = "Pick your barrier" — barrier-only, the same signal its badge
    // reads. An unrelated failing check must not blank this dot while the badge
    // stays green (Review owns the whole-checks rollup).
    environment: r.barrierReady,
    // The mock's own done rule (wardyn-proto.js): gate.on && reached. no_runner
    // UNLOCKS Next (Wardyn can't demand proof it can't collect) but never
    // earns the checkmark — nothing was proven; a custom pass does count as
    // reached (its weaker footing is carried by the badge tone and gate note).
    corp_network:
      corpNetworkGate(corpNetwork, corpNetworkRedirects).on &&
      corpNetwork.proxyProbe?.state === "reached",
    // An explicit "Skip this step" click (see setup-gate's markIntegrationsSkipped)
    // ORs in from the orchestrator, exactly like the old model-skip override —
    // this pure fn only knows about a real connected integration.
    integrations: integrationsCount > 0,
    ...demoDone,
    // Design delta: done only once a workspace is actually READY, matching the
    // badge above — merely onboarding one (still scanning/building/verifying)
    // no longer earns the stepper checkmark.
    workspaces: workspaces.some((w) => isUsable(w.status)),
    // Barrier is the only hard requirement; a model is optional (skippable), so
    // Review is done once the barrier is up and no check is failing.
    review: r.ready && !status.checks.some((c) => c.status === "fail"),
    launch: status.has_runs,
  };
}
