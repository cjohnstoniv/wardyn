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
// There is NO hard gate here any more. The old setupGateActive + App.tsx's
// RequireSetupComplete redirected EVERY route to /setup until the funnel was
// finished; both are deleted and should not come back.
//
// What replaced them is App.tsx's FirstRunLanding, which redirects "/" alone —
// so a fresh install still OPENS on the tour (landing on an empty Runs board
// left operators hunting for where to start) while every other route stays
// directly reachable and nothing ever bounces you back in. setupDismissed()
// below is that redirect's off-switch, set by one finish-or-skip.
//
// setup-screen re-exports these, so existing importers/tests are unaffected.
import { lsGet, lsSet } from "../../../lib/storage";
import type { Role } from "../../wardyn/operator-context";
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

// Member Getting Started "seen" flag (Phase 5) — set the first time the
// screen OBSERVES a non-empty own-runs list (never on a failed fetch), so a
// member who has actually launched a run isn't sent back to the funnel by
// firstRunLanding. Same lsGet/lsSet shape as DISMISS_KEY above.
const MEMBER_GETTING_STARTED_SEEN_KEY = "wardyn-member-getting-started-seen";

export function memberGettingStartedSeen(): boolean {
  return lsGet(MEMBER_GETTING_STARTED_SEEN_KEY) === "1";
}

export function markMemberGettingStartedSeen(): void {
  lsSet(MEMBER_GETTING_STARTED_SEEN_KEY, "1");
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
  status: { unreachable?: boolean; has_runs: boolean },
  role: Role = "admin",
): "/setup" | "/runs" {
  if (role === "member") {
    return !status.unreachable && !memberGettingStartedSeen() ? "/setup" : "/runs";
  }
  return !status.unreachable && !status.has_runs && !setupDismissed() ? "/setup" : "/runs";
}

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
