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
// not here. NOT a gate: /setup is a route an operator chooses to visit (the
// account menu's "Getting started" entry, or the Runs empty state's guided-tour
// link) — nothing redirects here and no other route redirects away. `finish`
// below still dismisses the funnel's own "seen it" flag (setup-gate.ts), which
// is per-browser cosmetic state, not a lock on the rest of the console.
import * as React from "react";
import { useSearchParams } from "react-router-dom";
import type {
  ConfinementClass,
  SetupStatus,
  SiteConfig,
} from "../../../lib/types";
import { health as healthApi } from "../../../lib/api/health";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { setup as setupApi } from "../../../lib/api/setup";
import { access as accessApi } from "../../../lib/api/access";
import { HttpError } from "../../../lib/api/core";
import { deriveIntegrations } from "../../../lib/api/integrations";
import { useWorkspaceList } from "../../../lib/use-workspace-list";
import type { AccessResponse } from "../../../lib/types";
import type { AccessLoadState } from "./access-panel";
import {
  getDefaultCc,
  resolveDefaultCc,
  setDefaultCc,
} from "../../wardyn/default-confinement";
import { deploymentMode, deriveReadiness, lastCheckedLabel } from "../../../lib/readiness";
import { useOperator } from "../../wardyn/operator-context";
import { SetupLayout } from "./setup-layout";
import { PhaseRail } from "./phase-rail";
import { EnvironmentStep } from "./environment-step";
import {
  CorpNetworkStep,
  isProxyConfigured,
  proxyDetected,
  type CorpStepActions,
} from "./corp-network-step";
import { IntegrationsStep } from "./integrations-step";
import { DeploymentStep, ReviewStep, WorkspacesStep } from "./step-bodies";
import {
  DEMO_STEP_IDS,
  OPTIONAL_STEPS,
  STEP_ORDER,
  corpNetworkGate,
  stepBadges,
  stepDone,
  stepOrder,
  type CorpGateActionKind,
  type CorpNetworkGate,
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
// the operator opens a demo step (the reasoning the deleted /demos route had). The pure
// demo catalog + launched-set reader are imported eagerly above (xterm-free).
const DemoDetail = React.lazy(() => import("./demos-step"));

// ------------------------------------------------------------
// SetupScreen
// ------------------------------------------------------------
export function SetupScreen({ onDone }: { onDone: () => void }) {
  const operator = useOperator();
  // ?step=<id> deep-links a specific step — the Integrations page's
  // proxy-detected banner uses it to hand off to Corporate network, now the
  // only place a proxy is configured. Read once at mount (an unknown or
  // absent value just starts at the beginning); the URL is not kept in sync
  // afterwards, since the rail is the navigation from then on.
  const [searchParams] = useSearchParams();
  const [stepId, setStepId] = React.useState<SetupStepId>(() => {
    const want = searchParams.get("step");
    // Validated against the FULL order — status (and so which conditional demo
    // steps survive) isn't known yet at mount, which is exactly what
    // stepOrder(null) returns. An unmet step that slips through here is pulled
    // back by the re-correct effect below the moment status lands.
    return want && (stepOrder(null) as string[]).includes(want)
      ? (want as SetupStepId)
      : "environment";
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
  const [skippedIntegrations, setSkippedIntegrations] = React.useState(
    integrationsSkipped(),
  );
  // Corporate network's gate proof (proxy probe / egress-tab visit / per-redirect
  // tests) — held HERE, not inside CorpNetworkStep, because that component
  // unmounts on navigation and this must survive leaving and re-entering the
  // step (see steps.ts's CorpNetworkState doc). Deliberately in-memory only,
  // never persisted: a stale "reached" surviving a reload would be exactly the
  // false reassurance this whole feature exists to prevent.
  const [corpGate, setCorpGate] = React.useState<
    Pick<
      CorpNetworkState,
      "proxyProbe" | "probeRunning" | "customDraft" | "redirectProbes"
    >
  >({
    probeRunning: false,
    customDraft: "",
    redirectProbes: {},
  });
  const onCorpGateChange = React.useCallback(
    (
      patch: Partial<
        Pick<
          CorpNetworkState,
          "proxyProbe" | "probeRunning" | "customDraft" | "redirectProbes"
        >
      >,
    ) => {
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
  const {
    workspaces,
    loading: wsLoading,
    reload: loadWorkspaces,
  } = useWorkspaceList();
  // Site config feeds the Integrations step's own derivation (SCM/mirror/proxy
  // rows) — read-only here now that the three corporate-baseline steps that
  // used to write it (Host Proxy / SCM Provider / Artifact Redirect) are gone
  // from the funnel; they're embedded in the Integrations "Add integration"
  // dialog instead, which owns its own local copy for editing.
  const [siteConfig, setSiteConfig] = React.useState<SiteConfig | null>(null);
  // Role-mappings acting surface (0.7 SSO Phase 3) — People's multi-user
  // branch. Owned here, same split every other status-derived fetch on this
  // screen follows (siteConfig, secrets): DeploymentStep only renders it.
  // "loading" until the first fetch settles; recheck() re-fetches it
  // alongside status/siteConfig/secrets whenever the CURRENT status reads
  // multi-user.
  const [access, setAccess] = React.useState<AccessResponse | null>(null);
  const [accessState, setAccessState] = React.useState<AccessLoadState>("loading");
  const loadAccess = React.useCallback(() => {
    setAccessState((s) => (s === "ready" ? s : "loading"));
    return accessApi
      .getAccess()
      .then((data) => {
        setAccess(data);
        setAccessState("ready");
      })
      .catch((e) => {
        setAccess(null);
        // 503 (requireOIDC's writeError) is a DISTINCT state from any other
        // fetch failure — see access-panel.tsx's ACCESS_STATE.SSO_UNAVAILABLE_*
        // vs FETCH_FAILED_*; containment, not discard, per §2.3.
        setAccessState(e instanceof HttpError && e.status === 503 ? "sso_unavailable" : "fetch_failed");
      });
  }, []);
  // Default-barrier pick (E3). Null until an explicit click — until then the
  // effective selection is the resolved default (persisted pick if this host runs
  // it, else strongest available). Clicking a ready card both selects and persists.
  const [ccOverride, setCcOverride] = React.useState<ConfinementClass | null>(
    null,
  );
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
  // corpNetworkGate's latest verdict (steps.ts) — read by refuseSelect below so
  // a rail jump (or the ?step= deep-link init effect further down) obeys the
  // SAME rule as the footer's Next button: "There is no click-past" (steps.ts's
  // own stated invariant) means every forward jump past Corporate network from
  // ANYWHERE, not just a Next click while that step is the one on screen —
  // W2-S1-2: the guard used to be scoped to `stepId === "corp_network"` and
  // this ref reset to open on every render taken off that step, so a rail
  // jump (or deep link) FROM an earlier step straight past it never saw the
  // real gate at all. Updated unconditionally wherever corpNetwork itself is
  // computed (below); defaults open so a rail click is never blocked before
  // that first computation lands. The whole gate, not just `.on`: a refused
  // Next renders disabled with the gate's own `reason` as its title.
  const corpGateRef = React.useRef<CorpNetworkGate>({ on: true });

  // THE crossing predicate — why a move to `next` is refused, or undefined
  // when it's allowed. Shared by selectStep (rail clicks, in-step jumps, the
  // footer's Next) and by SetupLayout, which renders a refused Next DISABLED
  // with this reason instead of a live button whose click silently no-ops.
  //
  // CROSSING-based, not target-index-based. A "is the target past
  // corp_network?" test looks equivalent and isn't: `integrations` also
  // indexes past it, so that version dead-ends the operator ON a demo — no way
  // back to Integrations, and every rail click refused. What the gate actually
  // forbids is CROSSING it, so three cases are always free:
  //  - anything at or before where you already are (Back, and re-entering a
  //    step you've reached, decide nothing new);
  //  - any demo step — a shared demo link has to open the demo. Demos gate
  //    their own Start on barrierReady, so nothing unsafe opens; the accepted
  //    trade is that a cold session can reach a demo and Back into the
  //    optional Integrations step without the network proof;
  //  - anything at or before corp_network itself.
  // Workspaces and Review stay gated, which is the part that matters.
  const refuseSelect = React.useCallback(
    (next: SetupStepId, from: SetupStepId = stepId): string | undefined => {
      const gate = corpGateRef.current;
      if (gate.on) return undefined;
      const order = stepOrder(status);
      if (order.indexOf(next) <= order.indexOf(from)) return undefined;
      if (DEMOS.some((d) => d.id === next)) return undefined;
      if (order.indexOf(next) <= order.indexOf("corp_network"))
        return undefined;
      return gate.reason;
    },
    [stepId, status],
  );
  const canSelect = React.useCallback(
    (next: SetupStepId) => refuseSelect(next) === undefined,
    [refuseSelect],
  );

  const selectStep = React.useCallback(
    (next: SetupStepId) => {
      if (!canSelect(next)) {
        return; // same block the footer's Next enforces — no click-past via the rail either
      }
      if (next !== stepId) {
        markStepVisited(stepId);
        setVisitedSteps((s) => (s.has(stepId) ? s : new Set(s).add(stepId)));
        const order = stepOrder(status);
        if (
          stepId === "integrations" &&
          order.indexOf(next) > order.indexOf("integrations") &&
          integrationsCountRef.current === 0
        ) {
          markIntegrationsSkipped();
          setSkippedIntegrations(true);
        }
      }
      setStepId(next);
    },
    [stepId, status, canSelect],
  );

  // LOW-3 (secrets-demos plan): same-route ?step= navigations. The param is
  // read once at mount, so a nav to /setup?step=<id> while ALREADY on /setup
  // (the app-shell "Demos" entry, runs-first-run cards) moved the URL and not
  // the step — a silent no-op. Route later param changes through selectStep,
  // which already enforces canSelect and the visited bookkeeping.
  React.useEffect(() => {
    const want = searchParams.get("step");
    if (
      want &&
      want !== stepId &&
      (stepOrder(status) as string[]).includes(want)
    ) {
      selectStep(want as SetupStepId);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- only param changes re-run this
  }, [searchParams]);

  // Read-only fetch — the orchestrator only needs SiteConfig to derive the
  // Integrations badge count; every WRITE to it now happens inside the
  // embedded Integrations step's own "Add integration" dialog, which keeps
  // its own local copy (see integrations/add-integration-dialog.tsx).
  const reloadSiteConfig = React.useCallback(() => {
    return healthApi
      .getSiteConfig()
      .then(setSiteConfig)
      .catch(() => {});
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
    secretsApi
      .listSecrets()
      .then(setSecretNames)
      .catch(() => setSecretNames([]));
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
        // Gated on the FRESH status, not the stale one this closure closed
        // over — People's multi-user branch is the only reader, and a
        // single-user deployment never needs this fetch at all.
        if (deploymentMode(s) === "multi-user") void loadAccess();
      })
      .finally(() => setRechecking(false));
  }, [reloadSiteConfig, loadSecrets, loadAccess]);

  React.useEffect(() => {
    recheck(); // also performs the initial SiteConfig + secrets GET (see recheck)
    loadWorkspaces();
    // run once on mount — the loaders are stable (useCallback([]))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // W2-S1-2: a `?step=` deep link is read once at mount (the initializer
  // above), BEFORE status — and so the corp_network gate — is known. An
  // operator pasting/bookmarking a link past it must not skip the same
  // mandatory proof a rail jump can't skip either (selectStep's guard,
  // above) — correct it the first time the gate becomes knowable. Runs once
  // (the ref latch): after that, staying on/returning to a later step is
  // legitimate forward progress, not a link to re-validate.
  //
  // Evaluated from the funnel's START, not from the current step: the crossing
  // predicate treats "where you already are" as free, and here the link IS
  // where you are — asking it about a cold landing means asking whether
  // walking to it from step one would have been allowed.
  const initialDeepLinkCheckedRef = React.useRef(false);
  React.useEffect(() => {
    if (initialDeepLinkCheckedRef.current || !status) return;
    initialDeepLinkCheckedRef.current = true;
    if (refuseSelect(stepId, "environment")) setStepId("corp_network");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [status]);

  // …and the OTHER correction, deliberately its own effect and deliberately
  // UN-LATCHED: a conditional demo step can leave the walk at any time (the
  // operator deletes the demo secret on /secrets, a model disconnects), not
  // just at mount. If the step on screen is no longer in stepOrder(status),
  // indexOf(current) === -1 and the shell's "Step N of M"/prev/next and the
  // rail's active state all quietly break. Fall BACK to the nearest surviving
  // step — never forward, which would advance the operator past something they
  // hadn't finished. Folding this into the latched effect above reproduces
  // exactly the breakage it exists to prevent (it would fire once and never
  // again), which is why they stay two.
  React.useEffect(() => {
    if (!status) return;
    const order = stepOrder(status);
    if (order.includes(stepId)) return;
    const before = STEP_ORDER.slice(0, STEP_ORDER.indexOf(stepId));
    setStepId(
      [...before].reverse().find((id) => order.includes(id)) ?? order[0],
    );
  }, [status, stepId]);

  const finish = React.useCallback(() => {
    // The INSTALL records completion (POST /setup/onboarding-complete,
    // idempotent, audited). The per-browser flag is still written as a legacy
    // fallback: an older daemon omits onboarding_complete from /setup/status,
    // and firstRunLanding treats absent as not-onboarded — without the local
    // flag such a daemon would re-open the tour on every visit forever.
    void setupApi.completeOnboarding();
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
    if (
      status &&
      status.runner.confinement_classes.length > 0 &&
      !getDefaultCc()
    )
      setDefaultCc(selectedCc);
  }, [status, selectedCc]);

  if (!status || !readiness) {
    return (
      <div className="mx-auto w-full max-w-[1200px] px-6 py-8">
        <p className="text-sm text-muted-foreground">
          Checking Wardyn&apos;s setup…
        </p>
      </div>
    );
  }

  // A manually-opened funnel against a daemon that didn't answer: say THAT,
  // instead of rendering step bodies (no-runner danger card, "Needs setup"
  // badges) built from the synthetic fallback's made-up fields.
  if (status.unreachable) {
    return (
      <div className="mx-auto w-full max-w-[1200px] px-6 py-8">
        <h1 className="text-lg font-semibold text-foreground">
          Getting started
        </h1>
        <div className="mt-4 max-w-xl rounded-lg border border-border bg-muted/40 p-4">
          <p className="text-sm text-foreground">Couldn&apos;t reach Wardyn.</p>
          <p className="mt-1 text-sm leading-relaxed text-muted-foreground">
            The setup status request didn&apos;t get an answer, so nothing on
            this page would be trustworthy. Check that wardynd is running (
            <code className="rounded bg-background/70 px-1 py-0.5 text-xs">
              make setup
            </code>
            , logs in{" "}
            <code className="rounded bg-background/70 px-1 py-0.5 text-xs">
              ~/.wardyn/host-wardynd.log
            </code>
            ), then re-check.
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

  // The one number the rail's "Model & git host" badge needs: AI + SCM, the two
  // categories the step's two cards actually configure. It used to add a third
  // term for the eight GENERIC categories (package feeds, registries, cloud,
  // data, MCP, work tracking, observability, other) that the old /integrations
  // catalog could hold — those kinds are gone in 0.5, so counting them would be
  // counting something that can no longer exist.
  //
  // Host proxy / egress redirection are NOT counted here: those moved to their
  // own Corporate network step (its own "Ready · proxy + N redirects" badge
  // below) — counting them here too would double-count one configuration under
  // two steps.
  const integrationsData = deriveIntegrations(status, siteConfig, secretNames);
  const integrationsCount =
    integrationsData.ai.length + integrationsData.scm.length;
  integrationsCountRef.current = integrationsCount;
  const corpRedirects = siteConfig?.egress_redirects ?? [];
  const corpNetwork: CorpNetworkState = {
    proxyConfigured: isProxyConfigured(siteConfig),
    proxyDetected: proxyDetected(status.host_proxy),
    redirectCount: corpRedirects.length,
    ...corpGate,
  };
  // Unconditional (not gated on `stepId === "corp_network"`): refuseSelect and the
  // deep-link init effect above both need the REAL gate state no matter which
  // step is on screen right now (see corpGateRef's declaration for why the
  // old stepId-scoped version was the bug).
  corpGateRef.current = corpNetworkGate(corpNetwork, corpRedirects);
  // The steps actually walkable against THIS readiness — a demo whose
  // needsModel/needsSecret precondition is unmet is dropped, so neither the
  // rail nor the footer offers a step whose Start is closed. badges/done stay
  // keyed by the full STEP_ORDER: a dropped step just never gets rendered.
  const walkOrder = stepOrder(status);
  const badges = stepBadges(
    status,
    readiness,
    workspaces,
    integrationsCount,
    corpNetwork,
    corpRedirects,
  );
  const done = stepDone(
    status,
    readiness,
    workspaces,
    integrationsCount,
    corpNetwork,
    corpRedirects,
  );
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
  // above — no_runner / not_run are the only honest bypasses, folded into that ladder.
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
  const corpGateResult =
    stepId === "corp_network"
      ? corpNetworkGate(corpNetwork, corpRedirects, corpTab)
      : null;
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
          ? {
              label: corpGateResult.action.label,
              onClick: () => dispatchCorpAction(corpGateResult.action!.kind),
            }
          : undefined,
        nextLabel: corpOnProxyTab ? "Next: Egress redirection" : undefined,
        onNext: corpOnProxyTab ? () => setCorpTab("egress") : undefined,
      }
    : undefined;

  return (
    <>
      <SetupLayout
        current={stepId}
        order={walkOrder}
        refuseNext={refuseSelect}
        rail={
          <PhaseRail
            current={stepId}
            badges={badges}
            done={done}
            onSelect={selectStep}
            order={walkOrder}
          />
        }
        checking={rechecking}
        lastCheckedLabel={lastCheckedLabel(lastCheckedAt)}
        onRecheck={recheck}
        onSelect={selectStep}
        onFinish={finish}
        nextGate={nextGate}
        backOverride={
          stepId === "corp_network" && corpTab === "egress"
            ? () => setCorpTab("proxy")
            : undefined
        }
        operator={operator}
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
        {stepId === "people" && (
          <DeploymentStep status={status} access={access} accessState={accessState} onReloadAccess={loadAccess} />
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
            secretNames={secretNames}
            registerActions={(a) => {
              corpActionsRef.current = a;
            }}
            tab={corpTab}
            onTabChange={setCorpTab}
          />
        )}
        {stepId === "integrations" && (
          <IntegrationsStep
            status={status}
            siteConfig={siteConfig}
            onRecheck={recheck}
          />
        )}
        {DEMOS.some((d) => d.id === stepId) && (
          <React.Suspense
            fallback={
              <p className="text-sm text-muted-foreground">Loading demo…</p>
            }
          >
            <DemoDetail
              demo={DEMOS.find((d) => d.id === stepId)!}
              barrierReady={readiness.barrierReady}
              githubAppReady={status ? !!status.secrets.github_app : true}
              onJump={selectStep}
              onDemoLaunched={(id) =>
                setLaunchedDemos((s) => new Set(s).add(id))
              }
            />
          </React.Suspense>
        )}
        {stepId === "workspaces" && (
          <WorkspacesStep
            workspaces={workspaces}
            loading={wsLoading}
            onReload={loadWorkspaces}
          />
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
      </SetupLayout>

      {/* The NewRunDialog lived here, opened only by the deleted Launch step.
          Launching is the top bar's "New run" on every screen, so the funnel no
          longer carries its own copy — and `finish` (Review's "Finish setup")
          is what retires the funnel now. */}
    </>
  );
}
