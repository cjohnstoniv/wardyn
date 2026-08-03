/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Corporate network — Getting Started step 2 of 10, BEFORE Integrations (see
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
// configured redirect testing reached — see steps.ts's corpNetworkBlockReason,
// the single source of truth setup-layout.tsx's Next button, the rail badge,
// and this step's own done-ness all read. no_runner is the one honest bypass:
// Wardyn is structurally unable to probe on this host, so holding the gate
// open would trap the operator with no way to ever satisfy it. The gate's
// PROOF (proxyProbe/egressVisited/redirectProbes) is lifted to the orchestrator
// (setup-screen.tsx, via the gate/onGateChange props below) because this
// component unmounts on navigation and the proof must survive leaving and
// re-entering the step.
import * as React from "react";
import { ChevronDown, Loader2 } from "lucide-react";
import { toast } from "sonner";
import type { HostProxyDetection, HostProxySetting, SetupStatus, SiteConfig } from "../../../lib/types";
import { health as healthApi, type ProxyTestResult } from "../../../lib/api/health";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { getErrorMessage } from "../../../lib/format";
import { T } from "../../../lib/integrations";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Tabs, TabsList, TabsTrigger } from "../../ui/tabs";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { cn } from "../../ui/utils";
import { Field } from "../new-run/step-shell";
import { Mono } from "../../wardyn/code-block";
import { Chip } from "../../wardyn/primitives";
import { useOperator } from "../../wardyn/operator-context";
import { AddSecretDialog } from "../secrets";
import { useSiteConfigStep } from "./step-bodies";
import { EgressTab, TestVerdictChip, useElapsedTimer, type ProbeUiState } from "./corp-network-egress";

// compactEndpoint moved with the egress family; re-exported so its tests and
// any caller keep one import path for this step's helpers.
export { compactEndpoint } from "./corp-network-egress";
import type { CorpNetworkState } from "./steps";

// ------------------------------------------------------------
// Pure helpers — exported so setup-screen.tsx's badge/done derivation reads
// the SAME facts this step renders from (single source of truth, no drift).
// ------------------------------------------------------------
export function isProxyConfigured(cfg: SiteConfig | null): boolean {
  return !!(cfg?.upstream_proxy_url || cfg?.upstream_proxy_secret_ref);
}

export interface EvidenceRow {
  key: string;
  value: string;
  source: string;
  hasCredentials?: boolean;
  /** The value "Use this" would populate — absent (NO_PROXY) means no button. */
  useValue?: string;
  note?: string;
}

// All rows worth showing, in the mock's own order. NO_PROXY is evidence too
// (T.NOPROXY_NOTE explains why it never gets a "Use this") — everything else
// is a real candidate proxy value.
export function evidenceRows(d?: HostProxyDetection): EvidenceRow[] {
  if (!d) return [];
  const rows: EvidenceRow[] = [];
  const push = (key: string, s?: HostProxySetting) => {
    if (s) rows.push({ key, value: s.value, source: s.source, hasCredentials: s.has_credentials, useValue: s.value });
  };
  push("HTTP_PROXY", d.http_proxy);
  push("HTTPS_PROXY", d.https_proxy);
  push("ALL_PROXY", d.all_proxy);
  if (d.no_proxy) rows.push({ key: "NO_PROXY", value: d.no_proxy.value, source: d.no_proxy.source, note: T.NOPROXY_NOTE });
  push("git http.proxy", d.git_proxy?.http_proxy);
  push("git https.proxy", d.git_proxy?.https_proxy);
  return rows;
}

export function proxyCandidateValues(d?: HostProxyDetection): string[] {
  const values = evidenceRows(d)
    .map((r) => r.useValue)
    .filter((v): v is string => !!v);
  return [...new Set(values)];
}

export function proxyDetected(d?: HostProxyDetection): boolean {
  return proxyCandidateValues(d).length > 0;
}

