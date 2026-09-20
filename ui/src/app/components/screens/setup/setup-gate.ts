/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The first-run funnel's own per-browser state (dismissed / skipped /
// visited), split out of setup-screen.tsx so App.tsx (and anything else)
// can read it without pulling the funnel itself into the entry chunk.
// setup-screen reaches integrations-step → integrations-screen →
// add-integration-dialog → harness-login-pane → attach-terminal → xterm, so
// importing these from the screen module dragged the whole terminal stack
// into the initial bundle and defeated route-level code-splitting.
//
// TWO different gates live here, and the difference matters.
//
// firstRunLanding() decides where "/" alone lands, and an operator can dismiss
// it for good. That is a convenience, not a gate.
//
// setupGateActive() IS a gate: while the daemon marks a setup check `blocking`,
// every route redirects to the funnel. It is NOT the old funnel-completion gate
// that was deleted in 0.5 — that one demanded you FINISH the tour, which is why
// it was obnoxious. This one asks only that the install can work at all: the
// DAEMON decides which rows mean that (0.7.8), a grade alone never gates, and
// demos and optional steps stay skippable throughout.
//
// It is deliberately SERVER-DERIVED with no browser flag. The dismiss flag
// below outlives the install it describes — a browser that onboarded a previous
// install skips the tour on a freshly wiped database — so a per-browser gate
// would repeat exactly that bug.
//
// setup-screen re-exports these, so existing importers/tests are unaffected.
import { lsGet, lsSet } from "../../../lib/storage";
import type { Role } from "../../wardyn/operator-context";
import type { SetupCheckStatus } from "../../../lib/types/setup";
import type { SetupStepId } from "./steps";

// ------------------------------------------------------------
// Dismiss flag — via lib/storage's private-mode-tolerant lsGet/lsSet.
// ------------------------------------------------------------
// Suffixed for 0.5: the funnel this flag dismisses was deleted and rebuilt (12
// steps → 10, new workspace flow), so a mark left against the OLD one must not
// silently suppress the new tour. Browsers that onboarded a pre-0.5 install keep
// their stale `wardyn-setup-dismissed` — now inert — and get the tour once.
const DISMISS_KEY = "wardyn-setup-dismissed-v05";

export function setupDismissed(): boolean {
  return lsGet(DISMISS_KEY) === "1";
}

export function dismissSetup(): void {
  lsSet(DISMISS_KEY, "1");
}

// Where "/" lands — App.tsx's FirstRunLanding is a thin wrapper over this.
// Lives beside the dismiss flag it reads rather than in App.tsx so the whole
// decision is one testable function.
//
// Both halves must hold to open the tour: the SERVER says this install has
// never run anything (so `make reset-all` genuinely re-arms it), and THIS
// BROWSER has never finished or skipped the funnel. An unreachable daemon
// answers has_runs:false from the synthetic READY_FALLBACK, so it is excluded
// explicitly — a broken backend belongs on Runs behind the banner, not in a
// tour whose every step would read as un-ready.
//
// A member takes a DIFFERENT rule (Phase 5): their own per-browser
// memberGettingStartedSeen() flag, never the admin's has_runs/setupDismissed
// pair — a global has_runs:true (someone else's runs) must not skip a member
// past their own first landing. role defaults to "admin" (the same fail-open
// default the rest of this codebase uses for an unresolved role), so every
// existing admin call site is unaffected.
export function firstRunLanding(
  status: {
    unreachable?: boolean;
    has_runs: boolean;
    onboarding_complete?: boolean;
  },
  role: Role = "admin",
): "/setup" | "/runs" {
  if (role === "member") {
    // A member always lands on THEIR page (it is theirs to leave). Their
    // done-states come from creator-scoped listRuns observed live by the page
    // itself — never the install-global has_runs, and no longer a per-browser
    // "seen" flag either: that flag's one stated purpose was surviving a
    // pruning of the member's runs, and nothing in this codebase prunes runs,
    // while its live failure mode (a shared browser handing member B member
    // A's mark) was real.
    return !status.unreachable ? "/setup" : "/runs";
  }
  // The INSTALL's own onboarding mark decides (server-derived, survives a
  // browser change, agrees between 127.0.0.1 and localhost). setupDismissed()
  // remains ONLY as the legacy fallback for a daemon too old to report the
  // field — `??`, not `||`: absent must fall back to the flag, or every visit
  // to an older daemon reopens the tour forever.
  const onboarded = status.onboarding_complete ?? setupDismissed();
  return !status.unreachable && !status.has_runs && !onboarded
    ? "/setup"
    : "/runs";
}

