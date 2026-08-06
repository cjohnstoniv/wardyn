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
//
// 10 -> 12: the tier-1/2/3 library split gave Directories & repos and Base
// images their own steps (`sources`, `images`) alongside `workspaces` under
// "Your work" — previously just the one. Current total: 12 (3 essentials + 4
// demos + 3 your-work + 2 finish — PHASES below is the count to trust, not
// this history).
export type SetupStepId = "environment" | "corp_network" | "integrations" | DemoStepId | "sources" | "images" | "workspaces" | "review" | "launch";

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
  sources: "Directories & repos",
  images: "Base images",
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
  // Rail-label-as-heading, the corp_network precedent: these ARE the tiers'
  // names; a distinct noun-phrase would just be a synonym.
  sources: "Directories & repos",
  images: "Base images",
  workspaces: "Onboard a workspace",
  review: "Review readiness",
  launch: "Launch your first run",
};

// ------------------------------------------------------------
// Phases (redesign) — groups the FROZEN steps above for the funnel layout.
// Translated 1:1 from the design's PHASES9 onto the real ids above.
// ------------------------------------------------------------
export interface PhaseDef {
  id: string;
  label: string;
  steps: SetupStepId[];
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
  { id: "work", label: "Your work", steps: ["sources", "images", "workspaces"] },
  { id: "finish", label: "Finish", steps: ["review", "launch"] },
];

