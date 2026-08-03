/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Integrations — Screen A of the approved mock (mockup/wardyn-integrations.js,
// IntegrationsList). A "row" here is not a backend entity: GET /integrations
// doesn't exist yet, so lib/api/integrations.ts derives every row from the
// three endpoints that DO exist, exactly the discipline lib/scm-provider.ts
// already established for the SCM Provider step. See that file for what each
// field traces back to, and for the `// W5:` seams a later wave replaces.
import * as React from "react";
import { useNavigate } from "react-router-dom";
import { toast } from "sonner";
import {
  ArrowLeftRight,
  Cable,
  GitBranch,
  Loader2,
  MoreHorizontal,
  Network,
  Plus,
  RotateCw,
  Sparkles,
  Trash2,
} from "lucide-react";
import {
  deriveIntegrations,
  describePosture,
  proxyBannerNeeded,
  blastRadius,
  type IntegrationRow,
  type IntegrationsData,
} from "../../../lib/api/integrations";
import { setup as setupApi } from "../../../lib/api/setup";
import { health, type ProxyTestResult } from "../../../lib/api/health";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { T, type IntegrationCategory } from "../../../lib/integrations";
import type { EgressRedirect, SetupStatus, SiteConfig } from "../../../lib/types";
import { getErrorMessage } from "../../../lib/format";
// compactEndpoint is the ONE symbol corp-network-step.tsx exports for reuse
// here (its own row/chip components are page-local, not exported) — see the
// ponytail note on TestVerdictChip below for why the rest is a small,
// deliberate duplicate rather than a cross-wave export ask.
import { compactEndpoint } from "../setup/corp-network-step";
import { Button } from "../../ui/button";
import { Tabs, TabsList, TabsTrigger } from "../../ui/tabs";
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "../../ui/dropdown-menu";
import { Mono } from "../../wardyn/code-block";
import { Chip, OperatorOnlyHint, SectionLabel } from "../../wardyn/primitives";
import { EmptyState, ErrorState, TableSkeleton } from "../../wardyn/states";
import { PageHeader } from "../../wardyn/page-header";
import { DeleteConfirmDialog } from "../../wardyn/delete-confirm-dialog";
import { RESIDENCY_META } from "../../../lib/integrations";
import { OPERATOR_ONLY_REASON } from "../../wardyn/copy";
import { useOperator } from "../../wardyn/operator-context";
import { AddSecretDialog } from "../secrets";
import { canRotateInline, deleteIntegration, primarySecretName } from "./actions";
import { AddIntegrationDialog } from "./add-integration-dialog";
import { ToolsTab } from "./tools-tab";

const CATEGORY_ICON: Record<IntegrationCategory, React.ElementType> = {
  ai_provider: Sparkles,
  scm_host: GitBranch,
  artifact_mirror: ArrowLeftRight,
  host_proxy: Network,
};

const CATEGORY_EMPTY: Record<IntegrationCategory, string> = {
  ai_provider: T.EMPTY_AI,
  scm_host: T.EMPTY_SCM,
  // T.EMPTY_MIRROR was retired with the Corporate-network restructure
  // (mockup2/wardyn-integrations.js) — T.EMPTY_EGRESS is its replacement.
  artifact_mirror: T.EMPTY_EGRESS,
  host_proxy: T.EMPTY_PROXY,
};

const POSTURE_CLASS: Record<"success" | "warning" | "muted", string> = {
  success: "text-success",
  warning: "text-warning",
  muted: "text-muted-foreground",
};

function Posture({ row }: { row: IntegrationRow }) {
  const { text, tone } = describePosture(row.posture);
  return <span className={`text-xs whitespace-nowrap ${POSTURE_CLASS[tone]}`}>{text}</span>;
}

// ---- Test probes (Host proxy row + Egress redirection rows) ----------------
// The only two Test buttons in the product besides the GitHub ref-confinement
// check (T.FOOTNOTE) — a real throwaway-sandbox probe, never a cached or
// inferred verdict. `running` carries no elapsed-seconds tick here (the
// Corporate network step's own version does); this page only needs to match
// its "disables and relabels while running" behavior, not reproduce the timer
// — ponytail: add a tick if this page ever needs live seconds too.
type ProbeUiState = { kind: "idle" } | { kind: "running" } | { kind: "done"; result: ProxyTestResult };

// ponytail: mirrors corp-network-step.tsx's own TestVerdictChip. That module
// exports only compactEndpoint (see the import above) — its row/chip
// components are page-local by design — so this ~5-line twin lives here
// rather than turning a same-wave reuse into a cross-wave export request.
function TestVerdictChip({ result }: { result: ProxyTestResult }) {
  if (result.state === "reached")
    return (
      <Chip tone="success" dot>
        Reached
      </Chip>
    );
  if (result.state === "no_runner") return <Chip tone="neutral">Can&apos;t test here</Chip>;
  return (
    <Chip tone="warning" dot>
      {result.state === "bypass" ? "Redirect not enforced" : result.intercepted ? "Blocked · intercepted" : "Blocked"}
    </Chip>
  );
}

function TestControl({ state, onTest, operator }: { state: ProbeUiState; onTest: () => void; operator: boolean }) {
  const running = state.kind === "running";
  return (
    <>
      {state.kind === "idle" && <span className="shrink-0 text-[0.6875rem] text-muted-foreground">Not tested</span>}
      {running && (
        <span className="inline-flex shrink-0 items-center gap-1.5 text-[0.6875rem] text-info">
          <Loader2 className="size-3 animate-spin" /> testing…
        </span>
      )}
      {state.kind === "done" && <TestVerdictChip result={state.result} />}
      <Button
        size="sm"
        variant="outline"
        className="shrink-0"
        disabled={!operator || running}
        title={!operator ? OPERATOR_ONLY_REASON : undefined}
        onClick={onTest}
      >
        {running ? "Testing…" : "Test"}
      </Button>
    </>
  );
}

type Loaded = { status: SetupStatus; siteConfig: SiteConfig; secretNames: string[]; data: IntegrationsData };

export function IntegrationsScreen({
  embedded = false,
  onChanged,
  hideCategories,
}: {
  /** Thin-embed mode (the Getting Started Integrations step): drops the
   *  PageHeader and the Integrations/Tools tab strip, keeping everything
   *  else — proxy banner, category sections with their full row actions,
   *  empty state, footnote, and the Add/rotate/delete dialogs. */
  embedded?: boolean;
  /** Called after a successful (re)load — lets an embedding parent (Getting
   *  Started) refresh its own copy of status/siteConfig/secrets so the rail's
   *  "Ready · N connected" badge doesn't go stale right after an in-embed add. */
  onChanged?: () => void;
  /** Category sections to omit entirely (their rows, their contribution to
   *  the empty-state total, and — for host_proxy — the proxy banner). Additive
   *  and opt-in: omitted (the default), every category renders exactly as
   *  before. Used by the Getting Started Integrations step now that Corporate
   *  network owns host_proxy/artifact_mirror (egress redirection) as its own
   *  step — see integrations-step.tsx's T.EMBED_SCOPE_NOTE. The full
   *  /integrations page never passes this: all four categories still appear
   *  there. */
  hideCategories?: IntegrationCategory[];
} = {}) {
  const operator = useOperator();
  const navigate = useNavigate();
  const [state, setState] = React.useState<"loading" | "error" | "ready">("loading");
  const [loaded, setLoaded] = React.useState<Loaded | null>(null);
  const [tab, setTab] = React.useState<"integrations" | "tools">("integrations");
  const [addOpen, setAddOpen] = React.useState(false);
  const [rotateName, setRotateName] = React.useState<string | null>(null);
  const [toDelete, setToDelete] = React.useState<IntegrationRow | null>(null);
  // Test-probe UI state — ephemeral, never persisted; reset on remount like
  // the Corporate network step's own (ProbeUiState is ONLY ever "idle" until
  // a click, so there's nothing to seed from siteConfig/status).
  const [proxyTest, setProxyTest] = React.useState<ProbeUiState>({ kind: "idle" });
  const [redirectTests, setRedirectTests] = React.useState<Record<string, ProbeUiState>>({});
  // Set after the FIRST load completes — onChanged fires only from here on, so
  // simply mounting the embed (Getting Started visiting the step) doesn't also
  // cascade into the caller's own recheck (a redundant second status/siteConfig
  // round trip on every visit); only a REAL in-embed mutation does.
  const loadedOnceRef = React.useRef(false);

  const load = React.useCallback(() => {
    setState("loading");
    Promise.all([setupApi.getSetupStatus(), health.getSiteConfig(), secretsApi.listSecrets()])
      .then(([status, siteConfig, secretNames]) => {
        setLoaded({ status, siteConfig, secretNames, data: deriveIntegrations(status, siteConfig, secretNames) });
        setState("ready");
        if (loadedOnceRef.current) onChanged?.();
        loadedOnceRef.current = true;
      })
      .catch(() => setState("error"));
    // `onChanged` must be a stable (memoized) callback from the caller — it's a
    // real dependency here (a fresh inline function every render would refire
    // this effect in a loop), not an omitted one.
  }, [onChanged]);
  React.useEffect(load, [load]);

  const wrapperClass = embedded ? undefined : "mx-auto max-w-[1400px] px-6 py-6";

  if (state === "loading" || !loaded) {
    return (
      <div className={wrapperClass}>
        {!embedded && <PageHeader title="Integrations" description={T.LEDE} />}
        <div className="overflow-hidden rounded-xl border border-border bg-card">
          <TableSkeleton rows={4} cols={4} />
        </div>
      </div>
    );
  }
  if (state === "error") {
    return (
      <div className={wrapperClass}>
        {!embedded && <PageHeader title="Integrations" description={T.LEDE} />}
        <div className="rounded-xl border border-border bg-card">
          <ErrorState onRetry={load} />
        </div>
      </div>
    );
  }

  const { status, siteConfig, data } = loaded;
  const hidden = new Set(hideCategories);
  const totalRows =
    (hidden.has("ai_provider") ? 0 : data.ai.length) +
    (hidden.has("scm_host") ? 0 : data.scm.length) +
    (hidden.has("artifact_mirror") ? 0 : data.mirror.length) +
    (hidden.has("host_proxy") ? 0 : data.proxy.length);
  const showBanner = !hidden.has("host_proxy") && proxyBannerNeeded(status, siteConfig);

  const requestDelete = (row: IntegrationRow) => setToDelete(row);
  const runDelete = async () => deleteIntegration(toDelete!, siteConfig);

  const runProxyTest = async () => {
    setProxyTest({ kind: "running" });
    try {
      setProxyTest({ kind: "done", result: await health.testProxy() });
    } catch (e) {
      // A request that never produced a probe result is NOT a probe verdict —
      // see corp-network-step.tsx's identical fix (c5f41cb). Synthesizing
      // "blocked" here would blame the corporate network for a 403, a
      // restarted wardynd, or a bad payload. Report the failure as itself and
      // stay untested so the operator can retry.
      toast.error("Could not run the proxy test", { description: getErrorMessage(e) });
      setProxyTest({ kind: "idle" });
    }
  };

  const runRedirectTest = async (row: IntegrationRow) => {
    if (!row.redirect) return;
    setRedirectTests((s) => ({ ...s, [row.id]: { kind: "running" } }));
    try {
      const result = await health.testRedirect(row.redirect.from, row.redirect.to);
      setRedirectTests((s) => ({ ...s, [row.id]: { kind: "done", result } }));
    } catch (e) {
      // See runProxyTest above.
      toast.error(`Could not test the ${row.redirect.from} redirect`, { description: getErrorMessage(e) });
      setRedirectTests((s) => ({ ...s, [row.id]: { kind: "idle" } }));
    }
  };

  // No confirm dialog — matches the Corporate network step's own × button on
  // the identical redirect list (a direct removal there too, not routed
  // through the blast-radius flow the other categories use).
  const removeRedirect = async (row: IntegrationRow) => {
    try {
      await deleteIntegration(row, siteConfig);
      load();
    } catch (e) {
      toast.error("Failed to remove the redirect", { description: getErrorMessage(e) });
    }
  };

  return (
    <div className={wrapperClass}>
      {!embedded && (
        <PageHeader
          title="Integrations"
          description={T.LEDE}
          actions={
            <>
              {!operator && <Chip tone="neutral">{OPERATOR_ONLY_REASON}</Chip>}
              <Button onClick={() => setAddOpen(true)} disabled={!operator}>
                <Plus className="size-4" /> Add integration
              </Button>
            </>
          }
        />
      )}

      {!operator && (
        <div className="mb-4 flex items-center gap-2">
          <Chip tone="neutral" dot>
            Viewer role
          </Chip>
          <p className="text-xs text-muted-foreground">{T.VIEWER_LINE}</p>
        </div>
      )}

      {!embedded && (
        <Tabs value={tab} onValueChange={(v) => setTab(v as typeof tab)} className="mb-5">
          <TabsList>
            <TabsTrigger value="integrations">Integrations</TabsTrigger>
            <TabsTrigger value="tools">Tools</TabsTrigger>
          </TabsList>
        </Tabs>
      )}

      {tab === "tools" ? (
        <ToolsTab data={data} />
      ) : (
        <div className="space-y-6">
          {showBanner && (
            <div className="flex items-start gap-2.5 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5">
              <Network className="mt-0.5 size-4 shrink-0 text-warning" />
              <p className="flex-1 text-[0.8125rem] leading-snug text-warning">{T.PROXY_BANNER}</p>
              <Button size="sm" variant="outline" onClick={() => setAddOpen(true)} disabled={!operator}>
                Add host proxy
              </Button>
            </div>
          )}

          {totalRows === 0 ? (
            <div className="rounded-xl border border-border">
              <EmptyState
                icon={Cable}
                title={T.EMPTY_TITLE}
                description={operator ? T.EMPTY_BODY : `${T.EMPTY_BODY} ${OPERATOR_ONLY_REASON}`}
                action={
                  <Button onClick={() => setAddOpen(true)} disabled={!operator}>
                    <Plus className="size-4" /> Add integration
                  </Button>
                }
              />
            </div>
          ) : (
            <>
              <CategorySection
                category="ai_provider"
                label="AI providers"
                rows={data.ai}
                operator={operator}
                onOpen={(r) => navigate(`/integrations/${r.id}`)}
                onRotate={setRotateName}
                onReCheck={load}
                onDelete={requestDelete}
              />
              <CategorySection
                category="scm_host"
                label="SCM hosts"
                rows={data.scm}
                operator={operator}
                onOpen={(r) => navigate(`/integrations/${r.id}`)}
                onRotate={setRotateName}
                onReCheck={load}
                onDelete={requestDelete}
              />
              {!hidden.has("artifact_mirror") && (
                <CategorySection
                  category="artifact_mirror"
                  label="Egress redirection"
                  rows={data.mirror}
                  operator={operator}
                  onOpen={(r) => navigate(`/integrations/${r.id}`)}
                  onRotate={setRotateName}
                  onReCheck={load}
                  onDelete={requestDelete}
                  redirectTest={(id) => redirectTests[id] ?? { kind: "idle" }}
                  onRedirectTest={runRedirectTest}
                  onRedirectRemove={removeRedirect}
                />
              )}
              {!hidden.has("host_proxy") && (
                <CategorySection
                  category="host_proxy"
                  label="Host proxy"
                  rows={data.proxy}
                  operator={operator}
                  onOpen={(r) => navigate(`/integrations/${r.id}`)}
                  onRotate={setRotateName}
                  onReCheck={load}
                  onDelete={requestDelete}
                  proxyTest={proxyTest}
                  onProxyTest={runProxyTest}
                />
              )}
            </>
          )}

          <p className="max-w-2xl text-[0.6875rem] leading-snug text-muted-foreground">{T.FOOTNOTE}</p>
        </div>
      )}

      <AddIntegrationDialog
        open={addOpen}
        onOpenChange={setAddOpen}
        status={status}
        siteConfig={siteConfig}
        existingAiRows={data.ai}
        reload={load}
      />

      <AddSecretDialog
        open={!!rotateName}
        onOpenChange={(o) => !o && setRotateName(null)}
        lockName
        initialName={rotateName ?? ""}
        existingNames={loaded.secretNames}
        onSaved={() => {
          setRotateName(null);
          load();
        }}
      />

      <DeleteConfirmDialog
        name={toDelete?.name ?? null}
        entity="integration"
        description={
          <ul className="space-y-1">
            {blastRadius(toDelete ?? ({} as IntegrationRow)).map((line, i) => (
              <li key={i}>{line}</li>
            ))}
          </ul>
        }
        onOpenChange={(o) => !o && setToDelete(null)}
        onDelete={runDelete}
        onDeleted={() => {
          setToDelete(null);
          load();
        }}
      />
    </div>
  );
}

function CategorySection({
  category,
  label,
  rows,
  operator,
  onOpen,
  onRotate,
  onReCheck,
  onDelete,
  proxyTest,
  onProxyTest,
  redirectTest,
  onRedirectTest,
  onRedirectRemove,
}: {
  category: IntegrationCategory;
  label: string;
  rows: IntegrationRow[];
  operator: boolean;
  onOpen: (row: IntegrationRow) => void;
  onRotate: (secretName: string) => void;
  onReCheck: () => void;
  onDelete: (row: IntegrationRow) => void;
  /** host_proxy only — the one Test control that isn't per-row. */
  proxyTest?: ProbeUiState;
  onProxyTest?: () => void;
  /** artifact_mirror rows sourced from egress_redirects (row.redirect set) only. */
  redirectTest?: (rowId: string) => ProbeUiState;
  onRedirectTest?: (row: IntegrationRow) => void;
  onRedirectRemove?: (row: IntegrationRow) => void;
}) {
  const Icon = CATEGORY_ICON[category];
  return (
    <section className="space-y-2" aria-label={label}>
      <div className="flex items-center gap-2">
        <Icon className="size-4 text-muted-foreground" />
        <SectionLabel>{label}</SectionLabel>
        <span className="text-xs text-muted-foreground">· {rows.length || "—"}</span>
      </div>
      {rows.length === 0 ? (
        <p className="pl-6 text-[0.8125rem] leading-snug text-muted-foreground">{CATEGORY_EMPTY[category]}</p>
      ) : (
        <div className="overflow-hidden rounded-xl border border-border bg-card">
          {rows.map((row, i) => {
            // Current-shape egress redirect: compact from/to + Test + a direct
            // remove, no kebab — the mock uses this SAME row shape (redirectRow)
            // for both the Corporate network step and this list.
            if (row.redirect) {
              return (
                <EgressRedirectRow
                  key={row.id}
                  redirect={row.redirect}
                  first={i === 0}
                  test={redirectTest?.(row.id) ?? { kind: "idle" }}
                  onTest={() => onRedirectTest?.(row)}
                  onRemove={() => onRedirectRemove?.(row)}
                  operator={operator}
                />
              );
            }
            if (category === "host_proxy") {
              return (
                <HostProxyRow
                  key={row.id}
                  row={row}
                  first={i === 0}
                  test={proxyTest ?? { kind: "idle" }}
                  onTest={() => onProxyTest?.()}
                  operator={operator}
                  onOpen={onOpen}
                  onRotate={onRotate}
                  onReCheck={onReCheck}
                  onDelete={onDelete}
                />
              );
            }
            return (
              <Row
                key={row.id}
                row={row}
                first={i === 0}
                operator={operator}
                onOpen={onOpen}
                onRotate={onRotate}
                onReCheck={onReCheck}
                onDelete={onDelete}
              />
            );
          })}
        </div>
      )}
    </section>
  );
}

// Shared by every row kind that offers a kebab (the generic AI/SCM row and the
// Host proxy row — egress redirects get a direct Test/remove instead, no
// kebab, matching the mock). Extracted so the Host proxy row's Open/Rotate/
// Delete behave identically to every other row's, not a hand-rolled twin.
function RowKebab({
  row,
  operator,
  onOpen,
  onRotate,
  onReCheck,
  onDelete,
}: {
  row: IntegrationRow;
  operator: boolean;
  onOpen: (row: IntegrationRow) => void;
  onRotate: (secretName: string) => void;
  onReCheck: () => void;
  onDelete: (row: IntegrationRow) => void;
}) {
  const rotateTarget = primarySecretName(row);
  // AI capabilities only — the mock never offers a default checkbox on an SCM/
  // mirror/proxy row (their kebabs pass no canAgent/canFeat at all); row.aiType
  // is undefined for a host_proxy row, so these fall out false for it with no
  // extra branching needed here.
  const canAgent = row.aiType && row.chips.some((c) => !c.muted && /Claude Code|Codex/.test(c.label));
  const canFeat = row.aiType && row.chips.some((c) => !c.muted && c.label.startsWith("Wardyn features"));
  const defAgent = row.aiType && row.chips.some((c) => !c.muted && /Claude Code|Codex/.test(c.label) && c.label.includes("· default"));
  const defFeat = row.aiType && row.chips.some((c) => !c.muted && c.label.startsWith("Wardyn features") && c.label.includes("· default"));

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="icon" className="size-8" aria-label={`${row.name} actions`}>
          <MoreHorizontal className="size-4" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuItem onClick={() => onOpen(row)}>Open</DropdownMenuItem>
        {(canRotateInline(row) || row.harnessProvider) && (
          <DropdownMenuItem
            disabled={!operator}
            onClick={() => (canRotateInline(row) && rotateTarget ? onRotate(rotateTarget) : onOpen(row))}
          >
            Rotate credential…
            {!operator && <OperatorOnlyHint />}
          </DropdownMenuItem>
        )}
        {canAgent && (
          <DropdownMenuCheckboxItem disabled={!operator} checked={!!defAgent} onCheckedChange={() => {}}>
            Set as default for agent runs
            {!operator && <OperatorOnlyHint />}
          </DropdownMenuCheckboxItem>
        )}
        {canFeat && (
          <DropdownMenuCheckboxItem disabled={!operator} checked={!!defFeat} onCheckedChange={() => {}}>
            Set as default for Wardyn features
            {!operator && <OperatorOnlyHint />}
          </DropdownMenuCheckboxItem>
        )}
        {row.canReCheck && (
          <DropdownMenuItem onClick={onReCheck}>
            <RotateCw className="size-3.5" /> Re-check
          </DropdownMenuItem>
        )}
        <DropdownMenuSeparator />
        <DropdownMenuItem className="text-danger focus:text-danger" disabled={!operator} onClick={() => onDelete(row)}>
          <Trash2 className="size-4" /> Delete integration…
          {!operator && <OperatorOnlyHint />}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function Row({
  row,
  first,
  operator,
  onOpen,
  onRotate,
  onReCheck,
  onDelete,
}: {
  row: IntegrationRow;
  first: boolean;
  operator: boolean;
  onOpen: (row: IntegrationRow) => void;
  onRotate: (secretName: string) => void;
  onReCheck: () => void;
  onDelete: (row: IntegrationRow) => void;
}) {
  const res = RESIDENCY_META[row.residency];
  return (
    <div
      className={`grid grid-cols-[190px_1fr_130px_160px_36px] items-center gap-3 px-3.5 py-3 ${first ? "" : "border-t border-border"}`}
    >
      <div className="flex flex-col gap-0.5">
        <span className="text-sm font-medium text-foreground">{row.name}</span>
        <Mono className="text-[0.6875rem]">{row.typeLabel}</Mono>
      </div>
      <div className="flex flex-wrap gap-1.5">
        {row.chips.map((c, i) => (
          <Chip
            key={`${c.label}-${i}`}
            tone={c.muted ? "neutral" : c.tone}
            className={c.muted ? "opacity-60 text-[0.6875rem]" : "text-[0.6875rem]"}
            title={c.tooltip}
          >
            {c.label}
          </Chip>
        ))}
      </div>
      <div>
        <Chip tone={res.tone} className="text-[0.6875rem]" title={res.tooltip}>
          {res.label}
        </Chip>
      </div>
      <div className="text-right">
        <Posture row={row} />
      </div>
      <div>
        <RowKebab row={row} operator={operator} onOpen={onOpen} onRotate={onRotate} onReCheck={onReCheck} onDelete={onDelete} />
      </div>
    </div>
  );
}

// Host proxy — a singleton row that now adopts the same compact "from → to"
// treatment as an egress redirect (mock's hostProxyListRow), plus a Test
// button, but keeps the standard kebab (Open/Rotate/Delete still apply to it
// like any other integration).
function HostProxyRow({
  row,
  first,
  test,
  onTest,
  operator,
  onOpen,
  onRotate,
  onReCheck,
  onDelete,
}: {
  row: IntegrationRow;
  first: boolean;
  test: ProbeUiState;
  onTest: () => void;
  operator: boolean;
  onOpen: (row: IntegrationRow) => void;
  onRotate: (secretName: string) => void;
  onReCheck: () => void;
  onDelete: (row: IntegrationRow) => void;
}) {
  // A plain upstream_proxy_url is real, readable config — show it compacted.
  // A secret-ref-backed proxy is write-only; there's no URL to show at all
  // (matches the Corporate network step's own "secret" render state).
  const target = row.proxyUrl
    ? { text: compactEndpoint(row.proxyUrl), full: row.proxyUrl }
    : row.secretNames[0]
      ? { text: `secret: ${row.secretNames[0]}`, full: "Write-only — the URL can't be shown here." }
      : { text: "—", full: "" };
  return (
    <div
      className={`flex items-center gap-2.5 px-3.5 py-3 ${first ? "" : "border-t border-border"}`}
      title={`wardyn-proxy chains through ${target.full || "an unconfigured upstream"}`}
    >
      <Mono className="shrink-0 text-xs text-foreground">wardyn-proxy</Mono>
      <span className="shrink-0 text-[0.6875rem] text-muted-foreground">&rarr;</span>
      <Mono className="min-w-0 flex-1 truncate text-xs text-foreground">{target.text}</Mono>
      {row.proxyUrl && (
        <Chip
          tone="neutral"
          className="shrink-0 text-[0.625rem]"
          title="The URL is readable configuration — topology, not a credential."
        >
          plain config
        </Chip>
      )}
      <TestControl state={test} onTest={onTest} operator={operator} />
      <RowKebab row={row} operator={operator} onOpen={onOpen} onRotate={onRotate} onReCheck={onReCheck} onDelete={onDelete} />
    </div>
  );
}

// Egress redirection — one row per SiteConfig.egress_redirects entry. `from`/
// `to` are compacted (compactEndpoint: scheme dropped, host never truncated,
// only the MIDDLE of a long path elided with "…"); the full values live in
// the row's own title, never just in a truncated span. No kebab: Test + a
// direct remove, matching the mock's redirectRow (the SAME row shape the
// Corporate network step uses for this exact list).
function EgressRedirectRow({
  redirect,
  first,
  test,
  onTest,
  onRemove,
  operator,
}: {
  redirect: EgressRedirect;
  first: boolean;
  test: ProbeUiState;
  onTest: () => void;
  onRemove: () => void;
  operator: boolean;
}) {
  return (
    <div
      className={`flex items-center gap-2.5 px-3.5 py-3 ${first ? "" : "border-t border-border"}`}
      title={`${redirect.from} → ${redirect.to}${redirect.token_secret_ref ? ` · token: ${redirect.token_secret_ref}` : ""}`}
    >
      <Mono className="shrink-0 text-xs text-foreground">{compactEndpoint(redirect.from)}</Mono>
      <span className="shrink-0 text-[0.6875rem] text-muted-foreground">&rarr;</span>
      <Mono className="min-w-0 flex-1 truncate text-xs text-foreground">{compactEndpoint(redirect.to)}</Mono>
      {!redirect.ecosystem && (
        <Chip
          tone="neutral"
          className="shrink-0 text-[0.625rem] opacity-70"
          title="Network only — egress is substituted and the token is injected, but no per-tool config file is generated (there's no config-file equivalent for this destination)."
        >
          network only
        </Chip>
      )}
      {redirect.token_secret_ref && (
        <Chip
          tone="neutral"
          mono
          className="shrink-0 text-[0.625rem]"
          title={`token: ${redirect.token_secret_ref} — injected proxy-side at fetch time; the sandbox never holds it`}
        >
          token
        </Chip>
      )}
      <TestControl state={test} onTest={onTest} operator={operator} />
      <button
        type="button"
        aria-label={`Remove ${redirect.from} redirect`}
        title={!operator ? OPERATOR_ONLY_REASON : "Remove"}
        disabled={!operator}
        className="inline-flex size-7 shrink-0 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-muted hover:text-foreground disabled:pointer-events-none disabled:opacity-40"
        onClick={onRemove}
      >
        &times;
      </button>
    </div>
  );
}
