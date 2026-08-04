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
import { Cable, GitBranch, MoreHorizontal, Network, Plus, RotateCw, Sparkles, Trash2 } from "lucide-react";
import {
  deriveIntegrations,
  describePosture,
  proxyBannerNeeded,
  blastRadius,
  type IntegrationRow,
  type IntegrationsData,
} from "../../../lib/api/integrations";
import { setup as setupApi } from "../../../lib/api/setup";
import { health } from "../../../lib/api/health";
import { secrets as secretsApi } from "../../../lib/api/secrets";
import { T, type IntegrationCategory } from "../../../lib/integrations";
import type { SetupStatus, SiteConfig } from "../../../lib/types";
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
  const totalRows = data.ai.length + data.scm.length;
  const showBanner = proxyBannerNeeded(status, siteConfig);

  const requestDelete = (row: IntegrationRow) => setToDelete(row);
  const runDelete = async () => deleteIntegration(toDelete!);

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
}: {
  category: IntegrationCategory;
  label: string;
  rows: IntegrationRow[];
  operator: boolean;
  onOpen: (row: IntegrationRow) => void;
  onRotate: (secretName: string) => void;
  onReCheck: () => void;
  onDelete: (row: IntegrationRow) => void;
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
}: {
  row: IntegrationRow;
  operator: boolean;
  onOpen: (row: IntegrationRow) => void;
  onRotate: (secretName: string) => void;
  onReCheck: () => void;
  onDelete: (row: IntegrationRow) => void;
}) {
  const rotateTarget = primarySecretName(row);
  // AI capabilities only — the mock never offers a default checkbox on an SCM
  // row (its kebab passes no canAgent/canFeat at all); row.aiType is undefined
  // there, so these fall out false with no extra branching needed here.
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
