/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// corp-network-proxy.tsx — the Host proxy sub-tab of the Corporate network
// step, split from corp-network-step.tsx along the design's own seam (the two
// sub-tabs) when the file crossed the size gate — the same split
// corp-network-egress.tsx already got for Egress redirection. isProxyConfigured,
// proxyDetected and hasUserinfo are re-exported from corp-network-step.tsx (see
// its own import comment) so every existing importer (setup-screen.tsx, this
// step's own tests) keeps working unchanged.
import * as React from "react";
import { ChevronDown, Loader2 } from "lucide-react";
import { toast } from "sonner";
import type { HostProxyDetection, HostProxySetting, SetupCheck, SiteConfig } from "../../../lib/types";
import type { ProxyTestResult } from "../../../lib/api/health";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { getErrorMessage } from "../../../lib/format";
import { T } from "../../../lib/integrations";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { cn } from "../../ui/utils";
import { Field } from "../../wardyn/form-primitives";
import { Mono } from "../../wardyn/code-block";
import { Chip } from "../../wardyn/primitives";
import { AddSecretDialog } from "../secrets";
import type { ProbeUiState } from "./corp-network-egress";

// ------------------------------------------------------------
// Pure helpers — exported so setup-screen.tsx's badge/done derivation reads
// the SAME facts this step renders from (single source of truth, no drift).
// Re-exported from corp-network-step.tsx.
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
function evidenceRows(d?: HostProxyDetection): EvidenceRow[] {
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

function proxyCandidateValues(d?: HostProxyDetection): string[] {
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

function ConfigStatusLine({ tone, children }: { tone: "success" | "neutral" | "warning"; children: React.ReactNode }) {
  return (
    <div className="flex items-center gap-2">
      <span
        className={cn(
          "size-1.5 shrink-0 rounded-full",
          tone === "success" ? "bg-success" : tone === "warning" ? "bg-warning" : "bg-border-strong",
        )}
      />
      <span
        className={cn(
          "text-xs",
          tone === "success" ? "text-success" : tone === "warning" ? "text-warning" : "text-muted-foreground",
        )}
      >
        {children}
      </span>
    </div>
  );
}

// W13-S1-1: the server's own graded verdict on this host's proxy detection
// (internal/api/setup_checks.go's hostProxyCheck) — same Detail/Fix the
// Review step's checks list would show, INCLUDING the one line that actually
// explains an unreachable upstream (a loopback-bound detected proxy) and its
// fix (`wardyn setup proxy-relay`). Review sits AFTER this mandatory gate, so
// an operator stuck here on a bad proxy could never reach it — surface the
// same diagnosis right here instead of only downstream of the gate it causes.
function HostProxyCheckNote({ check }: { check: SetupCheck }) {
  const warn = check.status === "warn";
  return (
    <div
      className={cn(
        "space-y-1 rounded-md border px-2.5 py-2",
        warn ? "border-warning/30 bg-warning-subtle" : "border-border bg-muted/40",
      )}
    >
      <p className={cn("text-[0.75rem] leading-snug", warn ? "text-warning" : "text-foreground")}>{check.detail}</p>
      {check.fix && <p className="text-[0.6875rem] leading-snug text-muted-foreground">{check.fix}</p>}
    </div>
  );
}

function EvidenceBlock({
  rows,
  showUse,
  onUse,
  onRecheck,
  operator,
  hostProxyCheck,
}: {
  rows: EvidenceRow[];
  showUse: boolean;
  onUse: (value: string) => void;
  onRecheck: () => void;
  operator: boolean;
  /** The server's graded host_proxy check (see HostProxyCheckNote above) — absent on a fixture-compat older daemon. */
  hostProxyCheck?: SetupCheck;
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
        hostProxyCheck?.detail ? (
          <HostProxyCheckNote check={hostProxyCheck} />
        ) : (
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">{T.EVIDENCE_NONE}</p>
        )
      ) : (
        <>
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
          {/* Beside the rows, not instead of them: a loopback-bound row still
              lists as real evidence, but "Use this" on it produces a proxy a
              sandbox can never reach — the warn-graded check is the one place
              that says so and names the fix. */}
          {hostProxyCheck?.status === "warn" && hostProxyCheck.detail && <HostProxyCheckNote check={hostProxyCheck} />}
        </>
      )}
    </div>
  );
}