// A URL "has credentials" (from the FE's own perspective — HostProxySetting's
// detected value is already server-masked, this is about what the operator is
// TYPING right now) when it parses with a non-empty username.
export function hasUserinfo(url: string): boolean {
  try {
    return !!new URL(url).username;
  } catch {
    return false;
  }
}


// ------------------------------------------------------------
// Small shared bits
// ------------------------------------------------------------
function BlockLabel({ children }: { children: React.ReactNode }) {
  return <div className="text-xs font-medium uppercase tracking-wide text-muted-foreground">{children}</div>;
}

function ConfigStatusLine({ tone, children }: { tone: "success" | "neutral"; children: React.ReactNode }) {
  return (
    <div className="flex items-center gap-2">
      <span className={cn("size-1.5 shrink-0 rounded-full", tone === "success" ? "bg-success" : "bg-border-strong")} />
      <span className={cn("text-xs", tone === "success" ? "text-success" : "text-muted-foreground")}>{children}</span>
    </div>
  );
}


function EvidenceBlock({
  rows,
  showUse,
  onUse,
  onRecheck,
  operator,
}: {
  rows: EvidenceRow[];
  showUse: boolean;
  onUse: (value: string) => void;
  onRecheck: () => void;
  operator: boolean;
}) {
  return (
    <div className="space-y-3 rounded-xl border border-border bg-card p-3.5">
      <div className="flex items-center gap-2">
        <BlockLabel>{T.EVIDENCE_HEAD}</BlockLabel>
        <span className="flex-1" />
        <Button size="sm" variant="outline" onClick={onRecheck}>
          Re-check
        </Button>
      </div>
      <p className="max-w-[560px] text-[0.6875rem] leading-snug text-muted-foreground">{T.EVIDENCE_EXPLAIN}</p>
      {rows.length === 0 ? (
        <p className="text-[0.6875rem] leading-snug text-muted-foreground">{T.EVIDENCE_NONE}</p>
      ) : (
        <div className="rounded-lg border border-border">
          {rows.map((r, i) => (
            <div
              key={r.key}
              className={cn("flex items-start gap-2.5 p-2.5", i > 0 && "border-t border-border")}
            >
              <Mono className="w-32 shrink-0 pt-0.5 text-[0.6875rem] text-muted-foreground">{r.key}</Mono>
              <div className="min-w-0 flex-1 space-y-0.5">
                <Mono className="block truncate text-xs text-foreground" title={r.value}>
                  {r.value}
                </Mono>
                {r.note && <p className="text-[0.6875rem] leading-snug text-muted-foreground">{r.note}</p>}
              </div>
              {r.hasCredentials && (
                <Chip tone="warning" className="shrink-0 text-[0.625rem]" title="A credential was detected in this value (masked above).">
                  creds
                </Chip>
              )}
              <Chip tone="neutral" mono className="shrink-0 text-[0.625rem]">
                {r.source}
              </Chip>
              {showUse && r.useValue && (
                <Button
                  size="sm"
                  variant="outline"
                  className="shrink-0"
                  disabled={!operator}
                  title={!operator ? T.VIEWER_HINT : undefined}
                  onClick={() => onUse(r.useValue!)}
                >
                  Use this
                </Button>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function ProxyTestBlock({
  state,
  onTest,
  operator,
}: {
  state: ProbeUiState;
  /** No arg runs the default multi-target check; a url retries against it instead (see T.TEST_CUSTOM_HINT). */
  onTest: (url?: string) => void;
  operator: boolean;
}) {
  const running = state.kind === "running";
  const [customUrl, setCustomUrl] = React.useState("");
  return (
    <div className="flex items-start gap-3 rounded-lg border border-border p-3">
      <Button size="sm" variant="outline" className="shrink-0" disabled={running || !operator} title={!operator ? T.VIEWER_HINT : undefined} onClick={() => onTest()}>
        {running ? <Loader2 className="size-3.5 animate-spin" /> : null}
        {running ? "Testing…" : "Test proxy"}
      </Button>
      <div className="min-w-0 flex-1 space-y-1">
        {state.kind === "idle" && (
          <>
            <span className="text-[0.6875rem] text-muted-foreground">Not tested</span>
            <p className="text-[0.6875rem] leading-snug text-muted-foreground">{T.TEST_PROXY_HINT}</p>
          </>
        )}
        {state.kind === "running" && (
          <p className="text-[0.75rem] text-info">Starting a throwaway sandbox — {state.elapsedSec}s</p>
        )}
        {state.kind === "done" && (
          <>
            <TestVerdictChip state={state.result.state} />
            <p className="text-[0.75rem] leading-snug text-foreground">{state.result.detail}</p>
            {state.result.state !== "no_runner" && (
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">{T.TEST_STANDING}</p>
            )}
            {/* Only surfaced after a real failure — the deliberate escape for a
                host with no public internet. Showing it unconditionally would
                make it the path of least resistance instead of a fallback. */}
            {state.result.state === "blocked" && (
              <div className="mt-1.5 space-y-1.5 rounded-md border border-border bg-muted/30 p-2">
                <p className="text-[0.6875rem] leading-snug text-muted-foreground">{T.TEST_CUSTOM_HINT}</p>
                <div className="flex items-center gap-1.5">
                  <Input
                    value={customUrl}
                    onChange={(e) => setCustomUrl(e.target.value)}
                    placeholder="https://an-internal-host-you-can-reach.example.com"
                    className="h-7 flex-1 font-mono text-xs"
                  />
                  <Button
                    size="sm"
                    variant="outline"
                    className="shrink-0"
                    disabled={!operator || !customUrl.trim()}
                    onClick={() => onTest(customUrl.trim())}
                  >
                    Test this URL instead
                  </Button>
                </div>
              </div>
            )}
          </>
        )}
      </div>
    </div>
  );
}

function HostProxyTab({
  siteConfig,
  detection,
  mutate,
  saving,
  operator,
  onRecheck,
  initialProbe,
  onProbeResult,
}: {
  siteConfig: SiteConfig | null;
  detection: HostProxyDetection | undefined;
  mutate: (next: SiteConfig, errorMessage: string) => Promise<boolean>;
  saving: boolean;
  operator: boolean;
  onRecheck: () => void;
  /** The last real result the orchestrator remembers from a PRIOR visit to this step (see setup-screen.tsx's corpGate) — seeds the panel back to what it last observed instead of a false "Not tested" the operator would have to redo. */
  initialProbe?: ProxyTestResult;
  /** Reports every terminal probe result upward so it survives leaving/re-entering the step and can gate Next (see steps.ts's corpNetworkBlockReason). Never called for a failed REQUEST (no verdict to report) — see runTest's catch below. */
  onProbeResult: (result: ProxyTestResult) => void;
}) {
  const [url, setUrl] = React.useState("");
  const [useSecret, setUseSecret] = React.useState(false);
  const [disclosureOpen, setDisclosureOpen] = React.useState(false);
  const [secretName, setSecretName] = React.useState("upstream-proxy-url");
  const [addSecretOpen, setAddSecretOpen] = React.useState(false);
  const [selected, setSelected] = React.useState(0);
  const [test, setTest] = React.useState<ProbeUiState>(() => (initialProbe ? { kind: "done", result: initialProbe } : { kind: "idle" }));

  // Seed from the freshest doc exactly once — a later reload (Re-check) must
  // never stomp an in-progress edit (matches HostProxyStep's own seededRef).
  const seededRef = React.useRef(false);
  React.useEffect(() => {
    if (seededRef.current || !siteConfig) return;
    seededRef.current = true;
    if (siteConfig.upstream_proxy_url) {
      setUrl(siteConfig.upstream_proxy_url);
    } else if (siteConfig.upstream_proxy_secret_ref) {
      setUseSecret(true);
      setDisclosureOpen(true);
      setSecretName(siteConfig.upstream_proxy_secret_ref);
    }
  }, [siteConfig]);

  const rows = evidenceRows(detection);
  const candidates = proxyCandidateValues(detection);
  const configured = isProxyConfigured(siteConfig);
  const showUse = !useSecret && !configured;

  const saveUrl = async (value: string) => {
    if (await mutate({ ...(siteConfig ?? {}), upstream_proxy_url: value || undefined, upstream_proxy_secret_ref: undefined }, "Failed to save the upstream proxy")) {
      setUrl(value);
      setUseSecret(false);
      toast.success("Upstream proxy saved");
    }
  };

  const saveAsSecret = async (rawUrl: string, name: string) => {
    try {
      await secretsApi.setSecret(name, rawUrl);
    } catch (e) {
      toast.error("Failed to store the proxy credential", { description: getErrorMessage(e) });
      return;
    }
    if (
      await mutate(
        { ...(siteConfig ?? {}), upstream_proxy_secret_ref: name, upstream_proxy_url: undefined },
        "Stored the secret, but failed to reference it",
      )
    ) {
      setUseSecret(true);
      setDisclosureOpen(true);
      toast.success("Upstream proxy saved as a secret");
    }
  };

  const saveSecretRef = async (name: string) => {
    const trimmed = name.trim();
    if (!trimmed) return;
    if (await mutate({ ...(siteConfig ?? {}), upstream_proxy_secret_ref: trimmed, upstream_proxy_url: undefined }, "Failed to save the proxy secret reference")) {
      toast.success("Upstream proxy saved");
    }
  };

  const runTest = async (customUrl?: string) => {
    setTest({ kind: "running", elapsedSec: 0 });
    try {
      const result = await healthApi.testProxy(customUrl);
      setTest({ kind: "done", result });
      onProbeResult(result);
    } catch (e) {
      // A request that never produced a probe result is NOT a probe verdict. Rendering
      // it as "blocked" would blame the corporate network for a 403, a restarted
      // wardynd, or a bad payload — sending the operator to debug a firewall that is
      // working fine. Report the request failure as itself and stay untested. A
      // rejected custom URL (400) lands here too — the server's specific reason is
      // the toast description, never papered over.
      toast.error("Could not run the proxy test", { description: getErrorMessage(e) });
      setTest({ kind: "idle" });
    }
  };
  const elapsed = useElapsedTimer(test.kind === "running");
  const liveTest: ProbeUiState = test.kind === "running" ? { kind: "running", elapsedSec: elapsed } : test;

  const draftHasCreds = !useSecret && hasUserinfo(url);

  return (
    <div className="space-y-4">
      <EvidenceBlock
        rows={rows}
        showUse={showUse}
        onUse={(value) => (hasUserinfo(value) ? setUrl(value) : saveUrl(value))}
        onRecheck={onRecheck}
        operator={operator}
      />

      <div className="space-y-3 rounded-xl border border-border bg-card p-3.5">
        <BlockLabel>{T.CONFIG_HEAD}</BlockLabel>
        {useSecret ? (
          <ConfigStatusLine tone={siteConfig?.upstream_proxy_secret_ref ? "success" : "neutral"}>
            {siteConfig?.upstream_proxy_secret_ref ? (
              <>
                Chaining through the URL in secret <Mono className="text-xs">{siteConfig.upstream_proxy_secret_ref}</Mono> — write-only, so
                the URL can&apos;t be shown here
              </>
            ) : (
              T.NOT_CONFIGURED
            )}
          </ConfigStatusLine>
        ) : (
          <ConfigStatusLine tone={configured ? "success" : "neutral"}>
            {configured ? <>Chaining through <Mono className="text-xs">{siteConfig?.upstream_proxy_url}</Mono></> : T.NOT_CONFIGURED}
          </ConfigStatusLine>
        )}

        {!useSecret && !configured && candidates.length === 1 && !url && (
          <div className="flex items-center gap-3 rounded-lg border border-primary/40 bg-primary/10 p-2.5">
            <Button size="sm" className="shrink-0" disabled={!operator} onClick={() => saveUrl(candidates[0])}>
              Use detected proxy
            </Button>
            <p className="min-w-0 flex-1 text-[0.75rem] text-foreground">
              Configures <Mono className="text-[0.75rem]">{candidates[0]}</Mono> — the value Wardyn already read. No retyping.
            </p>
          </div>
        )}

        {!useSecret && !configured && candidates.length > 1 && !url && (
          <div className="space-y-2.5">
            <p className="text-[0.75rem] text-foreground">Detected values differ — pick which one sandboxes chain through:</p>
            <RadioGroup value={String(selected)} onValueChange={(v) => setSelected(Number(v))} className="space-y-2">
              {candidates.map((c, i) => (
                <div
                  key={c}
                  className={cn(
                    "flex cursor-pointer items-center gap-2.5 rounded-lg border p-2",
                    selected === i ? "border-primary bg-primary/10" : "border-border hover:border-border-strong",
                  )}
                  onClick={() => setSelected(i)}
                >
                  <RadioGroupItem value={String(i)} id={`px-cand-${i}`} />
                  <Mono className="min-w-0 flex-1 truncate text-xs text-foreground">{c}</Mono>
                </div>
              ))}
            </RadioGroup>
            <Button size="sm" disabled={!operator} onClick={() => saveUrl(candidates[selected])}>
              Use selected
            </Button>
          </div>
        )}

        {!useSecret && (
          <>
            <Field
              label={candidates.length > 0 && !configured ? "Or enter the proxy URL yourself" : "Proxy URL"}
              htmlFor="corp-proxy-url"
              hint={T.PROXY_URL_HINT}
            >
              <Input
                id="corp-proxy-url"
                type={draftHasCreds ? "password" : "text"}
                value={url}
                onChange={(e) => setUrl(e.target.value)}
                placeholder="http://proxy.corp.example.com:8080"
                className="font-mono"
              />
            </Field>
            {draftHasCreds ? (
              <>
                <div className="rounded-lg border border-warning/30 bg-warning-subtle p-2">
                  <p className="text-[0.6875rem] leading-snug text-warning">{T.CRED_URL_NOTE}</p>
                </div>
                <div className="flex items-center gap-2">
                  <Chip tone="neutral" mono className="text-[0.625rem]">
                    {secretName}
                  </Chip>
                  <p className="text-[0.6875rem] text-muted-foreground">{T.WRITE_ONLY}</p>
                </div>
                <Button size="sm" disabled={!operator || saving} onClick={() => saveAsSecret(url, secretName)}>
                  {saving ? <Loader2 className="size-3.5 animate-spin" /> : "Save"}
                </Button>
              </>
            ) : (
              <Button size="sm" disabled={!operator || saving} onClick={() => saveUrl(url)}>
                {saving ? <Loader2 className="size-3.5 animate-spin" /> : "Save"}
              </Button>
            )}
          </>
        )}

        {useSecret ? (
          <div className="space-y-2.5">
            <button
              type="button"
              className="inline-flex items-center gap-1 text-[0.75rem] text-muted-foreground hover:text-foreground"
              onClick={() => setDisclosureOpen((o) => !o)}
            >
              <ChevronDown className={cn("size-3.5 transition-transform", !disclosureOpen && "-rotate-90")} />
              Use a stored secret instead
            </button>
            {disclosureOpen && (
              <>
                <div className="flex items-end gap-2">
                  <Field label="Secret name" htmlFor="corp-proxy-secret" className="flex-1">
                    <Input
                      id="corp-proxy-secret"
                      value={secretName}
                      onChange={(e) => setSecretName(e.target.value)}
                      placeholder="upstream-proxy-url"
                      className="font-mono"
                    />
                  </Field>
                  <Button variant="outline" disabled={!operator} onClick={() => setAddSecretOpen(true)}>
                    Add secret…
                  </Button>
                </div>
                <p className="text-[0.6875rem] leading-snug text-muted-foreground">{T.SECRET_INSTEAD_HINT}</p>
                <div className="flex items-center gap-2">
                  <Button size="sm" disabled={!operator || saving || !secretName.trim()} onClick={() => saveSecretRef(secretName)}>
                    Save
                  </Button>
                  <button
                    type="button"
                    className="text-[0.75rem] text-muted-foreground hover:text-foreground"
                    onClick={() => {
                      setUseSecret(false);
                      setDisclosureOpen(false);
                    }}
                  >
                    Enter a URL instead
                  </button>
                </div>
              </>
            )}
          </div>
        ) : (
          <button
            type="button"
            className="inline-flex items-center gap-1 text-[0.75rem] text-muted-foreground hover:text-foreground"
            onClick={() => {
              setUseSecret(true);
              setDisclosureOpen(true);
            }}
          >
            <ChevronDown className="size-3.5 -rotate-90" />
            Use a stored secret instead
          </button>
        )}
      </div>

      <ProxyTestBlock state={liveTest} onTest={runTest} operator={operator} />

      <AddSecretDialog
        open={addSecretOpen}
        onOpenChange={setAddSecretOpen}
        lockName
        initialName={secretName}
        onSaved={(name) => {
          setAddSecretOpen(false);
          saveSecretRef(name);
        }}
      />
    </div>
  );
}

// ------------------------------------------------------------
// Top-level step
// ------------------------------------------------------------
export function CorpNetworkStep({
  status,
  siteConfig,
  reloadSiteConfig,
  saveSiteConfig,
  gate,
  onGateChange,
}: {
  status: SetupStatus;
  siteConfig: SiteConfig | null;
  reloadSiteConfig: () => Promise<void>;
  saveSiteConfig: (next: SiteConfig) => Promise<void>;
  /** The proof-of-connectivity facts that gate Next — held by the orchestrator
   *  (setup-screen.tsx), not here, because this component unmounts on
   *  navigation and the proof must survive leaving and re-entering the step. */
  gate: Pick<CorpNetworkState, "proxyProbe" | "egressVisited" | "redirectProbes">;
  onGateChange: (patch: Partial<Pick<CorpNetworkState, "proxyProbe" | "egressVisited" | "redirectProbes">>) => void;
}) {
  const operator = useOperator();
  const [tab, setTab] = React.useState<"proxy" | "egress">("proxy");
  const { saving, mutate } = useSiteConfigStep(reloadSiteConfig, saveSiteConfig);

  return (
    <div className="space-y-5">
      <p className="text-sm leading-relaxed text-muted-foreground">{T.CORP_LEDE}</p>

      <Tabs
        value={tab}
        onValueChange={(v) => {
          const next = v as typeof tab;
          setTab(next);
          // The explicit "I looked" acknowledgement corpNetworkBlockReason
          // requires at zero redirects — an empty list needs a deliberate
          // look, same as a configured one (see steps.ts).
          if (next === "egress" && !gate.egressVisited) onGateChange({ egressVisited: true });
        }}
      >
        <TabsList>
          <TabsTrigger value="proxy">Host proxy</TabsTrigger>
          <TabsTrigger value="egress">Egress redirection</TabsTrigger>
        </TabsList>
      </Tabs>

      {tab === "proxy" ? (
        <HostProxyTab
          siteConfig={siteConfig}
          detection={status.host_proxy}
          mutate={mutate}
          saving={saving}
          operator={operator}
          onRecheck={reloadSiteConfig}
          initialProbe={gate.proxyProbe}
          onProbeResult={(result) => onGateChange({ proxyProbe: result })}
        />
      ) : (
        <EgressTab
          siteConfig={siteConfig}
          mutate={mutate}
          operator={operator}
          initialProbes={gate.redirectProbes}
          onProbeResult={(from, result) => onGateChange({ redirectProbes: { ...gate.redirectProbes, [from]: result } })}
        />
      )}
    </div>
  );
}
