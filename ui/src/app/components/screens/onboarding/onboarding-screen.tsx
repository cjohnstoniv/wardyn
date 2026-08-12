/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// OnboardingScreen — the first-boot WELCOME (redesign). The old 7-page tour is
// collapsed into ONE glanceable intro (B1): a hero, the single 5-node
// how-it-works strip (shared with the funnel shell's intro panel), live readiness chips
// off the real SetupStatus, and two exits — Get set up / Skip. Rendered inside
// the AppShell as the "Getting started" nav content; "Get set up" advances to the
// setup funnel, "Skip" drops the operator into the console.
import * as React from "react";
import { ArrowRight, BrickWall, Compass, KeyRound, Shield } from "lucide-react";
import { Button } from "../../ui/button";
import { Chip } from "../../wardyn/primitives";
import { CC_META } from "../../wardyn/cc-meta";
import { strongestAvailable } from "../../wardyn/default-confinement";
import { EmptyState } from "../../wardyn/states";
import { useRole } from "../../wardyn/operator-context";
import { setup as api } from "../../../lib/api/setup";
import { lsGet, lsSet } from "../../../lib/storage";
import type { SetupStatus } from "../../../lib/types";
import { HowItWorksStrip, IntroBlurb, deriveReadiness } from "./intro";
import { SetupScreen } from "../setup/setup-screen";

// ---------------------------------------------------------------------------
// "Have they seen the welcome" flag — localStorage, private-mode tolerant.
// Distinct from wardyn-setup-dismissed so the welcome and the setup funnel track
// separately (skipping the welcome must not dismiss the funnel).
// ---------------------------------------------------------------------------
const ONBOARDING_KEY = "wardyn-onboarding-seen";
export function onboardingSeen(): boolean {
  return lsGet(ONBOARDING_KEY) === "1";
}
export function markOnboardingSeen(): void {
  lsSet(ONBOARDING_KEY, "1");
}

// B4 HIGH-4: a member has no "Getting started" nav entry (setup is the
// operator's funnel — app-shell.tsx), but nothing stops a direct /setup
// navigation (an old bookmark, a shared link). Land honestly instead of
// rendering the operator funnel (built from a redacted SetupStatus a member
// can't act on) or silently bouncing them elsewhere unexplained.
function MemberSetupNotice({ onDone }: { onDone: () => void }) {
  return (
    <div className="mx-auto flex min-h-[60vh] w-full max-w-[560px] items-center px-6">
      <EmptyState
        icon={Compass}
        title="Setup is managed by your workspace admin"
        description="Barriers, model access, and integrations are configured by an admin — your runs inherit what they've set up. There's nothing to configure here."
        // onDone is App.tsx's navigate("/runs") — the same exit every finished
        // funnel uses, not a second copy of the route.
        action={<Button onClick={onDone}>Go to Runs</Button>}
      />
    </div>
  );
}

// The "Getting started" flow: the welcome hero first (until seen), then the setup
// funnel. No double stepper — the welcome has no stepper; the funnel has one.
export function GettingStarted({ onDone }: { onDone: () => void }) {
  const role = useRole();
  const [seen, setSeen] = React.useState(onboardingSeen());
  if (role === "member") {
    return <MemberSetupNotice onDone={onDone} />;
  }
  if (!seen) {
    // Single forward path: the welcome hands off INTO the funnel (no skip, no
    // demo side-door — demos live inside the funnel). The mandatory setup gate
    // (App.tsx) keeps the operator here until they finish the flow.
    return (
      <OnboardingScreen
        onGetStarted={() => {
          markOnboardingSeen();
          setSeen(true);
        }}
      />
    );
  }
  return <SetupScreen onDone={onDone} />;
}

function ReadinessRow({ status, loading }: { status: SetupStatus | null; loading: boolean }) {
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
  const strongest = status ? strongestAvailable(status.runner.confinement_classes) : undefined;
  const strongestLabel = strongest ? CC_META[strongest].label : CC_META.CC1.label;
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
        <KeyRound className="size-3" /> {readiness.llmLabel ? `Model: ${readiness.llmLabel}` : "Model: ready"}
      </Chip>
    ) : (
      <Chip tone="neutral">
        <KeyRound className="size-3" /> Model: optional
      </Chip>
    );
  return (
    <div className="mt-8 w-full rounded-xl border border-border bg-muted/40 p-4 text-left">
      <div className="mb-3 text-sm text-muted-foreground">This host right now:</div>
      <div className="flex flex-wrap gap-2">
        {chip(BrickWall, !!readiness?.barrierReady, `Barrier: ${strongestLabel} ready`, "Barrier: needs setup")}
        {modelChip}
      </div>
    </div>
  );
}

export function OnboardingScreen({ onGetStarted }: { onGetStarted: () => void }) {
  const [status, setStatus] = React.useState<SetupStatus | null>(null);
  const [loading, setLoading] = React.useState(true);

  React.useEffect(() => {
    let active = true;
    api
      .getSetupStatus()
      .then((s) => active && setStatus(s))
      .catch(() => {
        /* leave readiness unknown — never block the welcome on a failed probe */
      })
      .finally(() => active && setLoading(false));
    return () => {
      active = false;
    };
  }, []);

  return (
    <div className="mx-auto w-full max-w-[780px] px-6 py-12">
      <span className="inline-flex size-11 items-center justify-center rounded-xl bg-primary/15 text-primary">
        <Shield className="size-6" aria-hidden />
      </span>
      <h1 className="mt-5 text-[2rem] font-semibold leading-tight tracking-tight text-foreground">
        Run anything. Keep your keys.
      </h1>
      <p className="mt-3 max-w-[640px] text-base leading-relaxed text-muted-foreground">
        <IntroBlurb />
      </p>

      <div className="mt-7 w-full text-left">
        <HowItWorksStrip />
      </div>

      <ReadinessRow status={status} loading={loading} />

      <div className="mt-6 flex flex-wrap items-center gap-2.5">
        <Button onClick={onGetStarted}>
          Get started — about 2 minutes <ArrowRight className="size-4" />
        </Button>
      </div>

      <p className="mt-6 max-w-[560px] text-xs text-muted-foreground">
        A quick guided setup — the barrier is the only requirement; a model or agent
        is optional. You can revisit anytime under “Getting started” in the sidebar.
      </p>
    </div>
  );
}
