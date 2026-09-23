/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SetupScreen — the "Getting started" first-run funnel ORCHESTRATOR. It owns the
// SetupStatus fetch + re-check, the workspace/secret/site-config loads, and
// every dialog; the SHELL (header, dismissible
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
import { toast } from "sonner";
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
import { resolveDefaultCc } from "../../wardyn/default-confinement";
import { deploymentMode, deriveReadiness, lastCheckedLabel } from "../../../lib/readiness";
import { useOperator, useOperatorResolved } from "../../wardyn/operator-context";
import { useShellSetupStatus } from "../../wardyn/model-access-context";
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
import { ProvidersCard } from "./providers-card";
import { providers as providersApi } from "../../../lib/api/providers";
import { PROVIDERS, PROVIDERS_DRAFT } from "../../../lib/workspace-providers-copy";
import { SITE } from "../../wardyn/copy";
import { DeploymentStep, ReviewStep, WorkspacesStep } from "./step-bodies";
import {
  DEMO_STEP_IDS,
  OPTIONAL_STEPS,
  REQUIRED_STEPS,
  STEP_ORDER,
  corpNetworkGate,
  optionalStepCounts,
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
  clearStaleVisitFlagsOnce,
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

export function SetupScreen({ onDone }: { onDone: () => void }) {
  const operator = useOperator();
  // Defence in depth: this screen only ever mounts once App.tsx's SetupRoute
  // has let roleResolved through AND role === "admin" (onboarding-screen.tsx),
  // and app-shell.tsx already paints no route at all on a settled-but-unknown
  // identity (pinned by app-shell-identity-routes.test.tsx) — so operator
  // cannot actually be false on the path that reaches here. This holds if
  // that ever changes: the admin-only reads below (reloadSiteConfig /
  // loadSecrets / loadProviderCount) still gate on the caller being a
  // RESOLVED admin, not just "not yet known to be a member" (see
  // loadProviderCount's own precedent below).
  const operatorResolved = useOperatorResolved();
  const adminReads = operatorResolved && operator;
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
  // Count of ENABLED git-provider rows (§7.5: "counts ENABLED rows, never
  // hosts") — feeds the `providers` step's own badge/done rule, the same
  // shape `integrationsCount` feeds `integrations`'s.
  const [providerCount, setProviderCount] = React.useState(0);
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
  // #492: the last GET's (or the last successful PUT's) ETag — sent back as
  // If-Match on the one write path below, the same discipline
  // sign-in-help-card.tsx already applies to its own two site-config fields.
  // Without this, saveSiteConfig PUT the whole document from a GET that could
  // already be stale (another tab's scm_hosts/egress_redirects save, or that
  // card's own sign_in_help_* save) and silently reverted it.
  const [siteConfigEtag, setSiteConfigEtag] = React.useState<string | null>(null);
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
  // A role-mapping write can end the everyone-is-an-admin state, and the
  // shell's banner reads the SHELL's /setup/status (polled every few minutes),
  // not this screen's — so every access reload re-reads that one too.
  const { refresh: refreshShellStatus } = useShellSetupStatus();
  const reloadAccessAndShell = React.useCallback(() => {
    void loadAccess();
    void refreshShellStatus();
  }, [loadAccess, refreshShellStatus]);
  // Default-barrier pick (E3), IN-SESSION only (0.7.8: the default is a
  // server fact — the strongest installed class at or above the policy floor
  // — so there is nothing left to persist here). Null until an explicit
  // click — until then the effective selection is the resolved default
  // (this pick if this host still runs it, else strongest available).
  const [ccOverride, setCcOverride] = React.useState<ConfinementClass | null>(
    null,
  );
  const selectDefault = React.useCallback((cc: ConfinementClass) => {
    setCcOverride(cc);
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
  // The guard must not be scoped to `stepId === "corp_network"` —
  // a ref that only reset to open on renders taken off that step would leave
  // a rail jump (or deep link) FROM an earlier step straight past it, never
  // seeing the real gate at all. Updated unconditionally wherever corpNetwork
  // itself is computed (below); defaults open so a rail click is never
  // blocked before that first computation lands. The whole gate, not just
  // `.on`: a refused Next renders disabled with the gate's own `reason` as
  // its title.
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

  // Same-route ?step= navigations must not silently no-op. The param is
  // read once at mount, so a nav to /setup?step=<id> while ALREADY on /setup
  // (the app-shell "Demos" entry, runs-first-run cards) would move the URL
  // and not the step. Route later param changes through selectStep, which
  // already enforces canSelect and the visited bookkeeping.
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
  // its own local copy (see integrations/add-integration-dialog.tsx). Uses
  // the ETag-carrying snapshot (not plain getSiteConfig) so the write path
  // below always has the CURRENT document's ETag to send as If-Match, even
  // though most callers of reloadSiteConfig never write anything themselves.
  const reloadSiteConfig = React.useCallback(() => {
    if (!adminReads) return Promise.resolve();
    return healthApi
      .getSiteConfigSnapshot()
      .then(({ siteConfig, etag }) => {
        setSiteConfig(siteConfig);
        setSiteConfigEtag(etag);
      })
      .catch(() => {});
  }, [adminReads]);

  // The one write path into SiteConfig from within Getting Started — today
  // only Corporate network uses it (Host proxy / Egress redirection saves).
  // PUTs then re-GETs so this orchestrator's own copy — and the rail badges
  // derived from it — never goes stale after an in-step save.
  //
  // F6-F6 (Appendix A V8): the write's four advisory signals must reach the
  // caller — a putSiteConfig that returned void would tell an admin naming a
  // secret ref nothing stores that the save landed cleanly. These two
  // warnings are ADDITIONAL toasts, stacked above whatever success toast the
  // saving step itself already shows (corp-network-proxy.tsx /
  // corp-network-egress.tsx) — never a replacement for it, so a clean save's
  // plain success is untouched.
  //
  // #492: sends the last reload's ETag as If-Match (sign-in-help-card.tsx's
  // own discipline, applied here) — a PUT built from a document that changed
  // underneath since that reload is refused 412, not silently accepted. On a
  // 412, reloadSiteConfig() refreshes this orchestrator's copy (and its ETag,
  // for a retry) but the step's own draft is left exactly where the operator
  // left it: every corp-network field seeds from the `siteConfig` PROP once
  // (its own seededRef guard), never on a later prop update, so this reload
  // can't clobber mid-typed input. mutate() (step-bodies.tsx's
  // useSiteConfigStep) still shows its own toast for the failure — thrown as
  // a plain Error here only so its description is the human sentence, not the
  // server's raw If-Match refusal text.
  const saveSiteConfig = React.useCallback(
    async (next: SiteConfig) => {
      try {
        const result = await healthApi.putSiteConfig(next, siteConfigEtag);
        await reloadSiteConfig();
        if (result.danglingSecretRefs.length > 0) {
          toast.warning(PROVIDERS_DRAFT.SAVED_DANGLING_REFS(result.danglingSecretRefs));
        }
        // A POINTER on the wire: only a PRESENT positive number is a narrowed-
        // sources warning — never `?? 0`, which would claim "narrowed nothing"
        // for a save (this screen's own) that named no provider block at all.
        if (typeof result.sourcesNoLongerAdmitted === "number" && result.sourcesNoLongerAdmitted > 0) {
          toast.warning(PROVIDERS.SAVED_NARROWED(result.sourcesNoLongerAdmitted));
        }
      } catch (e) {
        if (e instanceof HttpError && e.status === 412) {
          await reloadSiteConfig();
          throw new Error(SITE.SAVED_ELSEWHERE);
        }
        throw e;
      }
    },
    [reloadSiteConfig, siteConfigEtag],
  );

  const loadSecrets = React.useCallback(() => {
    if (!adminReads) return Promise.resolve();
    return secretsApi
      .listSecrets()
      .then(setSecretNames)
      .catch(() => setSecretNames([]));
  }, [adminReads]);

  // GET /workspace-providers is operatorOnly — gated on `operatorResolved &&
  // operator` so a security admin or a member does not take a swallowed 403
  // on every recheck (this funnel runs for every caller); the count just
  // stays 0 (Optional) for them either way. The guard is the shared
  // `adminReads`, not plain `!operator` alone, for the same defence-in-
  // depth reason `operator`'s own definition above explains — this screen
  // cannot actually mount with operator false today (app-shell.tsx paints no
  // route at all on a settled-but-unknown identity), but the guard holds if
  // that ever changes.
  const loadProviderCount = React.useCallback(() => {
    if (!adminReads) return Promise.resolve();
    return providersApi
      .getWorkspaceProviders()
      .then(({ providers }) => setProviderCount((providers.git ?? []).filter((r) => !r.disabled).length))
      .catch(() => setProviderCount(0));
  }, [adminReads]);

  // R-09: the last host_proxy payload this screen has seen, serialized. The
  // baseline is the mount read; every read (forced or not) updates it.
  const hostProxySeenRef = React.useRef<string | null>(null);
  const recheck = React.useCallback((opts?: { force?: boolean }) => {
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
    loadProviderCount();
    return setupApi
      .getSetupStatus({ recheck: opts?.force })
      .then((s) => {
        setStatus(s);
        // U2-02 (blind round 2, lens-U2): ONLY the forced read carries a fresh
        // host sweep — the daemon answers every other /setup/status from its
        // 30s host-proxy memo, and only the ?recheck=1 path waits on the
        // re-detect it kicks off (internal/api/hostproxy_cache.go). Stamping
        // Stamping "Checked just now" beside a memoized "none detected" would
        // tell the operator the host had just been looked at when it had
        // not, which is precisely the complaint Re-check exists to answer.
        // No stamp beats a false one: lastCheckedLabel(null) is the empty
        // string.
        //
        // R-09: and the forced read is not proof on its own — hostProxyRecheck
        // waits hostProxyRecheckWait (2s) for the sweep it started and then
        // answers with LAST-KNOWN anyway, so a wedged host returns the same
        // unchanged payload the poll would have returned. The response carries
        // no freshness field (no checked_at, no stale flag) to tell the two
        // apart, so the console uses the only evidence it has: the host_proxy
        // payload MOVING. It moved ⇒ the sweep landed ⇒ stamp. It did not ⇒
        // leave the previous stamp standing rather than refresh it.
        //
        // ponytail: this understates — a real sweep that re-finds the SAME
        // proxy also leaves the stamp alone, which reads as "not checked
        // recently" rather than as a lie. Understating is the safe direction
        // here; the exact fix is one freshness field on HostProxyDetection
        // (server-side, fix-s2's), and this whole branch collapses to
        // `if (opts?.force && s.host_proxy?.checked_at !== seen)` when it lands.
        const hostProxySeen = JSON.stringify(s.host_proxy ?? null);
        const hostProxyMoved = hostProxySeenRef.current !== null && hostProxySeen !== hostProxySeenRef.current;
        hostProxySeenRef.current = hostProxySeen;
        if (opts?.force && hostProxyMoved) setLastCheckedAt(new Date());
        // A fresh probe landed — bump the token EnvironmentStep watches.
        setRecheckCount((n) => n + 1);
        // Gated on the FRESH status, not the stale one this closure closed
        // over — People's multi-user branch is the only reader, and a
        // single-user deployment never needs this fetch at all.
        if (deploymentMode(s) === "multi-user") void loadAccess();
      })
      .finally(() => setRechecking(false));
  }, [reloadSiteConfig, loadSecrets, loadProviderCount, loadAccess]);

  // The operator PRESSED Re-check, as opposed to the mount fetch and the
  // post-save refreshes that share the loader above: only a press asks the
  // daemon to look at the host again (the host-proxy memo is 30s-lived, and a
  // proxy configured ten seconds ago must be findable on the first press, not
  // the second). Its own callback rather than an inline lambda so the button
  // handlers stay referentially stable — and so no `onClick={recheck}` can ever
  // hand a MouseEvent in where the force flag goes.
  const forceRecheck = React.useCallback(() => recheck({ force: true }), [recheck]);

  React.useEffect(() => {
    recheck(); // also performs the initial SiteConfig + secrets GET (see recheck)
    loadWorkspaces();
    // run once on mount — recheck/loadWorkspaces are no longer identity-stable
    // ([adminReads] now rides through recheck's own dep chain), but adminReads
    // itself is invariant for the life of this mount: this screen only mounts
    // once App.tsx's SetupRoute has let the real, RESOLVED role through
    // (app-shell.tsx's OperatorProvider/RoleProvider both flip from the same
    // settled /me read), so there is no later tick where a fresh recheck()
    // would need to fire on adminReads' account.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // A `?step=` deep link is read once at mount (the initializer
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

  // Once per PAGE LOAD (clearStaleVisitFlagsOnce's own module latch —
  // NOT a per-mount ref: this screen can unmount/remount within one load, and
  // a mark the operator set a moment ago in THIS still-fresh session must
  // survive that), the first time status lands — a fresh install (never
  // onboarded, no runs yet) clears any stale wardyn-integrations-skipped /
  // wardyn-setup-visited left by a PREVIOUS install on this browser, so the
  // rail doesn't show a false-green "Skipped" for a step nobody has actually
  // visited this time. An install with real history (has_runs / onboarded)
  // leaves both flags alone.
  React.useEffect(() => {
    if (!status) return;
    if (clearStaleVisitFlagsOnce(status)) {
      setSkippedIntegrations(false);
      setVisitedSteps(new Set());
    }
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
  // Effective default-barrier selection: the explicit click this session if
  // any, else the strongest available — re-resolved against LIVE
  // availability, so a class that vanishes on a recheck (e.g. Docker stops
  // mid-session) degrades to the strongest available card instead of leaving
  // zero cards selected. There is nothing to persist across sessions: the
  // default is a server fact (0.7.8's strongest-installed-at-or-above-the-
  // floor rule), so a member landing here first can no longer downgrade a
  // shared browser's stored pick the way the old HIGH-4 guard had to defend
  // against — there is no stored pick left to downgrade.
  const selectedCc = resolveDefaultCc(
    ccOverride,
    status?.runner.confinement_classes ?? [],
  );

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
            onClick={forceRecheck}
            disabled={rechecking}
          >
            {rechecking ? "Re-checking…" : "Re-check"}
          </button>
        </div>
      </div>
    );
  }

  // The one number the Secrets rail badge needs: AI only, since 0.7.2 (the git
  // credential lanes moved to the `providers` step/screen, whose OWN badge now
  // counts enabled provider rows — see providerCount below). SCM's count is
  // not folded in here: GitHostCard, which used to carry a git credential on
  // THIS step, is retired (workspace-providers-prompt.md §2.1).
  const integrationsData = deriveIntegrations(status, siteConfig, secretNames);
  const integrationsCount = integrationsData.ai.length;
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
  // step is on screen right now (see corpGateRef's declaration for why a
  // stepId-scoped version would be wrong).
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
    providerCount,
  );
  const done = stepDone(
    status,
    readiness,
    workspaces,
    integrationsCount,
    corpNetwork,
    corpRedirects,
    providerCount,
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
        // #213 — the counter/footer walk only the four steps that BLOCK a
        // run; PhaseRail below keeps the old full walkOrder, since its three
        // groups (Required/Optional setup/Demos) still need to know which
        // conditional demos survived stepOrder(status).
        order={REQUIRED_STEPS}
        requiredSummary={optionalStepCounts(status)}
        refuseNext={refuseSelect}
        rail={
          <PhaseRail
            current={stepId}
            badges={badges}
            done={done}
            onSelect={selectStep}
            order={walkOrder}
            refuseNext={refuseSelect}
          />
        }
        checking={rechecking}
        lastCheckedLabel={lastCheckedLabel(lastCheckedAt)}
        onRecheck={forceRecheck}
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
          <DeploymentStep status={status} access={access} accessState={accessState} onReloadAccess={reloadAccessAndShell} />
        )}
        {stepId === "corp_network" && (
          <CorpNetworkStep
            status={status}
            siteConfig={siteConfig}
            reloadSiteConfig={reloadSiteConfig}
            saveSiteConfig={saveSiteConfig}
            gate={corpGate}
            onGateChange={onCorpGateChange}
            onRecheck={forceRecheck}
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
        {/* The step's BODY is the card (zero teal — the footer Next is the
            step's one affirmative; the forms and Save providers live on
            /providers). setup/providers-card.tsx is also the Settings card,
            replacing Git host. */}
        {stepId === "providers" && <ProvidersCard harnesses={status?.harnesses} />}
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
            onRecheck={forceRecheck}
            rechecking={rechecking}
            lastCheckedAt={lastCheckedAt}
            onJump={selectStep}
          />
        )}
      </SetupLayout>

      {/* Launching is the top bar's "New run" on every screen, so this funnel
          carries no launch dialog of its own — `finish` (Review's "Finish
          setup") is what retires the funnel. */}
    </>
  );
}