// The verdict, per the mock's proxyTestBlock kinds: a builtin reached says
// which path it proved; a custom pass DELIBERATELY avoids the success
// treatment ("Request completed", info tone — it must never read as a
// verified one); intercepted is a blocked flavor rendered apart, because it
// sends the operator to a different person than a refused connection does.
function ProxyVerdict({ result }: { result: ProxyTestResult }) {
  if (result.state === "reached") {
    if (result.custom) {
      return (
        <span className="flex flex-wrap items-center gap-2">
          <Chip tone="info">Request completed</Chip>
          <span className="text-[0.6875rem] text-muted-foreground">custom endpoint — not verified against a known payload</span>
        </span>
      );
    }
    return (
      <Chip tone="success" dot>
        {result.via === "direct" ? "Reached · direct" : "Reached · via proxy"}
      </Chip>
    );
  }
  if (result.state === "no_runner") return <Chip tone="neutral">Can&apos;t test here</Chip>;
  // Without this arm, not_run falls through to the plain warning chip below
  // and renders "Blocked" — exactly the misleading claim this state exists
  // to avoid (the probe never ran; nothing about the network was observed).
  if (result.state === "not_run") return <Chip tone="neutral">Never ran</Chip>;
  if (result.intercepted) {
    return (
      <span className="flex flex-wrap items-center gap-2">
        <Chip tone="warning" dot>Blocked · intercepted</Chip>
        <span className="text-[0.6875rem] text-muted-foreground">answered 200 OK — with someone else&apos;s page</span>
      </span>
    );
  }
  return (
    <Chip tone="warning" dot>
      {result.state === "bypass" ? "Redirect not enforced" : "Blocked"}
    </Chip>
  );
}

