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
import * as React from "react";
import { ArrowUpRight, ChevronDown, ChevronsUpDown, Check, Loader2 } from "lucide-react";
import { Link } from "react-router-dom";
import { toast } from "sonner";
import type { EgressRedirect, HostProxyDetection, HostProxySetting, SetupStatus, SiteConfig } from "../../../lib/types";
import { health as healthApi, type ProxyTestResult } from "../../../lib/api/health";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { getErrorMessage } from "../../../lib/format";
import { T, EGRESS_SUGGEST } from "../../../lib/integrations";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Tabs, TabsList, TabsTrigger } from "../../ui/tabs";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { Popover, PopoverContent, PopoverTrigger } from "../../ui/popover";
import { Command, CommandEmpty, CommandGroup, CommandItem, CommandList } from "../../ui/command";
import { cn } from "../../ui/utils";
import { Field } from "../new-run/step-shell";
import { Mono } from "../../wardyn/code-block";
import { Chip } from "../../wardyn/primitives";
import { useOperator } from "../../wardyn/operator-context";
import { AddSecretDialog } from "../secrets";
import { useSiteConfigStep } from "./step-bodies";

// ------------------------------------------------------------
// Pure helpers — exported so setup-screen.tsx's badge/done derivation reads
// the SAME facts this step renders from (single source of truth, no drift).
// ------------------------------------------------------------
export function isProxyConfigured(cfg: SiteConfig | null): boolean {
  return !!(cfg?.upstream_proxy_url || cfg?.upstream_proxy_secret_ref);
}