// ------------------------------------------------------------
// The hard gate (server-derived).
// ------------------------------------------------------------
// True while this install has a setup check the DAEMON marks `blocking`
// (internal/api/setup_checks.go's SetupCheck.Blocking doc names the three
// rows that ever carry it — a dead runner, an unenforceable confinement
// floor, or SSO with no role mapping). A `warn`/`fail` grade alone does NOT
// gate: most of this checklist is either optional (an absent model provider)
// or a fact about the CALLER's own credential (a lapsed AWS sign-in), never
// an install defect the funnel exists to fix.
//
// Three deliberate non-gates:
//   - a MEMBER is never gated. `checks` is redacted for members (types/setup.ts),
//     so a member cannot see, let alone clear, an operator's setup — gating them
//     would be a trap with no exit.
//   - an UNREACHABLE daemon is never gated. Its synthetic status proves nothing;
//     a broken backend belongs behind AppShell's banner, not in a funnel whose
//     every step would read un-ready.
//   - an EMPTY check list is never gated, for the same reason: absent evidence
//     is not evidence of a problem.
//
// The funnel is self-sufficient — its own steps configure the environment, the
// network and secrets — so redirecting into it is never a dead end.
// The gate fires once per PAGE LOAD, not on every client-side navigation.
// "Forced redirect for any UI site access" (the owner's requirement) means an
// ACCESS — opening or reloading the console — always lands a gated install in
// the funnel. It does not mean imprisonment: the funnel's own affordances
// (People's "Open Permissions", the Secrets step's settings links) navigate to
// gated routes, and a gate that re-fires on every navigation bounces its own
// funnel back to step one — the walk that found this bug. Module-level rather
// than component state on purpose: /setup sits OUTSIDE the gate's route
// wrapper, so the wrapper unmounts while the operator is in the funnel and any
// ref would forget the gate already fired. A fresh load (new tab, F5) re-arms.
let gateFiredThisLoad = false;

export function markGateFired(): void {
  gateFiredThisLoad = true;
}

export function gateAlreadyFired(): boolean {
  return gateFiredThisLoad;
}

// Test seam only: unit tests share one module instance across cases.
export function resetGateForTests(): void {
  gateFiredThisLoad = false;
}

export function setupGateActive(
  status: {
    unreachable?: boolean;
    checks?: { id: string; status: SetupCheckStatus; blocking?: boolean }[];
    onboarding_complete?: boolean;
  },
  role: Role = "admin",
): boolean {
  // DELIBERATELY `!== "admin"`, NOT `role === "member"`, now that role is
  // three-valued: a SECURITY ADMIN must not be walled into the deployer's
  // setup funnel. Every mutation the gate exists to drive (harness credential,
  // integrations, the shared LLM credential) is super-admin-only server-side,
  // so trapping a security admin there would strand them on a wizard whose
  // every button 403s — and the server redacts that status for them anyway
  // (internal/api/setup.go).
  if (role !== "admin") return false;
  if (status.unreachable) return false;
  // An install that has BEEN THROUGH onboarding is never walled in again —
  // "a place you go, not a wall you are trapped behind" (the recorded reason
  // the 0.5 gate died). Its failing checks stay visible on every surface;
  // what they stop doing is confiscating the console. The gate's job is the
  // FIRST run: a fresh install does not open on an unexplained, unusable
  // board when the daemon itself says something is broken or degraded.
  if (status.onboarding_complete) return false;
  const checks = status.checks ?? [];
  if (checks.length === 0) return false;
  return checks.some((c) => c.blocking);
}

// 0.7.8: the decision moved server-side. This was an id list here — 0.7.7 added
// llm_provider, then bedrock_provider — and each version guessed at which ids
// are install defects rather than facts about one person's own credential. The
// next lapsed AWS sign-in graded harness_credential_aws, an id nobody had added
// yet, because a list enumerates a family instead of naming the property. The
// daemon marks the property now; there is no list here to keep in sync.

// Integrations-skip flag — the operator explicitly chose to move past the
// Integrations step with nothing connected (no model/harness, SCM host,
// artifact mirror, or host proxy). It earns the step its checkmark without any
// integration, so a deliberate skip reads as a decision rather than an
// unfinished "Optional". Per-browser (like the dismiss + onboarding-seen
// flags); a real connected integration makes it moot. Generalized from the old
// per-step "model-skipped" flag now that the provider step is folded into
// Integrations.
const INTEGRATIONS_SKIPPED_KEY = "wardyn-integrations-skipped";