export const STEP_ORDER: SetupStepId[] = PHASES.flatMap((p) => p.steps);

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
  "sources",
  "images",
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
  /** A connectivity probe is in flight right now — the gate renders "Probe in
   *  flight" and offers nothing else while the sandbox is out. */
  probeRunning: boolean;
  /** What's typed in "Test against a URL of your own" — the gate's probe
   *  action relabels to "Test this URL" the moment this is non-empty, so the
   *  one launch point always names what it is about to try. Transient, never
   *  persisted. */
  customDraft: string;
  /** Each configured redirect's last test result this session, keyed by its `from` (a redirect has no server id) — a missing key means untested. Stale keys from a since-removed/edited redirect are harmless: corpNetworkGate only ever looks up a `from` from the CURRENT list. */
  redirectProbes: Record<string, ProxyTestResult>;
}
const CORP_NETWORK_UNSET: CorpNetworkState = {
  proxyConfigured: false,
  proxyDetected: false,
  redirectCount: 0,
  probeRunning: false,
  customDraft: "",
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
// The fix-it button the footer offers IN PLACE of a disabled Next while the
// gate is locked (the mock's corpFoot `action`): the gate row is the one
// place a probe is launched from, and it names what it will do. `kind` is
// dispatched by the orchestrator to the step's registered handlers
// (setup-screen.tsx) — the pure layer only decides WHICH action applies.
export type CorpGateActionKind = "probe" | "probe_custom" | "open_egress" | "test_redirects";

export interface CorpNetworkGate {
  on: boolean;
  /** Bold one-line state headline (T.GATE_HEAD_*), rendered above `reason`. */
  head?: string;
  reason?: string;
  tone?: "warning" | "neutral";
  action?: { label: string; kind: CorpGateActionKind };
}

// "a", "a and b", "a, b and c" — the mock's own join for naming every failing
// redirect at once (its corpGate builds the same list from Mono chunks).
function listJoin(names: string[]): string {
  if (names.length <= 1) return names[0] ?? "";
  return `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
}

// Ladder (order matters, and is the mock's): probe in flight -> no_runner
// bypass -> intercepted (a blocked flavor with its own instruction — a
// different person to call) -> blocked -> untested -> failing rows named all
// at once -> untested rows -> a custom pass unlocks with its standing note.
// What is required is the PROOF, not configuration: no rung demands a visit
// to Egress redirection — a host with no proxy and no redirects passes this
// step with one click of Test connectivity. Anything configured must still
// prove itself.
export function corpNetworkGate(c: CorpNetworkState, redirects: EgressRedirect[]): CorpNetworkGate {
  const p = c.proxyProbe;
  if (c.probeRunning) return { on: false, head: T.GATE_HEAD_RUNNING, reason: T.GATE_RUNNING, tone: "neutral" };
  if (p?.state === "no_runner") return { on: true, head: T.GATE_HEAD_NORUNNER, reason: T.NORUNNER_NOTE, tone: "neutral" };
  // Once the operator has typed a URL of their own, the gate probes THAT —
  // the one launch point relabels so it always says which endpoint it will try.
  const probe: CorpNetworkGate["action"] = c.customDraft.trim()
    ? { label: "Test this URL", kind: "probe_custom" }
    : { label: "Test connectivity", kind: "probe" };
  if (p?.state === "blocked") {
    return p.intercepted
      ? { on: false, head: T.GATE_HEAD_INTERCEPTED, reason: T.GATE_INTERCEPTED, tone: "warning", action: probe }
      : { on: false, head: T.GATE_HEAD_BLOCKED, reason: T.GATE_BLOCKED, tone: "warning", action: probe };
  }
  if (p?.state !== "reached") return { on: false, head: T.GATE_HEAD_UNTESTED, reason: T.GATE_UNTESTED, tone: "warning", action: probe };
  const bad = redirects.filter((red) => {
    const s = c.redirectProbes[red.from]?.state;
    return s === "blocked" || s === "bypass";
  });
  if (bad.length > 0) {
    return {
      on: false,
      head: T.GATE_HEAD_EGRESS_FAILING,
      reason: `Fix ${listJoin(bad.map((red) => red.from))} above — every configured redirect must prove reached before this step hands off. Or remove the rows.`,
      tone: "warning",
      action: { label: "Open Egress redirection", kind: "open_egress" },
    };
  }
  const untested = redirects.filter((red) => c.redirectProbes[red.from]?.state !== "reached");
  if (untested.length > 0) {
    return {
      on: false,
      head: T.GATE_HEAD_EGRESS_UNTESTED,
      reason: T.GATE_EGRESS_UNTESTED,
      tone: "warning",
      action: { label: untested.length > 1 ? "Test all redirects" : "Test the redirect", kind: "test_redirects" },
    };
  }
  if (p.custom) return { on: true, head: T.GATE_HEAD_CUSTOM_ON, reason: T.GATE_CUSTOM_ON, tone: "neutral" };
  return { on: true };
}

// The badge is the CONNECTIVITY FACT, not a done-ness judgment (the mock's
// corpBadge): "Reached · proxy" states what the probe proved even while the
// egress side still holds Next — the gate note and the egress tab's own dot
// carry that, and stepDone (gate.on && reached) owns the checkmark. Amber
// until proven, never green for anything unproven, counts always DERIVED.
function corpNetworkBadge(c: CorpNetworkState): StepBadge {
  const p = c.proxyProbe;
  if (c.probeRunning) return { text: "Testing…", tone: "info" };
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
  // Tier-1/2 library sizes — defaulted so pre-split call sites stand unchanged.
  sourcesCount = 0,
  imagesCount = 0,
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
    sources: sourcesCount
      ? { text: `Ready · ${sourcesCount} configured`, tone: "success" }
      : { text: "Optional", tone: "neutral" },
    images: imagesCount
      ? { text: `Ready · ${imagesCount} saved`, tone: "success" }
      : { text: "Optional", tone: "neutral" },
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
  sourcesCount = 0,
  imagesCount = 0,
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
    // A forward Next past the step with nothing connected (setup-screen's
    // selectStep -> markIntegrationsSkipped)
    // ORs in from the orchestrator, exactly like the old model-skip override —
    // this pure fn only knows about a real connected integration.
    integrations: integrationsCount > 0,
    ...demoDone,
    sources: sourcesCount > 0,
    images: imagesCount > 0,
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
