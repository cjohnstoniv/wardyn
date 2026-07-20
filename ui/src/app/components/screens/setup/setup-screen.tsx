/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SetupScreen — the "Getting started" first-run funnel ORCHESTRATOR. It owns the
// SetupStatus fetch + re-check, the persisted default-barrier pick, the workspace/
// secret/site-config loads, and every dialog; the SHELL (header, dismissible
// intro, fast-path banner, phase rail, host-status strip, step heading, footer
// nav) lives in SetupLayout, the pure step bodies in ./environment-step,
// ./llm-access, ./step-bodies, and the step/badge data in ./steps.
//
// Read-only against GET /api/v1/setup/status (the FROZEN SetupStatus contract in
// lib/types.ts) except for the setSecret()/putSiteConfig() writes each step body
// owns. Never traps the operator: "Finish later" and launching both dismiss it,
// and every AppShell nav item stays reachable while it's open.
import * as React from "react";
import type { ConfinementClass, SetupStatus, SiteConfig, Workspace } from "../../../lib/types";
import { health as healthApi } from "../../../lib/api/health";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { setup as setupApi } from "../../../lib/api/setup";
import { workspaces as workspacesApi } from "../../../lib/api/workspaces";
import { getDefaultCc, resolveDefaultCc, setDefaultCc } from "../../wardyn/default-confinement";
import { AddSecretDialog } from "../secrets";
import { NewRunDialog } from "../new-run/new-run-dialog";
import { SetupGuideDialog, type SetupGuide } from "./setup-guide";
import { deriveReadiness, lastCheckedLabel } from "../onboarding/intro";
import { SetupLayout } from "./setup-layout";
import { PhaseRail } from "./phase-rail";
import { EnvironmentStep } from "./environment-step";
import { ModelStep } from "./llm-access";
import {
  ArtifactRepoStep,
  CredentialsStep,
  HostProxyStep,
  LaunchStep,
  ReviewStep,
  ScmProviderStep,
  WorkspacesStep,
} from "./step-bodies";
import { DEMO_STEP_IDS, STEP_ORDER, stepBadges, stepDone, type SetupStepId } from "./steps";
import { DEMOS, loadLaunchedDemos } from "../demos/demo-catalog";

// The dismiss flag and the auto-open decision live in ./setup-gate so App.tsx
// can import them without pulling this module's terminal-heavy graph into the
// entry chunk. Re-exported here: this is still their public home.
export { dismissSetup, setupDismissed, shouldOpenSetup } from "./setup-gate";
import { dismissSetup, markModelSkipped, modelSkipped } from "./setup-gate";

// Each demo sub-step renders DemoDetail, which pulls AttachTerminal → xterm.
// Lazy-load it so that terminal-heavy graph stays out of the setup chunk until
// the operator opens a demo step (same reasoning as the /demos route). The pure
// demo catalog + launched-set reader are imported eagerly above (xterm-free).
const DemoDetail = React.lazy(() => import("./demos-step"));