export function isCorpNetworkConfigured(cfg: SiteConfig | null): boolean {
  return isProxyConfigured(cfg) || (cfg?.egress_redirects?.length ?? 0) > 0;
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

// Display-compaction for a redirect endpoint: drop the scheme, never touch
// the host, elide only the MIDDLE of a long path once the whole thing exceeds
// maxLen. A bare host/IP (no path) is returned as-is regardless of length —
// "host never truncated" has no path to elide in that case anyway.
export function compactEndpoint(raw: string, maxLen = 48): string {
  const noScheme = raw.replace(/^https?:\/\//, "");
  if (noScheme.length <= maxLen) return noScheme;
  const slash = noScheme.indexOf("/");
  if (slash === -1) return noScheme;
  const host = noScheme.slice(0, slash);
  const path = noScheme.slice(slash);
  const budget = Math.max(maxLen - host.length - 1, 6);
  if (path.length <= budget) return host + path;
  const headLen = Math.ceil((budget - 1) / 2);
  const tailLen = Math.floor((budget - 1) / 2);
  return host + path.slice(0, headLen) + "…" + path.slice(path.length - tailLen);
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

function TestVerdictChip({ state }: { state: ProxyTestResult["state"] }) {
  if (state === "reached") return <Chip tone="success" dot>Reached</Chip>;
  if (state === "no_runner") return <Chip tone="neutral">Can&apos;t test here</Chip>;
  return <Chip tone="warning" dot>{state === "bypass" ? "Redirect not enforced" : "Blocked"}</Chip>;
}

// ------------------------------------------------------------
// Host proxy tab
// ------------------------------------------------------------
type ProbeUiState = { kind: "idle" } | { kind: "running"; elapsedSec: number } | { kind: "done"; result: ProxyTestResult };

function useElapsedTimer(running: boolean): number {
  const [sec, setSec] = React.useState(0);
  React.useEffect(() => {
    if (!running) {
      setSec(0);
      return;
    }
    const start = Date.now();
    const id = setInterval(() => setSec(Math.round((Date.now() - start) / 1000)), 1000);
    return () => clearInterval(id);
  }, [running]);
  return sec;
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

function ProxyTestBlock({ state, onTest, operator }: { state: ProbeUiState; onTest: () => void; operator: boolean }) {
  const running = state.kind === "running";
  return (
    <div className="flex items-start gap-3 rounded-lg border border-border p-3">
      <Button size="sm" variant="outline" className="shrink-0" disabled={running || !operator} title={!operator ? T.VIEWER_HINT : undefined} onClick={onTest}>
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
}: {
  siteConfig: SiteConfig | null;
  detection: HostProxyDetection | undefined;
  mutate: (next: SiteConfig, errorMessage: string) => Promise<boolean>;
  saving: boolean;
  operator: boolean;
  onRecheck: () => void;
}) {
  const [url, setUrl] = React.useState("");
  const [useSecret, setUseSecret] = React.useState(false);
  const [disclosureOpen, setDisclosureOpen] = React.useState(false);
  const [secretName, setSecretName] = React.useState("upstream-proxy-url");
  const [addSecretOpen, setAddSecretOpen] = React.useState(false);
  const [selected, setSelected] = React.useState(0);
  const [test, setTest] = React.useState<ProbeUiState>({ kind: "idle" });

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

  const runTest = async () => {
    setTest({ kind: "running", elapsedSec: 0 });
    try {
      const result = await healthApi.testProxy();
      setTest({ kind: "done", result });
    } catch (e) {
      setTest({ kind: "done", result: { state: "blocked", detail: getErrorMessage(e) } });
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
// Egress redirection tab
// ------------------------------------------------------------
function TokenChip({ tokenRef }: { tokenRef: string }) {
  return (
    <Chip tone="neutral" mono className="shrink-0 text-[0.625rem]" title={`token: ${tokenRef} — injected proxy-side at fetch time; the sandbox never holds it`}>
      token
    </Chip>
  );
}

function NetworkOnlyChip() {
  return (
    <Chip
      tone="neutral"
      className="shrink-0 text-[0.625rem] opacity-70"
      title="Network only — egress is substituted and the token is injected, but no per-tool config file is generated (there's no config-file equivalent for this destination)."
    >
      network only
    </Chip>
  );
}

function RedirectRow({
  r,
  testState,
  onExpand,
  onTest,
  onRemove,
  operator,
}: {
  r: EgressRedirect;
  testState: ProbeUiState;
  onExpand: () => void;
  onTest: () => void;
  onRemove: () => void;
  operator: boolean;
}) {
  const testing = testState.kind === "running";
  return (
    <div
      className="flex cursor-pointer items-center gap-2.5 p-2.5 hover:bg-muted/40"
      title={`${r.from} → ${r.to}${r.token_secret_ref ? ` · token: ${r.token_secret_ref}` : ""}`}
      onClick={onExpand}
    >
      <Mono className="shrink-0 text-xs text-foreground">{compactEndpoint(r.from)}</Mono>
      <span className="shrink-0 text-[0.6875rem] text-muted-foreground">&rarr;</span>
      <Mono className="min-w-0 flex-1 truncate text-xs text-foreground">{compactEndpoint(r.to)}</Mono>
      {!r.ecosystem && <NetworkOnlyChip />}
      {r.token_secret_ref && <TokenChip tokenRef={r.token_secret_ref} />}
      {testState.kind === "idle" && <span className="shrink-0 text-[0.6875rem] text-muted-foreground">Not tested</span>}
      {testState.kind === "running" && (
        <span className="inline-flex shrink-0 items-center gap-1.5 text-[0.6875rem] text-info">
          <Loader2 className="size-3 animate-spin" /> testing &middot; {testState.elapsedSec}s
        </span>
      )}
      {testState.kind === "done" && <TestVerdictChip state={testState.result.state} />}
      <Button
        size="sm"
        variant="outline"
        className="shrink-0"
        disabled={!operator || testing}
        title={!operator ? T.VIEWER_HINT : undefined}
        onClick={(e) => {
          e.stopPropagation();
          onTest();
        }}
      >
        {testing ? "Testing…" : "Test"}
      </Button>
      <button
        type="button"
        aria-label={`Remove ${r.from} redirect`}
        title={!operator ? T.VIEWER_HINT : "Remove"}
        disabled={!operator}
        className="inline-flex size-7 shrink-0 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-muted hover:text-foreground disabled:pointer-events-none disabled:opacity-40"
        onClick={(e) => {
          e.stopPropagation();
          onRemove();
        }}
      >
        &times;
      </button>
    </div>
  );
}

function RedirectRowExpanded({
  r,
  onSave,
  onCancel,
  onRemove,
}: {
  r: EgressRedirect;
  onSave: (next: EgressRedirect) => void;
  onCancel: () => void;
  onRemove: () => void;
}) {
  const [from, setFrom] = React.useState(r.from);
  const [to, setTo] = React.useState(r.to);
  const [token, setToken] = React.useState(r.token_secret_ref ?? "");
  return (
    <div className="space-y-2.5 bg-primary/5 p-3">
      <div className="flex items-center gap-2">
        <span className="text-[0.6875rem] font-medium uppercase tracking-wide text-muted-foreground">Editing — full values</span>
        <span className="flex-1" />
        <button type="button" className="text-xs text-muted-foreground hover:text-foreground" onClick={onCancel}>
          collapse
        </button>
      </div>
      <div className="grid grid-cols-2 gap-2.5">
        <Field label="From" htmlFor="eg-edit-from">
          <Input id="eg-edit-from" value={from} onChange={(e) => setFrom(e.target.value)} className="font-mono" />
        </Field>
        <Field label="To" htmlFor="eg-edit-to">
          <Input id="eg-edit-to" value={to} onChange={(e) => setTo(e.target.value)} className="font-mono" />
        </Field>
      </div>
      <Field label="Token secret name (optional)" htmlFor="eg-edit-token" hint="Injected proxy-side at fetch time — the sandbox never holds it.">
        <Input id="eg-edit-token" value={token} onChange={(e) => setToken(e.target.value)} placeholder="artifactory-token" className="font-mono" />
      </Field>
      <div className="flex items-center gap-2">
        <Button size="sm" onClick={() => onSave({ ...r, from: from.trim(), to: to.trim(), token_secret_ref: token.trim() || undefined })}>
          Save
        </Button>
        <Button size="sm" variant="ghost" onClick={onCancel}>
          Cancel
        </Button>
        <span className="flex-1" />
        <Button size="sm" variant="ghost" className="text-danger hover:text-danger" onClick={onRemove}>
          Remove
        </Button>
      </div>
    </div>
  );
}

function FromCombobox({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const [open, setOpen] = React.useState(false);
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button type="button" variant="outline" role="combobox" aria-expanded={open} className="w-full justify-between font-mono">
          <span className={cn("truncate", !value && "font-sans text-muted-foreground")}>{value || "https://…, host, or IP"}</span>
          <ChevronsUpDown className="size-4 shrink-0 opacity-50" />
        </Button>
      </PopoverTrigger>
      <PopoverContent className="w-[420px] p-0" align="start">
        <Command>
          <CommandList>
            <CommandEmpty>Or type anything — a full URL, a bare host, or an IP. It&apos;s redirected exactly as entered.</CommandEmpty>
            <CommandGroup>
              {EGRESS_SUGGEST.map(([url, eco]) => (
                <CommandItem
                  key={url}
                  value={url}
                  onSelect={(v) => {
                    onChange(v);
                    setOpen(false);
                  }}
                  className="justify-between font-mono"
                >
                  <span className="inline-flex items-center gap-2">
                    <Check className={cn("size-3.5", value === url ? "opacity-100" : "opacity-0")} />
                    {url}
                  </span>
                  <span className="font-sans text-[0.6875rem] text-muted-foreground">{eco}</span>
                </CommandItem>
              ))}
            </CommandGroup>
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}

// The mock's ecosystem behavior: a redirect emits a per-tool config file only
// when `from` is exactly one of the well-known ecosystem registries; anything
// else (free text, a bare host/IP, a container registry) is network-only.
function ecosystemFor(from: string): string | undefined {
  return EGRESS_SUGGEST.find(([url]) => url === from)?.[1];
}

function AddRedirectForm({ onAdd, operator }: { onAdd: (r: EgressRedirect) => void; operator: boolean }) {
  const [from, setFrom] = React.useState("");
  const [to, setTo] = React.useState("");
  const [token, setToken] = React.useState("");

  const add = () => {
    const f = from.trim();
    const t = to.trim();
    if (!f || !t) return;
    const eco = ecosystemFor(f);
    onAdd({ from: f, to: t, token_secret_ref: token.trim() || undefined, ecosystem: eco && eco !== "container images" ? eco : undefined });
    setFrom("");
    setTo("");
    setToken("");
  };

  return (
    <div className="space-y-3 rounded-xl border border-border p-4">
      <div className="grid grid-cols-2 gap-2.5">
        <Field label="From" htmlFor="eg-add-from" hint="The public endpoint a run would reach — pick a common source or type any URL, host, or IP.">
          <FromCombobox value={from} onChange={setFrom} />
        </Field>
        <Field label="To" htmlFor="eg-add-to">
          <Input id="eg-add-to" value={to} onChange={(e) => setTo(e.target.value)} placeholder="https://artifactory.corp.internal/… — or a host/IP" className="font-mono" />
        </Field>
      </div>
      <Field label="Token secret name (optional)" htmlFor="eg-add-token" hint="Injected proxy-side at fetch time — the sandbox never holds it.">
        <Input id="eg-add-token" value={token} onChange={(e) => setToken(e.target.value)} placeholder="artifactory-token" className="font-mono" />
      </Field>
      <Button variant="outline" size="sm" disabled={!operator || !from.trim() || !to.trim()} onClick={add}>
        + Add redirect
      </Button>
    </div>
  );
}

function EgressTab({
  siteConfig,
  mutate,
  operator,
}: {
  siteConfig: SiteConfig | null;
  mutate: (next: SiteConfig, errorMessage: string) => Promise<boolean>;
  operator: boolean;
}) {
  const redirects = siteConfig?.egress_redirects ?? [];
  const [expandedIdx, setExpandedIdx] = React.useState<number | null>(null);
  const [testStates, setTestStates] = React.useState<Record<number, ProbeUiState>>({});

  const setRedirects = (next: EgressRedirect[]) => mutate({ ...(siteConfig ?? {}), egress_redirects: next }, "Failed to save the egress redirect");

  const runTest = async (i: number, r: EgressRedirect) => {
    setTestStates((s) => ({ ...s, [i]: { kind: "running", elapsedSec: 0 } }));
    try {
      const result = await healthApi.testRedirect(r.from, r.to);
      setTestStates((s) => ({ ...s, [i]: { kind: "done", result } }));
    } catch (e) {
      setTestStates((s) => ({ ...s, [i]: { kind: "done", result: { state: "blocked", detail: getErrorMessage(e) } } }));
    }
  };
  const testAll = () => redirects.forEach((r, i) => runTest(i, r));

  return (
    <div className="space-y-3.5">
      <p className="text-sm leading-relaxed text-muted-foreground">{T.EGRESS_DESC}</p>
      {redirects.length > 0 && (
        <>
          <div className="rounded-lg border border-border">
            {redirects.map((r, i) =>
              i === expandedIdx ? (
                <div key={i} className={i > 0 ? "border-t border-border" : undefined}>
                  <RedirectRowExpanded
                    r={r}
                    onCancel={() => setExpandedIdx(null)}
                    onSave={(next) => {
                      const copy = [...redirects];
                      copy[i] = next;
                      setRedirects(copy);
                      setExpandedIdx(null);
                    }}
                    onRemove={() => {
                      setRedirects(redirects.filter((_, j) => j !== i));
                      setExpandedIdx(null);
                    }}
                  />
                </div>
              ) : (
                <div key={i} className={i > 0 ? "border-t border-border" : undefined}>
                  <TickingRow
                    r={r}
                    testState={testStates[i] ?? { kind: "idle" }}
                    onExpand={() => setExpandedIdx(i)}
                    onTest={() => runTest(i, r)}
                    onRemove={() => setRedirects(redirects.filter((_, j) => j !== i))}
                    operator={operator}
                  />
                </div>
              ),
            )}
          </div>
          <div className="flex items-center gap-2">
            <p className="text-[0.6875rem] text-muted-foreground">Full values on hover — click a row to edit.</p>
            <span className="flex-1" />
            <Button size="sm" variant="outline" disabled={!operator} onClick={testAll}>
              Test all
            </Button>
          </div>
        </>
      )}
      {redirects.length === 0 && <p className="text-[0.8125rem] leading-snug text-muted-foreground">{T.EMPTY_EGRESS}</p>}
      <AddRedirectForm operator={operator} onAdd={(r) => setRedirects([...redirects, r])} />
    </div>
  );
}

// RedirectRow's elapsed-seconds ticker needs its own interval while THAT row
// is running — lifted into a tiny wrapper so EgressTab's testStates map stays
// a plain record of terminal results; each row ticks its OWN clock locally
// (useElapsedTimer only resets when `running` flips, so a sibling row's Test
// All click re-rendering this one doesn't touch it) — no need to write the
// live count back into the shared map for that to work.
function TickingRow(props: {
  r: EgressRedirect;
  testState: ProbeUiState;
  onExpand: () => void;
  onTest: () => void;
  onRemove: () => void;
  operator: boolean;
}) {
  const { testState, ...rest } = props;
  const elapsed = useElapsedTimer(testState.kind === "running");
  const live: ProbeUiState = testState.kind === "running" ? { kind: "running", elapsedSec: elapsed } : testState;
  return <RedirectRow {...rest} testState={live} />;
}

// ------------------------------------------------------------
// Top-level step
// ------------------------------------------------------------
export function CorpNetworkStep({
  status,
  siteConfig,
  reloadSiteConfig,
  saveSiteConfig,
  skipped,
  onSkip,
}: {
  status: SetupStatus;
  siteConfig: SiteConfig | null;
  reloadSiteConfig: () => Promise<void>;
  saveSiteConfig: (next: SiteConfig) => Promise<void>;
  skipped: boolean;
  onSkip: () => void;
}) {
  const operator = useOperator();
  const [tab, setTab] = React.useState<"proxy" | "egress">("proxy");
  const { saving, mutate } = useSiteConfigStep(reloadSiteConfig, saveSiteConfig);
  const configured = isCorpNetworkConfigured(siteConfig);

  return (
    <div className="space-y-5">
      <p className="text-sm leading-relaxed text-muted-foreground">{T.CORP_LEDE}</p>

      <Tabs value={tab} onValueChange={(v) => setTab(v as typeof tab)}>
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
        />
      ) : (
        <EgressTab siteConfig={siteConfig} mutate={mutate} operator={operator} />
      )}

      <div className="flex items-center gap-3 border-t border-border pt-4">
        <Link
          to="/integrations"
          className="inline-flex items-center gap-1 text-[0.8125rem] font-medium text-primary hover:underline"
        >
          Manage in Integrations
          <ArrowUpRight className="size-3.5" aria-hidden />
        </Link>
        <span className="flex-1" />
        {!configured && !skipped && (
          <Button size="sm" variant="ghost" onClick={onSkip}>
            Skip this step
          </Button>
        )}
      </div>
    </div>
  );
}
