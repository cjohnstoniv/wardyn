/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// OnboardingScreen — the first-boot WELCOME (redesign). The old 7-page tour is
// collapsed into ONE glanceable intro (B1): a hero, the single 5-node
// how-it-works strip (shared with the funnel shell's intro panel), live readiness chips
// off the real SetupStatus, and a single forward path into the setup funnel — no
// skip, no demo side-door (see GettingStarted below). Rendered inside the
// AppShell as the "Getting started" nav content.
import * as React from "react";
import { ArrowRight, BrickWall, KeyRound, Shield } from "lucide-react";
import { Button } from "../../ui/button";
import { Chip } from "../../wardyn/primitives";
import { CC_META } from "../../wardyn/cc-meta";
import { strongestAvailable } from "../../wardyn/default-confinement";
import { useRole } from "../../wardyn/operator-context";
import { lsGet, lsSet } from "../../../lib/storage";
import { markGateFired } from "../setup/setup-gate";
import { deploymentMode } from "../../../lib/readiness";
import type { SetupStatus } from "../../../lib/types";
import { HowItWorksStrip, IntroBlurb } from "./intro";
import { EpisodeList } from "./episode-card";
import { MemberGettingStarted } from "./member-getting-started";
import { deriveReadiness } from "../../../lib/readiness";
import { SetupScreen } from "../setup/setup-screen";

// "Have they seen the welcome" flag — localStorage, private-mode tolerant.
// Distinct from wardyn-setup-dismissed so the welcome and the setup funnel track
// separately (skipping the welcome must not dismiss the funnel).
const ONBOARDING_KEY = "wardyn-onboarding-seen";
export function onboardingSeen(): boolean {
  return lsGet(ONBOARDING_KEY) === "1";
}
export function markOnboardingSeen(): void {
  lsSet(ONBOARDING_KEY, "1");
}

// The "Getting started" flow: the welcome hero first (until seen), then the setup
// funnel. No double stepper — the welcome has no stepper; the funnel has one.
//
// B4 HIGH-4 / Phase 5: a member has no "Getting started" nav entry (setup is
// the operator's funnel — app-shell.tsx), but nothing stops a direct /setup
// navigation (an old bookmark, a shared link). Land on the member's OWN
// Getting Started (member-getting-started.tsx) instead of the operator funnel
// (built from a redacted SetupStatus a member can't act on) — replaces the
// former one-line MemberSetupNotice bounce.
export function GettingStarted({
  onDone,
  status,
}: {
  onDone: () => void;
  status?: SetupStatus | null;
}) {
  const role = useRole();
  // The hero is a fact about the install (status.onboarding_complete), not
  // the browser: a per-browser flag is origin-scoped, so the same console
  // reached at 127.0.0.1 and at localhost would disagree about whether the
  // welcome happened. The local flag survives as (a) the within-session
  // handoff — clicking "Get started" advances without waiting for a status
  // refetch — and (b) the legacy fallback for a daemon too old to report the
  // field.
  const [seen, setSeen] = React.useState(onboardingSeen());
  const installOnboarded = status?.onboarding_complete ?? false;
  // Being IN the funnel satisfies the gate's purpose for this load. The gate's
  // once-per-load flag otherwise arms only when a GATED route renders — but a
  // load can start directly on /setup (a reload while onboarding, the SSO
  // callback's return), which sits outside the gate's wrapper; without this,
  // the first navigation out of such a load re-fires the gate and the funnel's
  // own "Open Permissions" bounces back to step one. Landing here IS the
  // forced redirect's destination, so arriving here arms it.
  React.useEffect(() => {
    markGateFired();
  }, []);
  // Deliberately `!== "admin"`, not `role === "member"`, for the reason
  // setupGateActive (setup/setup-gate.ts) is written the same way now that role
  // is three-valued: GET /setup/status is redacted for every non-operator
  // (handleSetupStatus -> redactSetupStatusForMember zeroes Checks, Providers
  // and Secrets, internal/api/setup.go), and every mutation the deployer funnel
  // drives is super-admin-only server-side. A security admin falling through
  // here would get the operator funnel built from a status they cannot act on
  // and a wizard whose every button 403s.
  if (role !== "admin") {
    // No onDone: this is a page a member returns to, not a funnel step with
    // an exit action — the old MemberSetupNotice's "Go to Runs" button (and
    // the onDone it called) leaves with it.
    return <MemberGettingStarted />;
  }
  if (!installOnboarded && !seen) {
    // Single forward path: the welcome hands off INTO the funnel (no skip, no
    // demo side-door — demos live inside the funnel). The mandatory setup gate
    // (App.tsx) keeps the operator here until they finish the flow.
    return (
      <OnboardingScreen
        status={status ?? null}
        onGetStarted={() => {
          markOnboardingSeen();
          setSeen(true);
        }}
      />
    );
  }
  return <SetupScreen onDone={onDone} initialStatus={status} />;
}

