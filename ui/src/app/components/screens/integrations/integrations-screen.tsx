/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Integrations — the list. Two kinds of row live here, and the difference is
// worth knowing before editing:
//
//   - The two LEGACY categories (AI providers, SCM hosts) are DERIVED
//     client-side by lib/api/integrations.ts from the endpoints that predate the
//     entity, exactly the discipline lib/scm-provider.ts established for the SCM
//     Provider step. They carry lanes, posture and capability chips a generic
//     row has no notion of.
//   - The eight GENERIC categories are real entities the server returns on
//     SetupStatus.integrations, rendered by GenericSections. Such a row carries
//     its own hosts, header and secret ref — that is the whole contract, and it
//     is what lets a system Wardyn has never heard of be added with no backend
//     change.
import * as React from "react";
import { useNavigate } from "react-router-dom";
import { Cable, GitBranch, MoreHorizontal, Network, Plus, RotateCw, Sparkles, Trash2 } from "lucide-react";
import { toast } from "sonner";
import {
  deriveIntegrations,
  describePosture,
  genericIntegrations,
  proxyBannerNeeded,
  blastRadius,
  type IntegrationRow,
  type IntegrationsData,
} from "../../../lib/api/integrations";
import { setup as setupApi } from "../../../lib/api/setup";
import { health } from "../../../lib/api/health";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { T, type AiType, type IntegrationCategory } from "../../../lib/integrations";
import type { SetupStatus, SiteConfig, WireIntegration } from "../../../lib/types";
import { getErrorMessage } from "../../../lib/format";
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
import { canRotateInline, deleteIntegration, primarySecretName, setDefaultFor, type DefaultForMark } from "./actions";
import { AddIntegrationDialog, type AddIntegrationTarget } from "./add-integration-dialog";
import { AddServiceDialog } from "./add-service-dialog";
import { GenericSections } from "./generic-sections";
import { ToolsTab } from "./tools-tab";

const CATEGORY_ICON: Record<IntegrationCategory, React.ElementType> = {
  ai_provider: Sparkles,
  scm_host: GitBranch,
};

