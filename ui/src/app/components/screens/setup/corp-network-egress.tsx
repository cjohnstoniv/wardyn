/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// corp-network-egress.tsx — the Egress redirection sub-tab of the Corporate
// network step, split from corp-network-step.tsx along the design's own seam
// (the two sub-tabs) when the file crossed the size gate. Also home to the
// probe-result UI primitives (ProbeUiState / TestVerdictChip / useElapsedTimer
// / compactEndpoint) BOTH tabs render with: they live in this leaf so imports
// flow one way, step -> egress, and the two files cannot cycle.
import * as React from "react";
import { ChevronsUpDown, Check, Loader2 } from "lucide-react";
import { toast } from "sonner";
import type { EgressRedirect, SiteConfig } from "../../../lib/types";
import { health as healthApi, type ProxyTestResult } from "../../../lib/api/health";
import { getErrorMessage } from "../../../lib/format";
import { T, EGRESS_SUGGEST, ECOSYSTEM_CONFIG_FILE } from "../../../lib/integrations";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "../../ui/popover";
import { Command, CommandEmpty, CommandGroup, CommandInput, CommandItem, CommandList } from "../../ui/command";
import { cn } from "../../ui/utils";
import { Field } from "../new-run/step-shell";
import { Mono } from "../../wardyn/code-block";
import { Chip } from "../../wardyn/primitives";

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

export function TestVerdictChip({ state }: { state: ProxyTestResult["state"] }) {
  if (state === "reached") return <Chip tone="success" dot>Reached</Chip>;
  if (state === "no_runner") return <Chip tone="neutral">Can&apos;t test here</Chip>;
  return <Chip tone="warning" dot>{state === "bypass" ? "Redirect not enforced" : "Blocked"}</Chip>;
}

// ------------------------------------------------------------
// Host proxy tab
// ------------------------------------------------------------
export type ProbeUiState =
  | { kind: "idle" }
  // custom marks a custom-URL retry: the running view skips the builtin
  // probeLine (it names endpoints this run will not try).
  | { kind: "running"; elapsedSec: number; custom?: boolean }
  | { kind: "done"; result: ProxyTestResult };