function ProxyTestBlock({
  state,
  onTest,
  onTestCustom,
  operator,
  probeLine,
  customReject,
  customDraft,
  onCustomDraftChange,
  hideButton,
}: {
  state: ProbeUiState;
  /** The default multi-target check ("Test connectivity" / "Test again"). */
  onTest: () => void;
  /** The custom-endpoint probe — fired by Enter in the custom field here; the
   *  gate row's relabeled button is the primary launch point (steps.ts). */
  onTestCustom: () => void;
  operator: boolean;
  /** What the builtin probe is about to do, endpoints and chain named — shown while running. */
  probeLine: string;
  /** A rejected custom URL's server message, rendered inline in the custom block (never a toast — T.CUSTOM_REJECT_WHY). */
  customReject: string | null;
  /** Lifted to the orchestrator (CorpNetworkState.customDraft): the gate's
   *  action label derives from it ("Test this URL" once non-empty). */
  customDraft: string;
  onCustomDraftChange: (v: string) => void;
  /** One Test button per screen (the mock's stepTest): while the gate row
   *  below carries the action, the panel's own button is suppressed and this
   *  block shows only the result/hint. Once the gate has moved on to Next,
   *  the button returns here — as "Test again" — so re-testing stays
   *  reachable. */
  hideButton: boolean;
}) {
  const running = state.kind === "running";
  const hasResult = state.kind === "done";
  const customPass = state.kind === "done" && state.result.state === "reached" && state.result.custom;
  return (
    <div
      className={cn(
        // overflow-wrap inherits: probe results name real endpoints
        // (www.msftconnecttest.com/connecttest.txt) — unbreakable tokens whose
        // min-content width exceeds any narrow container.
        "flex items-start gap-3 rounded-lg border p-3 [overflow-wrap:anywhere]",
        // The mock's okcustom container: a dashed info frame, so even the box
        // around a custom pass reads differently from a verified one.
        customPass ? "border-dashed border-info/40" : "border-border",
      )}
    >
      {!hideButton && (
        <Button size="sm" variant="outline" className="shrink-0" disabled={running || !operator} title={!operator ? T.VIEWER_HINT : undefined} onClick={onTest}>
          {running ? <Loader2 className="size-3.5 animate-spin" /> : null}
          {running ? "Testing…" : hasResult ? "Test again" : "Test connectivity"}
        </Button>
      )}
      <div className="min-w-0 flex-1 space-y-1">
        {state.kind === "idle" && (
          <>
            <span className="text-[0.6875rem] text-muted-foreground">Not tested</span>
            <p className="text-[0.6875rem] leading-snug text-muted-foreground">{T.TEST_PROXY_HINT}</p>
          </>
        )}
        {state.kind === "running" && (
          <>
            <p className="text-[0.75rem] text-info">Starting a throwaway sandbox — {state.elapsedSec}s</p>
            {!state.custom && <p className="text-[0.6875rem] leading-snug text-muted-foreground">{probeLine}</p>}
          </>
        )}
        {state.kind === "done" && (
          <>
            <ProxyVerdict result={state.result} />
            {state.result.state === "no_runner" ? (
              // The canon sentence, not the wire detail: it says what to DO
              // (configure a barrier), which the server's own line doesn't.
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">{T.TEST_NORUNNER}</p>
            ) : state.result.state === "not_run" ? (
              // Unlike no_runner, the wire detail DOES carry the specific
              // launch failure (an image pull, a confinement class this host
              // can't enforce) — worth showing. No TEST_STANDING though:
              // nothing was actually tested from a sandbox here.
              <>
                <p className="text-[0.75rem] leading-snug text-foreground">{state.result.detail}</p>
                <p className="text-[0.6875rem] leading-snug text-muted-foreground">{T.NOT_RUN_NOTE}</p>
              </>
            ) : (
              <>
                <p className="text-[0.75rem] leading-snug text-foreground">{state.result.detail}</p>
                {state.result.custom && state.result.state === "reached" && (
                  <p className="text-[0.75rem] leading-snug text-info">{T.CUSTOM_CAVEAT}</p>
                )}
                {state.result.intercepted && (
                  <>
                    <div className="rounded-md border border-border bg-muted/40 px-2.5 py-2">
                      <p className="text-[0.75rem] leading-snug text-foreground">{T.INTERCEPT_MEANS}</p>
                    </div>
                    <p className="max-w-[620px] text-[0.6875rem] leading-snug text-muted-foreground">{T.PROBE_ENDPOINTS}</p>
                  </>
                )}
                <p className="text-[0.6875rem] leading-snug text-muted-foreground">{T.TEST_STANDING}</p>
              </>
            )}
            {/* Revealed ONLY after a failure — never on arrival, or everyone
                reaches for it instead of fixing the proxy and the gate goes
                decorative. A recovery affordance, not configuration. Covers
                intercepted too (it is a blocked flavor). No button of its own
                while the gate row owns the action: the gate relabels itself
                to "Test this URL" the moment this field is non-empty; Enter
                here fires the same probe. */}
            {state.result.state === "blocked" && (
              <div className="mt-1.5 space-y-2.5 rounded-lg border border-dashed border-border-strong p-3">
                <div className="space-y-1">
                  <p className="text-[0.8125rem] font-medium text-foreground">No public endpoint will answer here?</p>
                  <p className="max-w-[560px] text-[0.6875rem] leading-snug text-muted-foreground">{T.CUSTOM_URL_WHY}</p>
                </div>
                <div className="flex items-end gap-2">
                  <Field label="Test against a URL of your own" htmlFor="corp-custom-url" hint={T.CUSTOM_URL_HINT} className="min-w-0 flex-1">
                    <Input
                      id="corp-custom-url"
                      value={customDraft}
                      onChange={(e) => onCustomDraftChange(e.target.value)}
                      onKeyDown={(e) => {
                        if (e.key === "Enter" && customDraft.trim() && operator) onTestCustom();
                      }}
                      placeholder="https://nexus.corp.internal/repository/health"
                      className="font-mono"
                    />
                  </Field>
                  {!hideButton && (
                    <Button
                      size="sm"
                      variant="outline"
                      className="shrink-0"
                      disabled={!operator || !customDraft.trim()}
                      onClick={onTestCustom}
                    >
                      Test this URL
                    </Button>
                  )}
                </div>
                {customReject && (
                  <div className="space-y-1.5">
                    <div className="rounded-md border border-danger/30 bg-danger-subtle px-2.5 py-2">
                      <p className="text-[0.75rem] leading-snug text-danger">{customReject}</p>
                    </div>
                    <p className="text-[0.6875rem] leading-snug text-muted-foreground">{T.CUSTOM_REJECT_WHY}</p>
                  </div>
                )}
              </div>
            )}
          </>
        )}
      </div>
    </div>
  );
}

