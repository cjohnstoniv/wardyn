/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SetupScreen — the "Getting started" first-run funnel ORCHESTRATOR. It owns the
// SetupStatus fetch + re-check, the persisted default-barrier pick, the workspace/
// secret/site-config loads, and every dialog; the SHELL (header, dismissible
// intro, fast-path banner, phase rail, host-status strip, step heading, footer
// nav) lives in SetupLayout, the pure step bodies in ./environment-step,
// ./integrations-step, ./step-bodies, and the step/badge data in ./steps.
//
// Read-only against GET /api/v1/setup/status (the FROZEN SetupStatus contract in
// lib/types.ts) and GET /site-config — every write (secrets, SiteConfig,
// integrations) now happens inside the embedded Integrations step's own dialogs,
// not here. It is the MANDATORY first-run gate — there is no early escape; see
// App.tsx's RequireSetupComplete for everything that clears it.
import * as React from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import type { ConfinementClass, SetupStatus, SiteConfig } from "../../../lib/types";
import { health as healthApi } from "../../../lib/api/health";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { setup as setupApi } from "../../../lib/api/setup";
import { deriveIntegrations, genericIntegrations } from "../../../lib/api/integrations";
import { useWorkspaceList } from "../../../lib/use-workspace-list";
import { getDefaultCc, resolveDefaultCc, setDefaultCc } from "../../wardyn/default-confinement";
import { NewRunDialog } from "../new-run/new-run-dialog";
import { deriveReadiness, lastCheckedLabel } from "../onboarding/intro";
import { SetupLayout } from "./setup-layout";
import { PhaseRail } from "./phase-rail";
import { EnvironmentStep } from "./environment-step";
import { CorpNetworkStep, isProxyConfigured, proxyDetected, type CorpStepActions } from "./corp-network-step";
import { IntegrationsStep } from "./integrations-step";
import { ImagesStep, LaunchStep, ReviewStep, SourcesStep, WorkspacesStep } from "./step-bodies";
import { baseImagesApi, sourcesApi } from "../../../lib/api/sources";
import {
  DEMO_STEP_IDS,
  OPTIONAL_STEPS,
  STEP_ORDER,
  corpNetworkGate,
  stepBadges,
  stepDone,
  type CorpGateActionKind,
  type CorpNetworkState,
  type SetupStepId,
} from "./steps";
import { DEMOS, loadLaunchedDemos } from "../demos/demo-catalog";

// The dismiss flag lives in ./setup-gate so App.tsx can import it without
// pulling this module's terminal-heavy graph into the entry chunk. Re-exported
// here: this is still its public home.
export { dismissSetup, setupDismissed } from "./setup-gate";
import {
  dismissSetup,
  integrationsSkipped,
  loadVisitedSteps,
  markIntegrationsSkipped,
  markStepVisited,
} from "./setup-gate";

// Each demo sub-step renders DemoDetail, which pulls AttachTerminal → xterm.
// Lazy-load it so that terminal-heavy graph stays out of the setup chunk until
// the operator opens a demo step (same reasoning as the /demos route). The pure
// demo catalog + launched-set reader are imported eagerly above (xterm-free).
const DemoDetail = React.lazy(() => import("./demos-step"));