export function useElapsedTimer(running: boolean): number {
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
    <Chip tone="neutral" className="shrink-0 text-[0.625rem] opacity-70" title={T.NET_ONLY_TIP}>
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
      {/* What this row actually DOES at run start — the ecosystem tier also
          writes a tool config file; everything else is network-only, and
          needs nothing more (the mock's expanded-row mechanism line). */}
      <p className="text-[0.6875rem] leading-snug text-muted-foreground">
        {r.ecosystem && ECOSYSTEM_CONFIG_FILE[r.ecosystem]
          ? `Runs also get a generated ${ECOSYSTEM_CONFIG_FILE[r.ecosystem]} pointing at the mirror — the network substitution covers anything that ignores it.`
          : T.NET_ONLY_TIP}
      </p>
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
  // The suggestions are a shortcut, not the menu: redirecting a private host or
  // a bare IP is the whole reason this stopped being "artifact registries".
  // Without a CommandInput the list is select-only, CommandEmpty can never
  // render (nothing filters it), and the copy promising "type anything" is a
  // lie the UI can't keep.
  const [query, setQuery] = React.useState("");
  const typed = query.trim();
  const isNovel = typed !== "" && !EGRESS_SUGGEST.some(([url]) => url === typed);
  const pick = (v: string) => {
    onChange(v);
    setQuery("");
    setOpen(false);
  };
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
          <CommandInput value={query} onValueChange={setQuery} placeholder="https://…, host, or IP" className="font-mono" />
          <CommandList>
            <CommandEmpty>Or type anything — a full URL, a bare host, or an IP. It&apos;s redirected exactly as entered.</CommandEmpty>
            {isNovel && (
              // forceMount on the GROUP too, not just the item: cmdk counts only
              // items its filter SCORES, and a forced item scores nothing — so a
              // group left to the filter collapses to display:none and takes the
              // forced item down with it. Without this the typed host is in the
              // DOM but unclickable, and the CommandEmpty copy right above
              // ("type anything … redirected exactly as entered") is a promise
              // the UI cannot keep. The `isNovel &&` guard means the group only
              // exists when there IS a typed value, so nothing empty is forced.
              <CommandGroup forceMount>
                {/* forceMount + a value cmdk's filter always keeps: the typed
                    string must stay selectable even when it matches nothing. */}
                <CommandItem key="__custom" value={typed} forceMount onSelect={() => pick(typed)} className="font-mono">
                  <Check className="size-3.5 opacity-0" />
                  {typed}
                  <span className="ml-auto font-sans text-[0.6875rem] text-muted-foreground">use as typed</span>
                </CommandItem>
              </CommandGroup>
            )}
            <CommandGroup>
              {EGRESS_SUGGEST.map(([url, eco]) => (
                <CommandItem
                  key={url}
                  value={url}
                  // pick(url), not pick(v) — cmdk lowercases the value it hands
                  // back, and a redirect target is dialed verbatim.
                  onSelect={() => pick(url)}
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

export function EgressTab({
  siteConfig,
  mutate,
  operator,
  initialProbes,
  onProbeResult,
  testAllSignal = 0,
}: {
  siteConfig: SiteConfig | null;
  mutate: (next: SiteConfig, errorMessage: string) => Promise<boolean>;
  operator: boolean;
  /** Each redirect's last result from a PRIOR visit to this step, keyed by `from` (see setup-screen.tsx's corpGate) — seeds each row back to what it last observed. */
  initialProbes: Record<string, ProxyTestResult>;
  /** Reports a redirect's terminal probe result upward, by `from`, so it survives leaving/re-entering the step and can gate Next (see steps.ts's corpNetworkGate). */
  onProbeResult: (from: string, result: ProxyTestResult) => void;
  /** Bumped by the footer's "Test all redirects" gate action (corp-network-step.tsx) — each increment fires testAll once this tab is mounted. */
  testAllSignal?: number;
}) {
  const redirects = siteConfig?.egress_redirects ?? [];
  const [expandedIdx, setExpandedIdx] = React.useState<number | null>(null);
  // Seeded once at mount only (the lazy-initializer form runs exactly once) —
  // same rationale as HostProxyTab's `test` state and the file's existing
  // seededRef pattern: a later reload must never stomp in-progress state.
  // Keyed by `from` — the same identity redirectProbes/initialProbes already
  // use — not array index: an index is positional, so removing a redirect
  // shifts every LATER row into the slot the deleted row's verdict still
  // occupies, showing the wrong row as tested.
  const [testStates, setTestStates] = React.useState<Record<string, ProbeUiState>>(() => {
    const seeded: Record<string, ProbeUiState> = {};
    redirects.forEach((r) => {
      const prior = initialProbes[r.from];
      if (prior) seeded[r.from] = { kind: "done", result: prior };
    });
    return seeded;
  });

  const setRedirects = (next: EgressRedirect[]) => mutate({ ...(siteConfig ?? {}), egress_redirects: next }, "Failed to save the egress redirect");

  const runTest = async (r: EgressRedirect) => {
    setTestStates((s) => ({ ...s, [r.from]: { kind: "running", elapsedSec: 0 } }));
    try {
      const result = await healthApi.testRedirect(r.from, r.to);
      setTestStates((s) => ({ ...s, [r.from]: { kind: "done", result } }));
      onProbeResult(r.from, result);
    } catch (e) {
      // See the proxy tab's runTest: a failed request is not a "blocked" verdict.
      toast.error(`Could not test the ${r.from} redirect`, { description: getErrorMessage(e) });
      setTestStates((s) => ({ ...s, [r.from]: { kind: "idle" } }));
    }
  };
  const testAll = () => redirects.forEach((r) => runTest(r));
  // The footer's gate action lands as a bumped counter: the step switches to
  // this tab and increments, and the freshly-mounted tab fires the sweep.
  const firedSignal = React.useRef(0);
  React.useEffect(() => {
    if (testAllSignal > 0 && testAllSignal !== firedSignal.current) {
      firedSignal.current = testAllSignal;
      testAll();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- testAll is re-created per render; the signal is the trigger
  }, [testAllSignal]);

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
                    testState={testStates[r.from] ?? { kind: "idle" }}
                    onExpand={() => setExpandedIdx(i)}
                    onTest={() => runTest(r)}
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
      {/* No forced visit any more (steps.ts), so no check icon dressed as an
          answer the operator never gave — just the quiet fact. */}
      {redirects.length === 0 && (
        <p className="max-w-[560px] text-[0.8125rem] leading-snug text-muted-foreground">{T.EGRESS_SEEN_EMPTY}</p>
      )}
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