const CATEGORY_EMPTY: Record<IntegrationCategory, string> = {
  ai_provider: T.EMPTY_AI,
  scm_host: T.EMPTY_SCM,
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

type Loaded = { status: SetupStatus; siteConfig: SiteConfig; secretNames: string[]; data: IntegrationsData };

export function IntegrationsScreen({
  embedded = false,
  onChanged,
}: {
  /** Thin-embed mode (the Getting Started Integrations step): drops the
   *  PageHeader, the Integrations/Tools tab strip and the page-side pointer at
   *  Corporate network (the step renders its own, T.EMBED_SCOPE_NOTE), keeping
   *  everything else — proxy banner, category sections with their full row
   *  actions, empty state, footnote, and the Add/rotate/delete dialogs. */
  embedded?: boolean;
  /** Called after a successful (re)load — lets an embedding parent (Getting
   *  Started) refresh its own copy of status/siteConfig/secrets so the rail's
   *  "Ready · N connected" badge doesn't go stale right after an in-embed add. */
  onChanged?: () => void;
} = {}) {
  const operator = useOperator();
  const navigate = useNavigate();
  const [state, setState] = React.useState<"loading" | "error" | "ready">("loading");
  const [loaded, setLoaded] = React.useState<Loaded | null>(null);
  const [tab, setTab] = React.useState<"integrations" | "tools">("integrations");
  const [addOpen, setAddOpen] = React.useState(false);
  const [addServiceOpen, setAddServiceOpen] = React.useState(false);
  const [addTarget, setAddTarget] = React.useState<AddIntegrationTarget | undefined>();
  const [rotateName, setRotateName] = React.useState<string | null>(null);
  const [toDelete, setToDelete] = React.useState<IntegrationRow | null>(null);
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
  // The eight generic categories come straight from the server; the two legacy
  // ones are still derived above. Both count toward "connected anything".
  const generic = genericIntegrations(status);
  const totalRows = data.ai.length + data.scm.length + generic.length;
  const showBanner = proxyBannerNeeded(status, siteConfig);
  // The EFFECTIVE wire row behind a derived IntegrationRow (by its serverId) —
  // where a real default_for lives. Looked up once per render, not per row.
  const wireById = new Map((status.integrations ?? []).map((w) => [w.id, w]));
  // UI-WS-11: the row pending delete's own default-holder opts, the same ones
  // integration-detail.tsx passes — so the list's confirm doesn't drop the two
  // most consequential lines (agent runs lose model access, Composer loses its
  // backend) just because it deletes from a different surface.
  const toDeleteWire = toDelete?.serverId ? wireById.get(toDelete.serverId) : undefined;
  const toDeleteDefAgent = !!toDeleteWire?.default_for?.includes("agent_runs");
  const toDeleteDefFeat = !!toDeleteWire?.default_for?.includes("wardyn_features");

  const requestDelete = (row: IntegrationRow) => setToDelete(row);
  const runDelete = async () => deleteIntegration(toDelete!);
  const setDefault = async (row: IntegrationRow, mark: DefaultForMark, on: boolean) => {
    const wire = row.serverId ? wireById.get(row.serverId) : undefined;
    if (!wire) return;
    try {
      await setDefaultFor(wire, mark, on);
      load();
    } catch (e) {
      toast.error("Couldn't update the default", { description: getErrorMessage(e) });
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
              <Button onClick={() => setAddServiceOpen(true)} disabled={!operator}>
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
        <ToolsTab data={data} redirects={siteConfig.egress_redirects ?? []} />
      ) : (
        <div className="space-y-6">
          {/* Detection only — there is no "add it here" button any more,
              because there is no host_proxy category here to add it to. The
              sentence names the one place that configures a proxy. */}
          {showBanner && (
            <div className="flex items-start gap-2.5 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5">
              <Network className="mt-0.5 size-4 shrink-0 text-warning" />
              <p className="min-w-0 flex-1 text-[0.8125rem] leading-snug text-warning">{T.PROXY_BANNER}</p>
              {/* The banner names one place, so it takes you there — the old
                  "or add the Host proxy integration here" alternative went with
                  the category it offered. */}
              <Button size="sm" variant="outline" className="shrink-0" onClick={() => navigate("/setup?step=corp_network")}>
                Open Corporate network
              </Button>
            </div>
          )}

          {/* UX-2: embedded mode has no PageHeader (its Add button included)
              and, once any row exists, neither the EmptyState's own button
              (totalRows!==0) nor GenericSections' dashed panel one
              (sections.length!==0) — all three conditions can be
              simultaneously false, leaving no Add affordance anywhere on the
              step and no in-app link to /integrations either (the nav is
              gated). This is the one case the other three don't cover. */}
          {embedded && totalRows > 0 && (
            <div className="flex items-center justify-end gap-2">
              {!operator && <Chip tone="neutral">{OPERATOR_ONLY_REASON}</Chip>}
              <Button size="sm" onClick={() => setAddServiceOpen(true)} disabled={!operator}>
                <Plus className="size-4" /> Add integration
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
                  <Button onClick={() => setAddServiceOpen(true)} disabled={!operator}>
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
                wireById={wireById}
                onSetDefault={setDefault}
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
                wireById={wireById}
                onSetDefault={setDefault}
              />
              <GenericSections rows={generic} operator={operator} onAdd={() => setAddServiceOpen(true)} onChanged={load} />
            </>
          )}

          {/* Where the Host proxy / Egress redirection sections used to be.
              Skipped in the embed — the Getting Started step renders its own
              (backwards-pointing) half of the same pointer. */}
          {!embedded && (
            <p className="max-w-2xl text-[0.6875rem] leading-snug text-muted-foreground">{T.CORP_POINTER}</p>
          )}
          <p className="max-w-2xl text-[0.6875rem] leading-snug text-muted-foreground">{T.FOOTNOTE}</p>
        </div>
      )}

      {/* One Add button, routed by what you pick: a model provider or a git host
          hands off to the established AI/SCM dialog below, everything else is
          written straight through PUT /integrations. */}
      <AddServiceDialog
        open={addServiceOpen}
        onOpenChange={setAddServiceOpen}
        onAdded={load}
        existingRows={(status.integrations ?? []).map((w) => ({ id: w.id, name: w.name || w.id }))}
        onHandoff={(t) => {
          // The rule: never re-ask an answered question, never skip a real one.
          // A pick whose type still has an open sub-choice lands on the AI type
          // panel PRESELECTED — "Anthropic" still splits into API key vs Claude
          // subscription, a subscription still chooses managed vs host login,
          // Bedrock still has four credential lanes. A pick with nothing left
          // to ask (an OpenAI key is an OpenAI key) lands straight on connect.
          const aiType = t.addLane === "ai" ? (t.apiType as AiType | undefined) : undefined;
          const OPEN_QUESTION: Partial<Record<AiType, true>> = {
            anthropic_api_key: true, // the catalog's "Anthropic" is key OR subscription
            anthropic_subscription: true, // managed vs host login
            bedrock: true, // four credential lanes
          };
          setAddTarget(
            t.addLane === "scm"
              ? { s: "scm" }
              : aiType
                ? OPEN_QUESTION[aiType]
                  ? { s: "ai_type", preselect: aiType }
                  : { s: "ai_connect", type: aiType }
                : { s: "ai_type" },
          );
          setAddOpen(true);
        }}
      />

      {/* Mounted only once a pick handed off a target — there is no
          target-less way in any more; the category grid is gone. */}
      {addTarget && (
        <AddIntegrationDialog
          target={addTarget}
          open={addOpen}
          onOpenChange={setAddOpen}
          status={status}
          siteConfig={siteConfig}
          existingAiRows={data.ai}
          secretNames={loaded.secretNames}
          reload={load}
          onBackToSearch={() => {
            setAddOpen(false);
            setAddServiceOpen(true);
          }}
        />
      )}

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
            {blastRadius(toDelete ?? ({} as IntegrationRow), {
              isDefaultAgent: toDeleteDefAgent,
              isDefaultFeatures: toDeleteDefFeat,
            }).map((line, i) => (
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
  wireById,
  onSetDefault,
}: {
  category: IntegrationCategory;
  label: string;
  rows: IntegrationRow[];
  operator: boolean;
  onOpen: (row: IntegrationRow) => void;
  onRotate: (secretName: string) => void;
  onReCheck: () => void;
  onDelete: (row: IntegrationRow) => void;
  wireById: Map<string, WireIntegration>;
  onSetDefault: (row: IntegrationRow, mark: DefaultForMark, on: boolean) => void;
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
          {rows.map((row, i) => (
            <Row
              key={row.id}
              row={row}
              first={i === 0}
              operator={operator}
              onOpen={onOpen}
              onRotate={onRotate}
              onReCheck={onReCheck}
              onDelete={onDelete}
              wire={row.serverId ? wireById.get(row.serverId) : undefined}
              onSetDefault={onSetDefault}
            />
          ))}
        </div>
      )}
    </section>
  );
}

// The row kebab (Open / Rotate / defaults / Re-check / Delete), kept apart from
// Row's layout so the two read separately — Row is already a five-column grid.
function RowKebab({
  row,
  operator,
  onOpen,
  onRotate,
  onReCheck,
  onDelete,
  wire,
  onSetDefault,
}: {
  row: IntegrationRow;
  operator: boolean;
  onOpen: (row: IntegrationRow) => void;
  onRotate: (secretName: string) => void;
  onReCheck: () => void;
  onDelete: (row: IntegrationRow) => void;
  /** The row's effective wire row (status.integrations, by serverId) — where
   *  the REAL default_for lives. Absent when the row has no server identity
   *  yet (e.g. an Azure OpenAI row before a composer backend names one). */
  wire?: WireIntegration;
  onSetDefault: (row: IntegrationRow, mark: DefaultForMark, on: boolean) => void;
}) {
  const rotateTarget = primarySecretName(row);
  // AI capabilities only — the mock never offers a default checkbox on an SCM
  // row (its kebab passes no canAgent/canFeat at all); row.aiType is undefined
  // there, so these fall out false with no extra branching needed here. Also
  // requires a server identity to write to (`wire`) — nothing to adopt/PUT
  // without one.
  const canAgent = !!(row.aiType && wire && row.chips.some((c) => !c.muted && /Claude Code|Codex/.test(c.label)));
  const canFeat = !!(row.aiType && wire && row.chips.some((c) => !c.muted && c.label.startsWith("Wardyn features")));
  // The REAL persisted mark, not a guess — reflects whatever the last write
  // (from here, or the detail page) actually landed.
  const defAgent = !!wire?.default_for?.includes("agent_runs");
  const defFeat = !!wire?.default_for?.includes("wardyn_features");

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
          <DropdownMenuCheckboxItem
            disabled={!operator}
            checked={defAgent}
            onCheckedChange={(v) => onSetDefault(row, "agent_runs", v)}
          >
            Set as default for agent runs
            {!operator && <OperatorOnlyHint />}
          </DropdownMenuCheckboxItem>
        )}
        {canFeat && (
          <DropdownMenuCheckboxItem
            disabled={!operator}
            checked={defFeat}
            onCheckedChange={(v) => onSetDefault(row, "wardyn_features", v)}
          >
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
  wire,
  onSetDefault,
}: {
  row: IntegrationRow;
  first: boolean;
  operator: boolean;
  onOpen: (row: IntegrationRow) => void;
  onRotate: (secretName: string) => void;
  onReCheck: () => void;
  onDelete: (row: IntegrationRow) => void;
  wire?: WireIntegration;
  onSetDefault: (row: IntegrationRow, mark: DefaultForMark, on: boolean) => void;
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
      <div className="flex min-w-0 flex-col gap-1">
        <div className="flex flex-wrap gap-1.5">
          {row.chips.map((c, i) => (
            <Chip
              key={`${c.label}-${i}`}
              // Muted already reads as de-emphasized via the neutral tone
              // (grey vs. the colored live-capability chips) — no opacity
              // fade on top of it: that measured 3.16:1, under WCAG AA's
              // 4.5:1, because fading BOTH the chip's bg and its text toward
              // the page background collapses the contrast between them. The
              // label itself now carries the state too ("· n/a" / "· off"),
              // so it isn't color/opacity alone doing the telling.
              tone={c.muted ? "neutral" : c.tone}
              className="text-[0.6875rem]"
              title={c.tooltip}
            >
              {c.label}
            </Chip>
          ))}
        </div>
        {/* The reason a muted chip is muted, as always-visible text — not
            only a `title`, which a non-focusable span never surfaces to a
            keyboard user and no screen reader announces on its own. */}
        {row.chips
          .filter((c) => c.muted && c.tooltip)
          .map((c, i) => (
            <p key={i} className="text-[0.625rem] leading-snug text-muted-foreground">
              {c.tooltip}
            </p>
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
        <RowKebab
          row={row}
          operator={operator}
          onOpen={onOpen}
          onRotate={onRotate}
          onReCheck={onReCheck}
          onDelete={onDelete}
          wire={wire}
          onSetDefault={onSetDefault}
        />
      </div>
    </div>
  );
}