// ------------------------------------------------------------
// SetupScreen
// ------------------------------------------------------------
export function SetupScreen({ onDone }: { onDone: () => void }) {
  const [stepId, setStepId] = React.useState<SetupStepId>("environment");
  const [status, setStatus] = React.useState<SetupStatus | null>(null);
  const [rechecking, setRechecking] = React.useState(false);
  const [lastCheckedAt, setLastCheckedAt] = React.useState<Date | null>(null);
  // Bumped whenever a host re-check COMPLETES — EnvironmentStep reads it as
  // recheckToken to surface a tier's "still not detected" line after a re-probe.
  const [recheckCount, setRecheckCount] = React.useState(0);
  // Per-demo "launched at least once" set (per browser). Seeded from the durable
  // record markDemoLaunched writes, and grown live via onDemoLaunched so each demo
  // sub-step earns its checkmark this session too. See the stepDone override.
  const [launchedDemos, setLaunchedDemos] = React.useState<Set<string>>(
    () => new Set(loadLaunchedDemos()),
  );
  // The operator explicitly skipped the (optional) model/harness step — earns it a
  // checkmark without a connected model. Per-browser (setup-gate); a real model
  // supersedes it.
  const [skippedModel, setSkippedModel] = React.useState(modelSkipped());
  const [secretNames, setSecretNames] = React.useState<string[]>([]);
  const [workspaces, setWorkspaces] = React.useState<Workspace[]>([]);
  const [wsLoading, setWsLoading] = React.useState(true);
  // Site config powers the corporate-baseline step badges: the backend's own
  // host_proxy/scm/artifact checks stay hardcoded "info" forever, so the rail badge
  // must read the actual SiteConfig fields those steps edit, not the check status.
  const [siteConfig, setSiteConfig] = React.useState<SiteConfig | null>(null);
  const [addSecretOpen, setAddSecretOpen] = React.useState(false);
  const [addSecretName, setAddSecretName] = React.useState("");
  const [newRunOpen, setNewRunOpen] = React.useState(false);
  const [guide, setGuide] = React.useState<SetupGuide | null>(null);
  // Default-barrier pick (E3). Null until an explicit click — until then the
  // effective selection is the resolved default (persisted pick if this host runs
  // it, else strongest available). Clicking a ready card both selects and persists.
  const [ccOverride, setCcOverride] = React.useState<ConfinementClass | null>(null);
  const selectDefault = React.useCallback((cc: ConfinementClass) => {
    setCcOverride(cc);
    setDefaultCc(cc);
  }, []);

  // Sole SiteConfig owner (V2): the orchestrator holds the one fetched copy and
  // hands the three corp steps a reload + save pair instead of each keeping its
  // own mount-time GET synced back up via a callback prop.
  const reloadSiteConfig = React.useCallback(() => {
    return healthApi.getSiteConfig().then(setSiteConfig).catch(() => {});
  }, []);

  const saveSiteConfig = React.useCallback((next: SiteConfig) => {
    return healthApi.putSiteConfig(next).then(() => setSiteConfig(next));
  }, []);

  const recheck = React.useCallback(() => {
    setRechecking(true);
    // Load/resync the corporate-baseline SiteConfig (F2): the rail "Configured"
    // badges read siteConfig state, so mount (via this recheck) and every manual
    // Re-check pull it — a failure leaves the last-known config (or the initial
    // null) in place, never clobbers it. This is the ORCHESTRATOR'S sole GET path.
    reloadSiteConfig();
    return setupApi
      .getSetupStatus()
      .then((s) => {
        setStatus(s);
        setLastCheckedAt(new Date());
        // A fresh probe landed — bump the token EnvironmentStep watches.
        setRecheckCount((n) => n + 1);
      })
      .finally(() => setRechecking(false));
  }, [reloadSiteConfig]);

  const loadSecrets = React.useCallback(() => {
    secretsApi.listSecrets().then(setSecretNames).catch(() => setSecretNames([]));
  }, []);

  const loadWorkspaces = React.useCallback(() => {
    setWsLoading(true);
    workspacesApi
      .listWorkspaces()
      .then(setWorkspaces)
      .catch(() => setWorkspaces([]))
      .finally(() => setWsLoading(false));
  }, []);

  React.useEffect(() => {
    recheck(); // also performs the initial SiteConfig GET (see recheck)
    loadSecrets();
    loadWorkspaces();
    // run once on mount — the loaders are stable (useCallback([]))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const finish = React.useCallback(() => {
    dismissSetup();
    onDone();
  }, [onDone]);

  const openAddSecret = (name: string) => {
    setAddSecretName(name);
    setAddSecretOpen(true);
  };

  const readiness = status ? deriveReadiness(status) : null;
  // Effective default-barrier selection: the explicit click if any, else the
  // persisted pick — BOTH re-resolved against live availability, so a class that
  // vanishes on a recheck (e.g. Docker stops mid-session) degrades to the strongest
  // available card instead of leaving zero cards selected.
  const selectedCc = resolveDefaultCc(
    ccOverride ?? getDefaultCc(),
    status?.runner.confinement_classes ?? [],
  );

  // Persist the resolved default the first time we can (no explicit pick yet), so the
  // recommended/strongest-available tier the picker SHOWS is the one actually saved.
  // Otherwise consumers that read the stored default (e.g. the import SecurityChip)
  // fall back to CC1/Fence and disagree with what the barrier step displays — you pick
  // Vault, but the import shows Fence. Clicking a card still overrides + re-persists.
  React.useEffect(() => {
    if (status && !getDefaultCc()) setDefaultCc(selectedCc);
  }, [status, selectedCc]);

  if (!status || !readiness) {
    return (
      <div className="mx-auto w-full max-w-[1200px] px-6 py-8">
        <p className="text-sm text-muted-foreground">Checking Wardyn&apos;s setup…</p>
      </div>
    );
  }

  // A manually-opened funnel against a daemon that didn't answer: say THAT,
  // instead of rendering step bodies (no-runner danger card, "Needs setup"
  // badges) built from the synthetic fallback's made-up fields.
  if (status.unreachable) {
    return (
      <div className="mx-auto w-full max-w-[1200px] px-6 py-8">
        <h1 className="text-lg font-semibold text-foreground">Getting started</h1>
        <div className="mt-4 max-w-xl rounded-lg border border-border bg-muted/40 p-4">
          <p className="text-sm text-foreground">Couldn&apos;t reach Wardyn.</p>
          <p className="mt-1 text-sm leading-relaxed text-muted-foreground">
            The setup status request didn&apos;t get an answer, so nothing on this page would be
            trustworthy. Check that wardynd is running (<code className="rounded bg-background/70 px-1 py-0.5 text-xs">make setup</code>
            , logs in <code className="rounded bg-background/70 px-1 py-0.5 text-xs">~/.wardyn/host-wardynd.log</code>), then re-check.
          </p>
          <button
            className="mt-3 rounded-md border border-border px-3 py-1.5 text-sm text-foreground hover:bg-muted disabled:opacity-50"
            onClick={recheck}
            disabled={rechecking}
          >
            {rechecking ? "Re-checking…" : "Re-check"}
          </button>
        </div>
      </div>
    );
  }

  const badges = stepBadges(status, readiness, workspaces, siteConfig);
  const done = stepDone(status, readiness, workspaces, siteConfig);
  // Each demo sub-step earns its checkmark once THAT demo has been launched (a
  // per-browser signal kept out of the pure stepBadges/stepDone — see steps.ts).
  for (const id of DEMO_STEP_IDS) {
    if (launchedDemos.has(id)) {
      done[id] = true;
      badges[id] = { text: "Done · demo run", tone: "success" };
    }
  }
  // An explicitly-skipped (optional) model step earns its checkmark — a deliberate
  // "no model" decision reads as done, not as an unfinished "Optional". A real
  // connected model (llmReady) always wins and shows its own "Ready" badge.
  if (!readiness.llmReady && skippedModel) {
    done.provider = true;
    badges.provider = { text: "Skipped", tone: "neutral" };
  }

  return (
    <>
      <SetupLayout
        current={stepId}
        rail={<PhaseRail current={stepId} badges={badges} done={done} onSelect={setStepId} />}
        checking={rechecking}
        lastCheckedLabel={lastCheckedLabel(lastCheckedAt)}
        onRecheck={recheck}
        onSelect={setStepId}
        onFinish={finish}
        onLaunch={() => setNewRunOpen(true)}
        // A barrier is enough to launch (an interactive run works with no model —
        // the operator drives it over an attached terminal).
        canLaunch={readiness.ready}
      >
        {stepId === "environment" && (
          <EnvironmentStep
            status={status}
            selected={selectedCc}
            onSelect={selectDefault}
            recheckToken={recheckCount}
            rechecking={rechecking}
          />
        )}
        {stepId === "provider" && (
          <ModelStep
            status={status}
            readiness={readiness}
            skipped={skippedModel}
            onAddSecret={openAddSecret}
            onSetup={setGuide}
            onRecheck={recheck}
            rechecking={rechecking}
            onSkip={() => {
              markModelSkipped();
              setSkippedModel(true);
              // Advance past the (now-decided) optional step.
              const i = STEP_ORDER.indexOf("provider");
              const nextStep = STEP_ORDER[i + 1];
              if (nextStep) setStepId(nextStep);
            }}
          />
        )}
        {DEMOS.some((d) => d.id === stepId) && (
          <React.Suspense
            fallback={<p className="text-sm text-muted-foreground">Loading demo…</p>}
          >
            <DemoDetail
              demo={DEMOS.find((d) => d.id === stepId)!}
              barrierReady={readiness.barrierReady}
              onJump={setStepId}
              onDemoLaunched={(id) => setLaunchedDemos((s) => new Set(s).add(id))}
            />
          </React.Suspense>
        )}
        {stepId === "host_proxy" && (
          <HostProxyStep
            status={status}
            siteConfig={siteConfig}
            reloadSiteConfig={reloadSiteConfig}
            saveSiteConfig={saveSiteConfig}
            onAddSecret={openAddSecret}
            onRecheck={recheck}
            rechecking={rechecking}
          />
        )}
        {stepId === "scm_provider" && (
          <ScmProviderStep
            status={status}
            siteConfig={siteConfig}
            reloadSiteConfig={reloadSiteConfig}
            saveSiteConfig={saveSiteConfig}
            onAddSecret={openAddSecret}
            onJump={setStepId}
            onRecheck={recheck}
            rechecking={rechecking}
          />
        )}
        {stepId === "artifact_repo" && (
          <ArtifactRepoStep
            status={status}
            siteConfig={siteConfig}
            reloadSiteConfig={reloadSiteConfig}
            saveSiteConfig={saveSiteConfig}
            onRecheck={recheck}
            rechecking={rechecking}
          />
        )}
        {stepId === "workspaces" && (
          <WorkspacesStep workspaces={workspaces} loading={wsLoading} onReload={loadWorkspaces} />
        )}
        {stepId === "credentials" && (
          <CredentialsStep
            status={status}
            onAddSecret={openAddSecret}
            onRecheck={recheck}
            rechecking={rechecking}
          />
        )}
        {stepId === "review" && (
          <ReviewStep
            status={status}
            readiness={readiness}
            onRecheck={recheck}
            rechecking={rechecking}
            lastCheckedAt={lastCheckedAt}
            onJump={setStepId}
          />
        )}
        {stepId === "launch" && (
          <LaunchStep
            status={status}
            onLaunch={() => setNewRunOpen(true)}
            onOpenRuns={onDone}
            canLaunch={readiness.ready}
            llmReady={readiness.llmReady}
          />
        )}
      </SetupLayout>

      <AddSecretDialog
        open={addSecretOpen}
        onOpenChange={setAddSecretOpen}
        existingNames={secretNames}
        initialName={addSecretName}
        onSaved={() => {
          loadSecrets();
          recheck();
        }}
      />
      <NewRunDialog
        open={newRunOpen}
        onOpenChange={setNewRunOpen}
        onCreated={() => {
          dismissSetup();
          onDone();
        }}
      />
      <SetupGuideDialog
        guide={guide}
        onClose={() => setGuide(null)}
        onRecheck={recheck}
        rechecking={rechecking}
      />
    </>
  );
}