function ReadinessRow({
  status,
  loading,
}: {
  status: SetupStatus | null;
  loading: boolean;
}) {
  const readiness = status ? deriveReadiness(status) : null;
  const chip = (
    icon: React.ElementType,
    ready: boolean,
    readyText: string,
    // The not-ready label carries its own subject too — a bare "Needs setup"
    // put the noun only in the ready branch, so the not-ready meaning rode on
    // the warning icon alone. Mirrors modelChip below, which already prefixes
    // "Model:" in every branch.
    notReadyText: string,
  ): React.ReactNode => {
    const Icon = icon;
    if (loading || !readiness)
      return (
        <Chip tone="neutral">
          <Icon className="size-3" /> Checking…
        </Chip>
      );
    return (
      <Chip tone={ready ? "success" : "warning"} dot={ready}>
        <Icon className="size-3" /> {ready ? readyText : notReadyText}
      </Chip>
    );
  };
  // Strongest AVAILABLE barrier label for the "Fence ready" text, from the real
  // confinement-class list.
  const strongest = status
    ? strongestAvailable(status.runner.confinement_classes)
    : undefined;
  const strongestLabel = strongest
    ? CC_META[strongest].label
    : CC_META.CC1.label;
  // Barrier (the one hard requirement) + Model (optional). The model chip reads
  // neutral "optional" when absent — not a warning "Needs setup" — because a run
  // works with no model (you drive it, or bring your own container). No Composer
  // chip (zero composer UI on the hero; composerReady is left unused here).
  const modelChip =
    loading || !readiness ? (
      <Chip tone="neutral">
        <KeyRound className="size-3" /> Checking…
      </Chip>
    ) : readiness.llmReady ? (
      <Chip tone="success" dot>
        <KeyRound className="size-3" />{" "}
        {readiness.llmLabel ? `Model: ${readiness.llmLabel}` : "Model: ready"}
      </Chip>
    ) : (
      <Chip tone="neutral">
        <KeyRound className="size-3" /> Model: optional
      </Chip>
    );
  return (
    <div className="mt-8 w-full rounded-xl border border-border bg-muted/40 p-4 text-left">
      <div className="mb-3 text-sm text-muted-foreground">
        This host right now:
      </div>
      <div className="flex flex-wrap gap-2">
        {chip(
          BrickWall,
          !!readiness?.barrierReady,
          `Barrier: ${strongestLabel} ready`,
          "Barrier: needs setup",
        )}
        {modelChip}
      </div>
    </div>
  );
}

export function OnboardingScreen({
  onGetStarted,
  status = null,
}: {
  onGetStarted: () => void;
  // The App-resolved status (App.tsx fetches it once per session and polls
  // it every few minutes) — NOT a fetch of its own. A second, independent
  // getSetupStatus() call here used to run once with no retry, so a single
  // dropped request left this page permanently reading a real install as
  // "unknown" (or, worse, silently defaulting the episode catalog below to
  // single-user) for the rest of that page load. Sharing App's copy means a
  // dropped request self-heals on App's next poll instead of wedging.
  status?: SetupStatus | null;
}) {
  // `unreachable` marks the synthetic READY_FALLBACK (setup.ts) — a failed or
  // unreachable probe, not a real answer. Treat it exactly like "no answer
  // yet" rather than a real single-user/no-barrier install (same rule every
  // other consumer of this field follows).
  const known = status && !status.unreachable ? status : null;
  const loading = status === null;

  return (
    <div className="mx-auto w-full max-w-[780px] px-6 py-12">
      <span className="inline-flex size-11 items-center justify-center rounded-xl bg-primary/15 text-primary">
        <Shield className="size-6" aria-hidden />
      </span>
      <h1 className="mt-5 text-[2rem] font-semibold leading-tight tracking-tight text-foreground">
        Sandboxed. Governed. Self-hosted. Free.
      </h1>
      <p className="mt-3 max-w-[640px] text-base leading-relaxed text-muted-foreground">
        <IntroBlurb />
      </p>

      <div className="mt-7 w-full text-left">
        <HowItWorksStrip />
      </div>

      <ReadinessRow status={known} loading={loading} />

      <div className="mt-6 flex flex-wrap items-center gap-2.5">
        <Button onClick={onGetStarted}>
          Get started — a few minutes <ArrowRight className="size-4" />
        </Button>
      </div>

      <p className="mt-6 max-w-[560px] text-xs text-muted-foreground">
        A quick guided setup — the barrier is the only requirement; a model or
        agent is optional. You can revisit anytime under “Getting started” in
        the account menu.
      </p>

      <EpisodeList mode={known && deploymentMode(known) === "multi-user" ? "multi" : "single"} />
    </div>
  );
}