// Everything ProxyTestBlock needs, owned by CorpNetworkStep (NOT this tab):
// the probe can be fired from the step's gate row while EITHER tab is up, and
// its state must survive switching tabs — so the tab only renders it.
export interface ProxyProbeBundle {
  state: ProbeUiState;
  onTest: () => void;
  onTestCustom: () => void;
  probeLine: string;
  customReject: string | null;
  customDraft: string;
  onCustomDraftChange: (v: string) => void;
  hideButton: boolean;
}

export function HostProxyTab({
  siteConfig,
  detection,
  hostProxyCheck,
  secretNames,
  mutate,
  saving,
  operator,
  onRecheck,
  probe,
}: {
  siteConfig: SiteConfig | null;
  detection: HostProxyDetection | undefined;
  /** See EvidenceBlock/HostProxyCheckNote (W13-S1-1) — the server's graded verdict on this detection. */
  hostProxyCheck?: SetupCheck;
  /** W12-W12-C-3: the store's actual secret names — lets this tab notice when
   *  a configured upstream_proxy_secret_ref no longer resolves to anything
   *  (deleted/renamed elsewhere), and gates AddSecretDialog's overwrite warning.
   *  Undefined (not `[]`) means unknown, not empty — see CorpNetworkStep's doc. */
  secretNames?: string[];
  mutate: (next: SiteConfig, errorMessage: string) => Promise<boolean>;
  saving: boolean;
  operator: boolean;
  onRecheck: () => void;
  probe: ProxyProbeBundle;
}) {
  const [url, setUrl] = React.useState("");
  const [useSecret, setUseSecret] = React.useState(false);
  const [disclosureOpen, setDisclosureOpen] = React.useState(false);
  const [secretName, setSecretName] = React.useState("upstream-proxy-url");
  const [addSecretOpen, setAddSecretOpen] = React.useState(false);
  const [selected, setSelected] = React.useState(0);

  // Seed from the freshest doc exactly once — a later reload (Re-check) must
  // never stomp an in-progress edit. (Inherited from the retired HostProxyStep,
  // whose seededRef this is; this step is the only proxy surface left.)
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
  // W12-W12-C-3: a referenced secret can vanish out from under this config —
  // deleted or rotated away from the Secrets screen, which has no idea this
  // ref exists to warn about it — leaving "Chaining through the URL in secret
  // X" claiming a proxy that no longer resolves to anything. secretNames is
  // the store's actual list (the orchestrator's own recheck already fetches
  // it), so this reads as a live presence check, not a second store.
  const secretRefDangling =
    !!siteConfig?.upstream_proxy_secret_ref && !!secretNames && !secretNames.includes(siteConfig.upstream_proxy_secret_ref);

  const saveUrl = async (value: string) => {
    if (await mutate({ ...(siteConfig ?? {}), upstream_proxy_url: value || undefined, upstream_proxy_secret_ref: undefined }, "Failed to save the upstream proxy")) {
      setUrl(value);
      setUseSecret(false);
      toast.success(value ? "Upstream proxy saved" : "Upstream proxy removed");
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
      // The secret above was just created — the orchestrator's secretNames
      // (last fetched at mount/Re-check) doesn't know about it yet, so
      // secretRefDangling would otherwise misread this freshly-saved ref as
      // "no longer exists in the store" the instant it's set. onRecheck
      // re-fetches secretNames (+ status/siteConfig) so the just-created name
      // resolves immediately instead of only after a manual Re-check.
      onRecheck();
    }
  };

  const saveSecretRef = async (name: string) => {
    const trimmed = name.trim();
    if (!trimmed) return;
    if (await mutate({ ...(siteConfig ?? {}), upstream_proxy_secret_ref: trimmed, upstream_proxy_url: undefined }, "Failed to save the proxy secret reference")) {
      toast.success("Upstream proxy saved");
      // Same staleness gap as saveAsSecret above — most commonly hit right
      // after the "Add secret…" dialog just created `name` for real.
      onRecheck();
    }
  };

  const draftHasCreds = !useSecret && hasUserinfo(url);

  return (
    <div className="space-y-4">
      <EvidenceBlock
        rows={rows}
        showUse={showUse}
        onUse={(value) => (hasUserinfo(value) ? setUrl(value) : saveUrl(value))}
        onRecheck={onRecheck}
        operator={operator}
        hostProxyCheck={hostProxyCheck}
      />

      <div className="space-y-3 rounded-xl border border-border bg-card p-3.5">
        <BlockLabel>{T.CONFIG_HEAD}</BlockLabel>
        {/* Driven by what's actually SAVED (siteConfig), never by which input
            mode (useSecret) happens to be open — switching to "Enter a URL
            instead" on a secret-only config must keep naming the secret, not
            claim "Chaining through" a plain URL that was never set. */}
        <ConfigStatusLine tone={secretRefDangling ? "warning" : configured ? "success" : "neutral"}>
          {siteConfig?.upstream_proxy_url ? (
            <>Chaining through <Mono className="text-xs">{siteConfig.upstream_proxy_url}</Mono></>
          ) : siteConfig?.upstream_proxy_secret_ref ? (
            secretRefDangling ? (
              <>
                Secret <Mono className="text-xs">{siteConfig.upstream_proxy_secret_ref}</Mono> no longer exists in the
                store — sandboxes go direct, not through a proxy, despite this looking configured
              </>
            ) : (
              <>
                Chaining through the URL in secret <Mono className="text-xs">{siteConfig.upstream_proxy_secret_ref}</Mono> — write-only, so
                the URL can&apos;t be shown here
              </>
            )
          ) : (
            T.NOT_CONFIGURED
          )}
        </ConfigStatusLine>

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
              <div className="flex items-center gap-2">
                <Button
                  size="sm"
                  // An empty URL here would PUT both upstream_proxy_url and
                  // upstream_proxy_secret_ref undefined — fine when nothing was
                  // configured yet, but a silent delete of a configured proxy
                  // (e.g. switching from the secret field via "Enter a URL
                  // instead" without typing one) otherwise. Removing one on
                  // purpose is the explicit "Remove proxy" control beside it.
                  disabled={!operator || saving || (!url.trim() && isProxyConfigured(siteConfig))}
                  onClick={() => saveUrl(url)}
                >
                  {saving ? <Loader2 className="size-3.5 animate-spin" /> : "Save"}
                </Button>
                {/* W13-S1-1: an unreachable saved proxy hard-locks the operator
                    behind the mandatory Corporate network gate — Save alone
                    can't get back to "no proxy" (it refuses an empty URL to
                    guard against an accidental clear, above), so "no proxy" needs
                    its own explicit, un-ambiguous exit right where a proxy was set. */}
                {isProxyConfigured(siteConfig) && (
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={!operator || saving}
                    onClick={() => saveUrl("")}
                  >
                    Remove proxy
                  </Button>
                )}
              </div>
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

      <ProxyTestBlock {...probe} operator={operator} />

      <AddSecretDialog
        open={addSecretOpen}
        onOpenChange={setAddSecretOpen}
        lockName
        initialName={secretName}
        // W12-W12-C-3: without this, the dialog's own overwrite-confirm gate
        // (secrets.tsx) never fires — an operator typing an in-use name here
        // silently clobbers whatever that secret already held, no different
        // from every other AddSecretDialog caller that DOES pass this.
        existingNames={secretNames}
        onSaved={(name) => {
          setAddSecretOpen(false);
          saveSecretRef(name);
        }}
      />
    </div>
  );
}