export function integrationsSkipped(): boolean {
  return lsGet(INTEGRATIONS_SKIPPED_KEY) === "1";
}

export function markIntegrationsSkipped(): void {
  lsSet(INTEGRATIONS_SKIPPED_KEY, "1");
}

// Visited-step set (A4) — steps the operator has navigated AWAY from at least
// once (per browser), regardless of direction (Next, Back, a rail jump, or an
// in-step jump all count — leaving a step backward still means you've seen it).
// Powers the rail's neutral "Skipped" chip + muted dot for an OPTIONAL step left
// unconfigured, so it stops reading as a perpetual, un-acted-on "Optional". Same
// JSON-array-over-lsGet/lsSet shape as loadLaunchedDemos/markDemoLaunched
// (../demos/demo-catalog.ts) — this is a Set of ids, not a single flag.
const VISITED_KEY = "wardyn-setup-visited";

export function loadVisitedSteps(): SetupStepId[] {
  try {
    const parsed = JSON.parse(lsGet(VISITED_KEY) ?? "[]");
    return Array.isArray(parsed) ? (parsed as SetupStepId[]) : [];
  } catch {
    return [];
  }
}

export function markStepVisited(stepId: SetupStepId): void {
  const set = new Set(loadVisitedSteps());
  if (!set.has(stepId)) {
    set.add(stepId);
    lsSet(VISITED_KEY, JSON.stringify([...set]));
  }
}

// F3-F6: neither of the two flags above gates anything (the hard gate is
// server-derived, setupGateActive above) — their only job is the rail's
// "Skipped" badge. On a genuinely FRESH install (never onboarded, never run
// anything), a stale mark from a PREVIOUS install on this same browser reads
// as a false-green "Skipped" on a step nobody has actually visited this time
// — the same origin-scoped-flag class setupDismissed's own v05 suffix exists
// to prevent. Returns whether it cleared anything, so the caller can also
// reset its own in-memory copy of the same state (setup-screen.tsx holds
// visitedSteps/skippedIntegrations in React state, seeded from these at
// mount — clearing storage alone would leave that copy stale).
export function clearStaleVisitFlags(status: {
  onboarding_complete?: boolean;
  has_runs: boolean;
}): boolean {
  if (status.onboarding_complete || status.has_runs) return false;
  lsSet(INTEGRATIONS_SKIPPED_KEY, null);
  lsSet(VISITED_KEY, null);
  return true;
}

// setup-screen.tsx calls THIS, not clearStaleVisitFlags directly: a mark the
// operator sets a moment ago in THIS still-fresh session (skip Integrations,
// navigate away, come back) must survive a remount — a real browser reload
// really does re-mint the module (the module-level latch below resets), while
// a same-page remount (Getting Started's own re-entry, or this file's own
// test harness unmount/render) must not re-run the wipe. Same "once per page
// LOAD" shape as gateFiredThisLoad above, deliberately module state rather
// than component state for the identical reason.
//
// KNOWN CEILING (L2, no fix available in this lane): "once per page load" can
// only distinguish THIS load from the NEXT one — it cannot tell "a previous
// install's mark" from "mine, from 30 seconds ago, before a reload". A fresh
// install where the operator skips Integrations and then reloads (still
// !onboarding_complete && !has_runs) sees the latch re-arm and wipe its OWN
// skip. The correct fix is discriminating by INSTALL identity, not by page
// load — comparing this flag against a stable per-install marker, the same
// shape setupDismissed's own -v05 suffix uses — but SetupStatus (this file's
// own import) and GET /healthz carry no such value today (checked both;
// every field on each is a deliberate disclosure decision, and inventing one
// is a backend change outside this lane). Traded a rare false-green (a
// wiped-then-reinstalled browser showing a stale "Skipped") for this more
// common false-neutral (a fresh install's reload forgetting its own skip) —
// see local/v074/canon/ui-setup-shell-changelog.md's Known gaps.
let staleFlagsCheckedThisLoad = false;

export function clearStaleVisitFlagsOnce(status: {
  onboarding_complete?: boolean;
  has_runs: boolean;
}): boolean {
  if (staleFlagsCheckedThisLoad) return false;
  staleFlagsCheckedThisLoad = true;
  return clearStaleVisitFlags(status);
}

// Test seam only, mirroring resetGateForTests above.
export function resetStaleFlagsCheckForTests(): void {
  staleFlagsCheckedThisLoad = false;
}
