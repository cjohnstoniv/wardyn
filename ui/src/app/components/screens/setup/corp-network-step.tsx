/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Corporate network — Getting Started step 2 of 12, BEFORE Integrations (see
// steps.ts for why the order itself is the fix). Two sub-tabs: Host proxy
// (evidence read from this host, over the config sandboxes actually use, over
// a real Test probe) and Egress redirection (the SiteConfig.egress_redirects
// list — package-registry mirrors and, more generally, any outbound
// URL/host/IP redirect). Mirrors the mock's corpStep/proxyPanel/egressPanel
// (mockup2/wardyn-integrations.js) with two deliberate departures from it,
// both called out where they happen:
//   1. The masked "cred" display is the SAME editable Proxy URL field with its
//      `type` flipped to "password" once it carries a username/password,
//      instead of a second read-only field — the native input-masking
//      primitive, not a hand-rolled partial-mask string.
//   2. A row's own Test button disables + relabels while THAT row is testing
//      (the mock left it always-enabled/"Test" — inconsistent with the proxy
//      Test button one panel over, which already disables while running).
//
// GATED, unlike the mock: Next won't advance to Integrations without proof of
// internet access (a passing test-proxy probe), a look at Egress redirection
// (empty is a fine answer, but it has to be an answered question), and every
// configured redirect testing reached — see steps.ts's corpNetworkGate,
// the single source of truth setup-layout.tsx's Next button, the rail badge,
// and this step's own done-ness all read. no_runner is the one honest bypass:
// Wardyn is structurally unable to probe on this host, so holding the gate
// open would trap the operator with no way to ever satisfy it. The gate's
// PROOF (proxyProbe/egressVisited/redirectProbes) is lifted to the orchestrator
// (setup-screen.tsx, via the gate/onGateChange props below) because this
// component unmounts on navigation and the proof must survive leaving and
// re-entering the step.
import * as React from "react";
import { toast } from "sonner";
import type { SetupStatus, SiteConfig } from "../../../lib/types";
import { health as healthApi } from "../../../lib/api/health";
import { HttpError } from "../../../lib/api/core";
import { getErrorMessage } from "../../../lib/format";
import { T } from "../../../lib/integrations";
import { Tabs, TabsList, TabsTrigger } from "../../ui/tabs";
import { useOperator } from "../../wardyn/operator-context";
import { useSiteConfigStep } from "./step-bodies";
import { EgressTab, useElapsedTimer, type ProbeUiState } from "./corp-network-egress";

// compactEndpoint moved with the egress family; re-exported so its tests and
// any caller keep one import path for this step's helpers.
export { compactEndpoint } from "./corp-network-egress";
import type { CorpNetworkGate, CorpNetworkState } from "./steps";
// HostProxyTab (+ EvidenceBlock/ProxyTestBlock and the evidence-reading pure
// helpers) moved to corp-network-proxy.tsx — the same size-gate split
// corp-network-egress.tsx already got for the Egress tab. Re-exported so every
// existing importer (setup-screen.tsx, this step's own tests) keeps working
// unchanged.
import { HostProxyTab } from "./corp-network-proxy";
export { hasUserinfo, isProxyConfigured, proxyDetected } from "./corp-network-proxy";

// ------------------------------------------------------------
// Top-level step
// ------------------------------------------------------------

// The step's imperative fix-it handlers, registered up to the orchestrator
// (setup-screen.tsx) so the footer's gate action — rendered by SetupLayout,
// far outside this tree — can reach in: run the probe, open the egress tab,
// fire the redirect tests. The gate row is the ONE place a probe is launched
// from while the gate is locked (the mock's corpFoot `action`).
export interface CorpStepActions {
  probe: () => void;
  probeCustom: () => void;
  openEgress: () => void;
  testRedirects: () => void;
}

export function CorpNetworkStep({
  status,
  siteConfig,
  reloadSiteConfig,
  saveSiteConfig,
  gate,
  onGateChange,
  onRecheck = reloadSiteConfig,
  gateResult,
  registerActions,
  tab: tabProp,
  onTabChange,
  secretNames,
}: {
  status: SetupStatus;
  siteConfig: SiteConfig | null;
  reloadSiteConfig: () => Promise<void>;
  saveSiteConfig: (next: SiteConfig) => Promise<void>;
  /** The proof-of-connectivity facts that gate Next — held by the orchestrator
   *  (setup-screen.tsx), not here, because this component unmounts on
   *  navigation and the proof must survive leaving and re-entering the step. */
  gate: Pick<CorpNetworkState, "proxyProbe" | "probeRunning" | "customDraft" | "redirectProbes">;
  onGateChange: (
    patch: Partial<Pick<CorpNetworkState, "proxyProbe" | "probeRunning" | "customDraft" | "redirectProbes">>,
  ) => void;
  /** The evidence block's own "Re-check" button (host-proxy detection only) —
   *  the orchestrator's FULL recheck (status + site-config), the SAME one the
   *  persistent host-status strip already calls, so both buttons named
   *  "Re-check" actually refresh the same host_proxy detection. Falls back to
   *  reloadSiteConfig (site-config only — detection never changes) for
   *  standalone/test renders that don't wire the orchestrator up. */
  onRecheck?: () => void;
  /** The orchestrator's computed gate (the SAME corpNetworkGate call that
   *  drives the footer) — this step only READS on/action from it, to suppress
   *  the panel's own Test button while the gate row carries the action. One
   *  derivation, no recompute drift. Absent in standalone renders (tests),
   *  where the panel keeps its button. */
  gateResult?: CorpNetworkGate;
  registerActions?: (a: CorpStepActions | null) => void;
  /** Controlled sub-tab (the orchestrator owns it so the FOOTER can walk the
   *  tabs — Next on Host proxy goes to Egress redirection, not the exit; see
   *  setup-screen.tsx). Standalone renders (tests) omit both and the step
   *  falls back to its own state. */
  tab?: "proxy" | "egress";
  onTabChange?: (t: "proxy" | "egress") => void;
  /** W12-W12-C-3: the store's live secret names — see HostProxyTab's
   *  secretRefDangling and the AddSecretDialog existingNames wiring below.
   *  Optional and UNKNOWN (not "empty") when omitted — a standalone/test
   *  render that doesn't wire the orchestrator's list up must never read a
   *  real secret ref as dangling just because nothing was passed. */
  secretNames?: string[];
}) {
  const operator = useOperator();
  const [innerTab, setInnerTab] = React.useState<"proxy" | "egress">("proxy");
  const tab = tabProp ?? innerTab;
  const setTab = onTabChange ?? setInnerTab;
  const { saving, mutate } = useSiteConfigStep(reloadSiteConfig, saveSiteConfig);

  // ---- The probe, owned HERE (not in HostProxyTab): the gate row can fire it
  // from either tab, and its UI state must survive tab switches. ----
  const [test, setTest] = React.useState<ProbeUiState>(() =>
    gate.probeRunning
      ? { kind: "running", elapsedSec: 0 }
      : gate.proxyProbe
        ? { kind: "done", result: gate.proxyProbe }
        : { kind: "idle" },
  );
  const [customReject, setCustomReject] = React.useState<string | null>(null);
  const runTest = async (customUrl?: string) => {
    const before = test;
    setCustomReject(null);
    setTest({ kind: "running", elapsedSec: 0, custom: !!customUrl });
    onGateChange({ probeRunning: true });
    try {
      const result = await healthApi.testProxy(customUrl);
      setTest({ kind: "done", result });
      onGateChange({ proxyProbe: result, probeRunning: false });
    } catch (e) {
      // A request that never produced a probe result is NOT a probe verdict. Rendering
      // it as "blocked" would blame the corporate network for a 403, a restarted
      // wardynd, or a bad payload — sending the operator to debug a firewall that is
      // working fine.
      if (customUrl && e instanceof HttpError && e.status === 400) {
        // A rejected custom URL renders INLINE in the block that asked for it,
        // the server's own message verbatim (T.CUSTOM_REJECT_WHY), and the
        // failed result that revealed the block stays on screen — a toast
        // would vanish with the reason while the operator is mid-recovery.
        setCustomReject(`Rejected: “${customUrl}” — ${getErrorMessage(e)}. Nothing was launched.`);
        setTest(before);
        onGateChange({ probeRunning: false });
        return;
      }
      // Anything else: report the request failure as itself and stay untested.
      toast.error("Could not run the proxy test", { description: getErrorMessage(e) });
      setTest({ kind: "idle" });
      onGateChange({ probeRunning: false });
    }
  };
  const elapsed = useElapsedTimer(test.kind === "running");
  const liveTest: ProbeUiState = test.kind === "running" ? { ...test, elapsedSec: elapsed } : test;

  // The `test` state seeds from `gate` once at mount. If the operator leaves
  // mid-probe and returns before it resolves, the in-flight runTest resolves in
  // the prior (unmounted) instance — its setTest is a no-op, but its onGateChange
  // still lands the verdict on the surviving orchestrator gate. Without this,
  // the remounted instance stays stuck on the seeded "Testing…" forever. Adopt
  // the gate's outcome whenever it reports the probe finished while we still show
  // running. (During our OWN probe gate.probeRunning is true, so this never
  // clobbers a live test.)
  React.useEffect(() => {
    if (test.kind === "running" && !gate.probeRunning) {
      setTest(gate.proxyProbe ? { kind: "done", result: gate.proxyProbe } : { kind: "idle" });
    }
  }, [gate.probeRunning, gate.proxyProbe, test.kind]);

  // What the builtin probe is about to traverse, named up front (the mock's
  // probeLine) — endpoints mirror internal/api/site_config_probe.go's targets.
  const upstreamLabel =
    siteConfig?.upstream_proxy_url ||
    (siteConfig?.upstream_proxy_secret_ref ? `the upstream in secret ${siteConfig.upstream_proxy_secret_ref}` : "");
  const probeLine = `The probe will try www.msftconnecttest.com and detectportal.firefox.com ${
    upstreamLabel ? `through wardyn-proxy → ${upstreamLabel}` : "directly"
  }, match their published payloads, and report what actually happened.`;

  // Fired by the footer's "Test all redirects" action: switching to the egress
  // tab mounts EgressTab, and the bumped signal fires its testAll once up.
  const [testAllSignal, setTestAllSignal] = React.useState(0);

  // Latest known redirectProbes, updated SYNCHRONOUSLY inside onProbeResult
  // below as each result lands — never re-read from the `gate` prop after
  // this initial seed (re-entering the step). "Test all" fires every
  // redirect's probe concurrently; onProbeResult used to materialize its
  // patch from gate.redirectProbes at write time, a snapshot that lags a
  // whole batch still resolving, so the LAST result to land clobbered every
  // other one already accumulated (onGateChange's outer merge is a shallow
  // spread — a patch's redirectProbes key REPLACES the map wholesale, it
  // doesn't deep-merge). This ref sidesteps that: each call composes onto
  // whatever the ref most recently held, independent of render timing.
  const redirectProbesRef = React.useRef(gate.redirectProbes);

  // Registered once; the handlers read the LATEST runTest/draft through refs,
  // so a keystroke in the custom field never re-registers anything.
  const runTestRef = React.useRef(runTest);
  runTestRef.current = runTest;
  const draftRef = React.useRef(gate.customDraft);
  draftRef.current = gate.customDraft;
  React.useEffect(() => {
    if (!registerActions) return;
    registerActions({
      probe: () => {
        setTab("proxy");
        runTestRef.current();
      },
      probeCustom: () => {
        setTab("proxy");
        const draft = draftRef.current.trim();
        if (draft) runTestRef.current(draft);
      },
      openEgress: () => setTab("egress"),
      testRedirects: () => {
        setTab("egress");
        setTestAllSignal((n) => n + 1);
      },
    });
    return () => registerActions(null);
  }, [registerActions]);

  // One Test button per screen (the mock's stepTest): while the gate row
  // below is offering the action, the panel's own button is suppressed.
  const hideProbeButton = !!gateResult && !gateResult.on && !!gateResult.action;

  // The dot means only "configured rows not yet proven" — never "you haven't
  // looked" (no rung demands a visit any more; see steps.ts).
  const redirectRows = siteConfig?.egress_redirects ?? [];
  const egressDot = redirectRows.some((r) => gate.redirectProbes[r.from]?.state !== "reached");

  // W13-S1-1: the server's graded host_proxy check — the SAME row the Review
  // step lists, surfaced here too (see HostProxyCheckNote) since Review sits
  // behind this step's own mandatory gate.
  const hostProxyCheck = status.checks.find((c) => c.id === "host_proxy");

  return (
    <div className="space-y-5">
      <p className="text-sm leading-relaxed text-muted-foreground">{T.CORP_LEDE}</p>

      <Tabs value={tab} onValueChange={(v) => setTab(v as typeof tab)}>
        <TabsList>
          <TabsTrigger value="proxy">Host proxy</TabsTrigger>
          <TabsTrigger value="egress">
            Egress redirection
            {egressDot && <span aria-hidden className="ml-1.5 inline-block size-1.5 rounded-full bg-warning" />}
          </TabsTrigger>
        </TabsList>
      </Tabs>

      {tab === "proxy" ? (
        <HostProxyTab
          siteConfig={siteConfig}
          detection={status.host_proxy}
          hostProxyCheck={hostProxyCheck}
          secretNames={secretNames}
          mutate={mutate}
          saving={saving}
          operator={operator}
          onRecheck={onRecheck}
          probe={{
            state: liveTest,
            onTest: () => runTest(),
            onTestCustom: () => {
              const draft = gate.customDraft.trim();
              if (draft) runTest(draft);
            },
            probeLine,
            customReject,
            customDraft: gate.customDraft,
            onCustomDraftChange: (v) => onGateChange({ customDraft: v }),
            hideButton: hideProbeButton,
          }}
        />
      ) : (
        <EgressTab
          siteConfig={siteConfig}
          mutate={mutate}
          operator={operator}
          initialProbes={gate.redirectProbes}
          onProbeResult={(from, result) => {
            const next = { ...redirectProbesRef.current, [from]: result };
            redirectProbesRef.current = next;
            onGateChange({ redirectProbes: next });
          }}
          testAllSignal={testAllSignal}
          onTestAllConsumed={() => setTestAllSignal(0)}
        />
      )}
    </div>
  );
}