// ------------------------------------------------------------
// SetupScreen
// ------------------------------------------------------------
export function SetupScreen({ onDone }: { onDone: () => void }) {
  // ?step=<id> deep-links a specific step — the Integrations page's
  // proxy-detected banner uses it to hand off to Corporate network, now the
  // only place a proxy is configured. Read once at mount (an unknown or
  // absent value just starts at the beginning); the URL is not kept in sync
  // afterwards, since the rail is the navigation from then on.
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const [stepId, setStepId] = React.useState<SetupStepId>(() => {
    const want = searchParams.get("step");
    return want && (STEP_ORDER as string[]).includes(want) ? (want as SetupStepId) : "environment";
  });
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
  // The operator explicitly skipped the (optional) Integrations step — earns it
  // a checkmark with nothing connected. Per-browser (setup-gate); a real
  // connected integration supersedes it. Generalized from the old per-step
  // "model-skipped" flag now that the provider picker lives inside Integrations.
  const [skippedIntegrations, setSkippedIntegrations] = React.useState(integrationsSkipped());
  // Corporate network's gate proof (proxy probe / egress-tab visit / per-redirect
  // tests) — held HERE, not inside CorpNetworkStep, because that component
  // unmounts on navigation and this must survive leaving and re-entering the
  // step (see steps.ts's CorpNetworkState doc). Deliberately in-memory only,
  // never persisted: a stale "reached" surviving a reload would be exactly the
  // false reassurance this whole feature exists to prevent.
  const [corpGate, setCorpGate] = React.useState<
    Pick<CorpNetworkState, "proxyProbe" | "probeRunning" | "customDraft" | "redirectProbes">
  >({
    probeRunning: false,
    customDraft: "",
    redirectProbes: {},
  });
  const onCorpGateChange = React.useCallback(
    (patch: Partial<Pick<CorpNetworkState, "proxyProbe" | "probeRunning" | "customDraft" | "redirectProbes">>) => {
      setCorpGate((g) => ({ ...g, ...patch }));
    },
    [],
  );
  // The step's imperative fix-it handlers (run the probe, open the egress tab,
  // fire the redirect tests) — registered by CorpNetworkStep while mounted, so
  // the footer's gate action (rendered by SetupLayout, dispatched here) can
  // reach INTO the step. A ref, not state: nothing re-renders on registration.
  const corpActionsRef = React.useRef<CorpStepActions | null>(null);
  // The step's sub-tab, held HERE so the footer can walk the tabs (Next on
  // Host proxy forwards to Egress redirection, not the exit — the mock's
  // gateFor) and so the tab survives leaving/re-entering the step, same as
  // the gate proof itself.
  const [corpTab, setCorpTab] = React.useState<"proxy" | "egress">("proxy");
  // Steps navigated AWAY from at least once (per browser) — feeds the rail's
  // "Skipped" badge override below. See selectStep for what counts as leaving.
  const [visitedSteps, setVisitedSteps] = React.useState<Set<SetupStepId>>(
    () => new Set(loadVisitedSteps()),
  );
  const [secretNames, setSecretNames] = React.useState<string[]>([]);
  const { workspaces, loading: wsLoading, reload: loadWorkspaces } = useWorkspaceList();
  // Tier-1/2 library sizes for the rail badges. The tier step bodies own their
  // own lists; this refetches on every step change so a just-added entry earns
  // its badge as you move on. Best-effort — a failed fetch keeps the last count.
  const [tierCounts, setTierCounts] = React.useState({ sources: 0, images: 0 });
  React.useEffect(() => {
    let live = true;
    Promise.all([sourcesApi.listSources().catch(() => null), baseImagesApi.listBaseImages().catch(() => null)]).then(
      ([srcs, imgs]) =>
        live &&
        setTierCounts((prev) => ({
          sources: srcs ? srcs.length : prev.sources,
          images: imgs ? imgs.length : prev.images,
        })),
    );
    return () => {
      live = false;
    };
  }, [stepId]);
  // Site config feeds the Integrations step's own derivation (SCM/mirror/proxy
  // rows) — read-only here now that the three corporate-baseline steps that
  // used to write it (Host Proxy / SCM Provider / Artifact Redirect) are gone
  // from the funnel; they're embedded in the Integrations "Add integration"
  // dialog instead, which owns its own local copy for editing.
  const [siteConfig, setSiteConfig] = React.useState<SiteConfig | null>(null);
  const [newRunOpen, setNewRunOpen] = React.useState(false);
  // Default-barrier pick (E3). Null until an explicit click — until then the
  // effective selection is the resolved default (persisted pick if this host runs
  // it, else strongest available). Clicking a ready card both selects and persists.
  const [ccOverride, setCcOverride] = React.useState<ConfinementClass | null>(null);
  const selectDefault = React.useCallback((cc: ConfinementClass) => {
    setCcOverride(cc);
    setDefaultCc(cc);
  }, []);

  // THE single navigation funnel (A4): every Next/Back/rail-jump/in-step-jump
  // goes through this instead of raw setStepId, so leaving a step can be
  // recorded exactly once. "Leaving" counts in every direction — going Back off
  // a step still means you've seen it — simplest honest rule, no special-casing.
  // One exception touches more than the visited set: moving FORWARD past
  // Integrations with nothing connected IS the skip (the step is optional and
  // has no skip button of its own — Next is the one forward affordance), so it
  // marks the same per-browser decision the old explicit control did and the
  // step reads Skipped with its checkmark. Backing off it decides nothing.
  const integrationsCountRef = React.useRef(0);
  // corpNetworkGate's latest `.on` (steps.ts) — read by selectStep below so a
  // rail jump obeys the SAME rule as the footer's Next button: "There is no
  // click-past" (steps.ts's own stated invariant) means every forward exit
  // from Corporate network, not just the one button. Updated wherever
  // corpGateResult itself is computed (below); defaults open so a rail click
  // is never blocked before that first computation lands.
  const corpGateOnRef = React.useRef(true);
  const selectStep = React.useCallback(
    (next: SetupStepId) => {
      if (
        stepId === "corp_network" &&
        STEP_ORDER.indexOf(next) > STEP_ORDER.indexOf("corp_network") &&
        !corpGateOnRef.current
      ) {
        return; // same block the footer's Next enforces — no click-past via the rail either
      }
      if (next !== stepId) {
        markStepVisited(stepId);
        setVisitedSteps((s) => (s.has(stepId) ? s : new Set(s).add(stepId)));
        if (
          stepId === "integrations" &&
          STEP_ORDER.indexOf(next) > STEP_ORDER.indexOf("integrations") &&
          integrationsCountRef.current === 0
        ) {
          markIntegrationsSkipped();
          setSkippedIntegrations(true);
        }
      }
      setStepId(next);
    },
    [stepId],
  );

  // Read-only fetch — the orchestrator only needs SiteConfig to derive the
  // Integrations badge count; every WRITE to it now happens inside the
  // embedded Integrations step's own "Add integration" dialog, which keeps
  // its own local copy (see integrations/add-integration-dialog.tsx).
  const reloadSiteConfig = React.useCallback(() => {
    return healthApi.getSiteConfig().then(setSiteConfig).catch(() => {});
  }, []);

  // The one write path into SiteConfig from within Getting Started — today
  // only Corporate network uses it (Host proxy / Egress redirection saves).
  // PUTs then re-GETs so this orchestrator's own copy — and the rail badges
  // derived from it — never goes stale after an in-step save.
  const saveSiteConfig = React.useCallback(
    async (next: SiteConfig) => {
      await healthApi.putSiteConfig(next);
      await reloadSiteConfig();
    },
    [reloadSiteConfig],
  );

  const loadSecrets = React.useCallback(() => {
    secretsApi.listSecrets().then(setSecretNames).catch(() => setSecretNames([]));
  }, []);

  const recheck = React.useCallback(() => {
    setRechecking(true);
    // Resync SiteConfig too (F2): the rail's Integrations badge count is
    // derived from it (via deriveIntegrations), so mount (via this recheck)
    // and every manual Re-check pull it — a failure leaves the last-known
    // config (or the initial null) in place, never clobbers it. This is the
    // ORCHESTRATOR'S sole GET path.
    reloadSiteConfig();
    // Secret names feed the SAME Integrations badge (deriveIntegrations) —
    // without this, adding/deleting a secret-backed integration inside the
    // embedded step never reaches the rail, which keeps reading the
    // mount-time snapshot until a full page reload.
    loadSecrets();
    return setupApi
      .getSetupStatus()
      .then((s) => {
        setStatus(s);
        setLastCheckedAt(new Date());
        // A fresh probe landed — bump the token EnvironmentStep watches.
        setRecheckCount((n) => n + 1);
      })
      .finally(() => setRechecking(false));
  }, [reloadSiteConfig, loadSecrets]);

  React.useEffect(() => {
    recheck(); // also performs the initial SiteConfig + secrets GET (see recheck)
    loadWorkspaces();
    // run once on mount — the loaders are stable (useCallback([]))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const finish = React.useCallback(() => {
    dismissSetup();
    onDone();
  }, [onDone]);

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
  //
  // HIGH-4 guard: a member's redacted SetupStatus always reports
  // confinement_classes: [] (redactSetupStatusForMember) — resolveDefaultCc
  // would floor that to CC1, and persisting it here would silently downgrade
  // the STORED default for anyone sharing this browser profile (this
  // localStorage key isn't per-role) the moment a member is ever the first to
  // land on this screen. Only an operator's fully-informed, non-empty class
  // list may seed the initial persisted default.
  React.useEffect(() => {
    if (status && status.runner.confinement_classes.length > 0 && !getDefaultCc()) setDefaultCc(selectedCc);
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

  // The one number the rail's Integrations badge and the embedded step body
  // both need — the SAME rows /integrations itself derives and totals
  // (integrations-screen.tsx's totalRows), so the funnel can never disagree
  // with that page: AI + SCM (deriveIntegrations) PLUS the eight generic
  // categories (genericIntegrations — pkg/registry/cloud/data/mcp/work/obs/
  // other). Host proxy / egress redirection are NOT counted here: those moved
  // to their own Corporate network step (its own "Ready · proxy + N redirects"
  // badge below) — counting them here too would double-count the same
  // configuration under two steps.
  const integrationsData = deriveIntegrations(status, siteConfig, secretNames);
  const integrationsCount =
    integrationsData.ai.length + integrationsData.scm.length + genericIntegrations(status).length;
  integrationsCountRef.current = integrationsCount;
  const corpRedirects = siteConfig?.egress_redirects ?? [];
  const corpNetwork: CorpNetworkState = {
    proxyConfigured: isProxyConfigured(siteConfig),
    proxyDetected: proxyDetected(status.host_proxy),
    redirectCount: corpRedirects.length,
    ...corpGate,
  };
  const badges = stepBadges(status, readiness, workspaces, integrationsCount, corpNetwork, corpRedirects, tierCounts.sources, tierCounts.images);
  const done = stepDone(status, readiness, workspaces, integrationsCount, corpNetwork, corpRedirects, tierCounts.sources, tierCounts.images);
  // Each demo sub-step earns its checkmark once THAT demo has been launched (a
  // per-browser signal kept out of the pure stepBadges/stepDone — see steps.ts).
  for (const id of DEMO_STEP_IDS) {
    if (launchedDemos.has(id)) {
      done[id] = true;
      badges[id] = { text: "Done · demo run", tone: "success" };
    }
  }
  // An explicitly-skipped (optional) Integrations step earns its checkmark — a
  // deliberate "nothing connected" decision reads as done, not as an unfinished
  // "Optional". A real connected integration always wins and shows its own
  // "Ready · N connected" badge (stepBadges/stepDone already handle that case).
  if (integrationsCount === 0 && skippedIntegrations) {
    done.integrations = true;
    badges.integrations = { text: "Skipped", tone: "neutral" };
  }
  // Corporate network has no skip override any more: it's mandatory, and
  // stepDone/stepBadges already read the real gate (corpNetworkGate)
  // above — no_runner is the only honest bypass, folded into that ladder.
  // A4: an optional step the operator navigated away from without configuring it
  // reads "Skipped" instead of a perpetual, un-acted-on "Optional" — a neutral
  // "you saw this and moved on" marker. Scoped to the exact still-default badge
  // (neutral "Optional") so anything the two overrides above (or stepBadges/
  // stepDone themselves) already upgraded — Done · demo run, a connected
  // integration's Ready, workspaces' In progress — wins outright and is left
  // alone.
  for (const id of OPTIONAL_STEPS) {
    if (
      visitedSteps.has(id) &&
      !done[id] &&
      badges[id].tone === "neutral" &&
      badges[id].text === "Optional"
    ) {
      badges[id] = { text: "Skipped", tone: "neutral" };
    }
  }

  // The Next-button gate — only Corporate network produces one today. Computed
  // fresh from the SAME corpNetwork/corpRedirects stepDone.corp_network above
  // already read, so the badge, the checkmark, and this can never disagree.
  // While the gate is OFF, its ACTION is the footer's button (in place of a
  // disabled Next — one launch point that names what it will do); while it is
  // ON a head/reason can still be present (no_runner / a custom-endpoint
  // pass) and renders as a neutral standing note beside the ENABLED button.
  const corpGateResult = stepId === "corp_network" ? corpNetworkGate(corpNetwork, corpRedirects, corpTab) : null;
  // Read by selectStep (a stable useCallback that can't see this render's
  // local const) so a rail click obeys the same gate — see corpGateOnRef.
  corpGateOnRef.current = corpGateResult ? corpGateResult.on : true;
  const dispatchCorpAction = (kind: CorpGateActionKind) => {
    const a = corpActionsRef.current;
    if (!a) return;
    if (kind === "probe") a.probe();
    else if (kind === "probe_custom") a.probeCustom();
    else if (kind === "open_egress") a.openEgress();
    else a.testRedirects();
  };
  // The forward walk passes THROUGH Egress redirection rather than over it
  // (navigation, not a gate — the visit rung stays deleted): from Host proxy
  // a rendered Next reads "Next: Egress redirection" and switches the tab —
  // including the disabled one while a probe is in flight, so the footer
  // already names where a pass will go; from Egress redirection it hands off
  // to Integrations as usual. Back mirrors it below.
  const corpOnProxyTab = stepId === "corp_network" && corpTab === "proxy";
  const nextGate = corpGateResult
    ? {
        blocked: !corpGateResult.on,
        head: corpGateResult.head,
        reason: corpGateResult.reason,
        tone: corpGateResult.tone,
        action: corpGateResult.action
          ? { label: corpGateResult.action.label, onClick: () => dispatchCorpAction(corpGateResult.action!.kind) }
          : undefined,
        nextLabel: corpOnProxyTab ? "Next: Egress redirection" : undefined,
        onNext: corpOnProxyTab ? () => setCorpTab("egress") : undefined,
      }
    : undefined;

  return (
    <>
      <SetupLayout
        current={stepId}
        rail={<PhaseRail current={stepId} badges={badges} done={done} onSelect={selectStep} />}
        checking={rechecking}
        lastCheckedLabel={lastCheckedLabel(lastCheckedAt)}
        onRecheck={recheck}
        onSelect={selectStep}
        onFinish={finish}
        nextGate={nextGate}
        backOverride={stepId === "corp_network" && corpTab === "egress" ? () => setCorpTab("proxy") : undefined}
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
        {stepId === "corp_network" && (
          <CorpNetworkStep
            status={status}
            siteConfig={siteConfig}
            reloadSiteConfig={reloadSiteConfig}
            saveSiteConfig={saveSiteConfig}
            gate={corpGate}
            onGateChange={onCorpGateChange}
            onRecheck={recheck}
            gateResult={corpGateResult ?? undefined}
            registerActions={(a) => {
              corpActionsRef.current = a;
            }}
            tab={corpTab}
            onTabChange={setCorpTab}
          />
        )}
        {stepId === "integrations" && <IntegrationsStep onRecheck={recheck} />}
        {DEMOS.some((d) => d.id === stepId) && (
          <React.Suspense
            fallback={<p className="text-sm text-muted-foreground">Loading demo…</p>}
          >
            <DemoDetail
              demo={DEMOS.find((d) => d.id === stepId)!}
              barrierReady={readiness.barrierReady}
              onJump={selectStep}
              onDemoLaunched={(id) => setLaunchedDemos((s) => new Set(s).add(id))}
            />
          </React.Suspense>
        )}
        {stepId === "sources" && <SourcesStep workspaces={workspaces} />}
        {stepId === "images" && <ImagesStep workspaces={workspaces} />}
        {stepId === "workspaces" && (
          <WorkspacesStep workspaces={workspaces} loading={wsLoading} onReload={loadWorkspaces} />
        )}
        {stepId === "review" && (
          <ReviewStep
            status={status}
            readiness={readiness}
            onRecheck={recheck}
            rechecking={rechecking}
            lastCheckedAt={lastCheckedAt}
            onJump={selectStep}
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

      <NewRunDialog
        open={newRunOpen}
        onOpenChange={setNewRunOpen}
        onCreated={(r) => {
          // One post-launch rule: every entry point lands on the run it
          // launched (§7) — supersedes onDone's own plain navigate("/runs")
          // (App.tsx) with the more specific detail-page destination.
          dismissSetup();
          navigate(`/runs/${encodeURIComponent(r.id)}`);
        }}
      />
    </>
  );
}
