/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Integrations — the list. Base-component model (B3): ONE shape, three
// DERIVED sections by kind (AI providers / Source control / Connections —
// the Category enum is gone, groupForKind is the whole derivation). Every row
// — generic or one of the seven closed kinds — is read straight off
// GET /api/v1/integrations; there is no second, richer client-side rerivation
// for AI/SCM any more. A row the server only DERIVED (source !== "stored")
// offers exactly one promotion: "Adopt to edit" — the default-for checkbox a
// prior wave silently adopted on toggle is gone (actions.ts's setDefaultFor).
import * as React from "react";
import { useNavigate } from "react-router-dom";
import { Cable, MoreHorizontal, Network, Plus, RotateCw, Sparkles, GitBranch, Cloud, Trash2 } from "lucide-react";
import { toast } from "sonner";
import {
  baseBlastRadius,
  baseSummary,
  genericIntegrations,
  genericIntegrationsApi,
  probeChip,
  proxyBannerNeeded,
  type GenericIntegrationRow,
} from "../../../lib/api/integrations";
import { setup as setupApi } from "../../../lib/api/setup";
import { health } from "../../../lib/api/health";
import { integrationsApi as legacyIntegrationsApi } from "../../../lib/api/integrations";
import { T } from "../../../lib/integrations";
import type { SetupStatus, SiteConfig } from "../../../lib/types";
import { getErrorMessage } from "../../../lib/format";
import { Button } from "../../ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "../../ui/dropdown-menu";
import { Mono } from "../../wardyn/code-block";
import { Chip, OperatorOnlyHint, SectionLabel } from "../../wardyn/primitives";
import { EmptyState, ErrorState, TableSkeleton } from "../../wardyn/states";
import { PageHeader } from "../../wardyn/page-header";
import { DeleteConfirmDialog } from "../../wardyn/delete-confirm-dialog";
import { OPERATOR_ONLY_REASON } from "../../wardyn/copy";
import { useOperator } from "../../wardyn/operator-context";
import { deleteWireRow, setDefaultFor, toggleDisabled } from "./actions";
import { AddIntegrationDialog } from "./add-integration-dialog";

type Section = "model" | "scm" | "other";
const SECTION_LABEL: Record<Section, string> = { model: "AI providers", scm: "Source control", other: "Connections" };
const SECTION_ICON: Record<Section, React.ElementType> = { model: Sparkles, scm: GitBranch, other: Cloud };

function sectionFor(row: GenericIntegrationRow): Section {
  return row.group.id === "model" ? "model" : row.group.id === "scm" ? "scm" : "other";
}

type Loaded = { status: SetupStatus; siteConfig: SiteConfig; rows: GenericIntegrationRow[] };

export function IntegrationsScreen({
  embedded = false,
  onChanged,
}: {
  /** Thin-embed mode (the Getting Started Integrations step): drops the
   *  PageHeader and the page-side pointer at Corporate network, keeping
   *  everything else — proxy banner, sections, empty state, footnote, dialogs. */
  embedded?: boolean;
  /** Called after a successful (re)load past the first one — lets an
   *  embedding parent (Getting Started) refresh its own status copy. */
  onChanged?: () => void;
} = {}) {
  const operator = useOperator();
  const navigate = useNavigate();
  const [state, setState] = React.useState<"loading" | "error" | "ready">("loading");
  const [loaded, setLoaded] = React.useState<Loaded | null>(null);
  const [addOpen, setAddOpen] = React.useState(false);
  const [toDelete, setToDelete] = React.useState<GenericIntegrationRow | null>(null);
  const loadedOnceRef = React.useRef(false);

  const load = React.useCallback(() => {
    setState("loading");
    Promise.all([genericIntegrationsApi.list(), setupApi.getSetupStatus(), health.getSiteConfig()])
      .then(([wireRows, status, siteConfig]) => {
        setLoaded({ status, siteConfig, rows: genericIntegrations(wireRows, { allKinds: true }) });
        setState("ready");
        if (loadedOnceRef.current) onChanged?.();
        loadedOnceRef.current = true;
      })
      .catch(() => setState("error"));
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

  const { status, siteConfig, rows } = loaded;
  const showBanner = proxyBannerNeeded(status, siteConfig);
  const sections: Section[] = ["model", "scm", "other"];
  const toDeleteDefAgent = !!toDelete?.wire.default_for?.includes("agent_runs");
  const toDeleteDefFeat = !!toDelete?.wire.default_for?.includes("wardyn_features");
  // A managed subscription / Bedrock-SSO row is DERIVED from a captured harness
  // login, not stored — a plain delete has no row to remove, so the list just
  // re-derives it. deleteWireRow already knows to disconnect the login instead
  // when handed the provider; resolve it here (only for a non-stored row whose
  // provider is actually captured, so the resident-host CLI lane and stored rows
  // still take the normal delete path).
  const harnessProviderForDelete = (row: GenericIntegrationRow): string | undefined => {
    if (row.wire.source === "stored") return undefined;
    const provider =
      row.wire.id === "anthropic_subscription:managed" ? "anthropic" : row.wire.kind === "bedrock" ? "aws" : undefined;
    return provider && status.harness?.some((h) => h.provider === provider && h.captured) ? provider : undefined;
  };
  const toDeleteHarnessProvider = toDelete ? harnessProviderForDelete(toDelete) : undefined;

  const adopt = async (row: GenericIntegrationRow) => {
    try {
      await legacyIntegrationsApi.adoptIntegration(row.wire.id);
      load();
    } catch (e) {
      toast.error("Couldn't adopt the integration", { description: getErrorMessage(e) });
    }
  };
  const test = async (row: GenericIntegrationRow) => {
    try {
      await genericIntegrationsApi.test(row.wire.id);
      toast.success(`Tested ${row.name}`);
      load();
    } catch (e) {
      toast.error("Test failed", { description: getErrorMessage(e) });
      load();
    }
  };
  const setDefault = async (row: GenericIntegrationRow, mark: "agent_runs" | "wardyn_features", on: boolean) => {
    try {
      await setDefaultFor(row.wire, mark, on);
      load();
    } catch (e) {
      toast.error("Couldn't update the default", { description: getErrorMessage(e) });
    }
  };
  const setDisabled = async (row: GenericIntegrationRow, disabled: boolean) => {
    try {
      await toggleDisabled(row.wire, disabled);
      load();
    } catch (e) {
      toast.error("Couldn't update the integration", { description: getErrorMessage(e) });
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

      <div className="space-y-6">
        {showBanner && (
          <div className="flex items-start gap-2.5 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5">
            <Network className="mt-0.5 size-4 shrink-0 text-warning" />
            <p className="min-w-0 flex-1 text-[0.8125rem] leading-snug text-warning">{T.PROXY_BANNER}</p>
            <Button size="sm" variant="outline" className="shrink-0" onClick={() => navigate("/setup?step=corp_network")}>
              Open Corporate network
            </Button>
          </div>
        )}

        {embedded && rows.length > 0 && (
          <div className="flex items-center justify-end gap-2">
            {!operator && <Chip tone="neutral">{OPERATOR_ONLY_REASON}</Chip>}
            <Button size="sm" onClick={() => setAddOpen(true)} disabled={!operator}>
              <Plus className="size-4" /> Add integration
            </Button>
          </div>
        )}

        {rows.length === 0 ? (
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
          sections.map((s) => {
            const sectionRows = rows.filter((r) => sectionFor(r) === s);
            return (
              <SectionBlock
                key={s}
                section={s}
                rows={sectionRows}
                operator={operator}
                onOpen={(r) => navigate(`/integrations/${encodeURIComponent(r.wire.id)}`)}
                onAdopt={adopt}
                onTest={test}
                onDelete={setToDelete}
                onSetDefault={setDefault}
                onSetDisabled={setDisabled}
              />
            );
          })
        )}

        {!embedded && <p className="max-w-2xl text-[0.6875rem] leading-snug text-muted-foreground">{T.CORP_POINTER}</p>}
        <p className="max-w-2xl text-[0.6875rem] leading-snug text-muted-foreground">{T.FOOTNOTE}</p>
      </div>

      <AddIntegrationDialog
        open={addOpen}
        onOpenChange={setAddOpen}
        existingRows={rows.map((r) => ({ id: r.wire.id, name: r.name, stored: r.wire.source === "stored" }))}
        onSaved={load}
      />

      <DeleteConfirmDialog
        name={toDelete?.name ?? null}
        entity="integration"
        description={
          <ul className="space-y-1">
            {(toDeleteHarnessProvider
              ? [
                  "Disconnects the captured login — every run relying on it loses model access until you log in again (Log in again on the integration, or the Getting Started subscription step).",
                  ...(toDelete ? baseBlastRadius(toDelete, { isDefaultAgent: toDeleteDefAgent, isDefaultFeatures: toDeleteDefFeat }) : []),
                ]
              : toDelete
                ? baseBlastRadius(toDelete, { isDefaultAgent: toDeleteDefAgent, isDefaultFeatures: toDeleteDefFeat })
                : []
            ).map((line, i) => (
              <li key={i}>{line}</li>
            ))}
          </ul>
        }
        onOpenChange={(o) => !o && setToDelete(null)}
        onDelete={async () => deleteWireRow(toDelete!.wire, toDeleteHarnessProvider)}
        onDeleted={() => {
          setToDelete(null);
          load();
        }}
      />
    </div>
  );
}

function SectionBlock({
  section,
  rows,
  operator,
  onOpen,
  onAdopt,
  onTest,
  onDelete,
  onSetDefault,
  onSetDisabled,
}: {
  section: Section;
  rows: GenericIntegrationRow[];
  operator: boolean;
  onOpen: (row: GenericIntegrationRow) => void;
  onAdopt: (row: GenericIntegrationRow) => void;
  onTest: (row: GenericIntegrationRow) => void;
  onDelete: (row: GenericIntegrationRow) => void;
  onSetDefault: (row: GenericIntegrationRow, mark: "agent_runs" | "wardyn_features", on: boolean) => void;
  onSetDisabled: (row: GenericIntegrationRow, disabled: boolean) => void;
}) {
  const Icon = SECTION_ICON[section];
  return (
    <section className="space-y-2" aria-label={SECTION_LABEL[section]}>
      <div className="flex items-center gap-2">
        <Icon className="size-4 text-muted-foreground" />
        <SectionLabel>{SECTION_LABEL[section]}</SectionLabel>
        <span className="text-xs text-muted-foreground">· {rows.length || "—"}</span>
      </div>
      {rows.length === 0 ? (
        <p className="pl-6 text-[0.8125rem] leading-snug text-muted-foreground">
          {section === "model" ? T.EMPTY_AI : section === "scm" ? T.EMPTY_SCM : T.EMPTY_OTHER}
        </p>
      ) : (
        <div className="overflow-hidden rounded-xl border border-border bg-card">
          {rows.map((row, i) => (
            <Row
              key={row.wire.id}
              row={row}
              first={i === 0}
              isAi={section === "model"}
              operator={operator}
              onOpen={onOpen}
              onAdopt={onAdopt}
              onTest={onTest}
              onDelete={onDelete}
              onSetDefault={onSetDefault}
              onSetDisabled={onSetDisabled}
            />
          ))}
        </div>
      )}
    </section>
  );
}

function Row({
  row,
  first,
  isAi,
  operator,
  onOpen,
  onAdopt,
  onTest,
  onDelete,
  onSetDefault,
  onSetDisabled,
}: {
  row: GenericIntegrationRow;
  first: boolean;
  isAi: boolean;
  operator: boolean;
  onOpen: (row: GenericIntegrationRow) => void;
  onAdopt: (row: GenericIntegrationRow) => void;
  onTest: (row: GenericIntegrationRow) => void;
  onDelete: (row: GenericIntegrationRow) => void;
  onSetDefault: (row: GenericIntegrationRow, mark: "agent_runs" | "wardyn_features", on: boolean) => void;
  onSetDisabled: (row: GenericIntegrationRow, disabled: boolean) => void;
}) {
  const stored = row.wire.source === "stored";
  const probe = probeChip(row.wire.probe_status);
  const defAgent = !!row.wire.default_for?.includes("agent_runs");
  const defFeat = !!row.wire.default_for?.includes("wardyn_features");
  return (
    <div
      className={`grid grid-cols-[1fr_130px_150px_170px_36px] items-center gap-3 px-3.5 py-3 ${first ? "" : "border-t border-border"}`}
    >
      <button type="button" onClick={() => onOpen(row)} className="min-w-0 text-left">
        <span className="block truncate text-sm font-medium text-foreground">{row.name}</span>
        <Mono className="text-[0.6875rem]">{row.wire.kind}</Mono>
        <span className="block truncate text-[0.6875rem] text-muted-foreground">{baseSummary(row.wire, row.delivery)}</span>
      </button>
      <div>{row.wire.disabled && <Chip tone="neutral">Off</Chip>}</div>
      <div>
        <Chip tone={probe.tone} className="text-[0.6875rem]" title={probe.detail}>
          {probe.label}
        </Chip>
      </div>
      <div className="text-right">
        {stored ? (
          <Chip tone="neutral" className="text-[0.6875rem]">stored</Chip>
        ) : (
          <Button size="sm" variant="outline" disabled={!operator} onClick={() => onAdopt(row)}>
            Adopt to edit
            {!operator && <OperatorOnlyHint />}
          </Button>
        )}
      </div>
      <div>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon" className="size-8" aria-label={`${row.name} actions`}>
              <MoreHorizontal className="size-4" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem onClick={() => onOpen(row)}>Open</DropdownMenuItem>
            {row.wire.probe && (
              <DropdownMenuItem onClick={() => onTest(row)}>
                <RotateCw className="size-3.5" /> Test
              </DropdownMenuItem>
            )}
            {isAi && stored && (
              <>
                <DropdownMenuItem disabled={!operator} onClick={() => onSetDefault(row, "agent_runs", !defAgent)}>
                  {defAgent ? "Unset" : "Set"} default for agent runs
                  {!operator && <OperatorOnlyHint />}
                </DropdownMenuItem>
                <DropdownMenuItem disabled={!operator} onClick={() => onSetDefault(row, "wardyn_features", !defFeat)}>
                  {defFeat ? "Unset" : "Set"} default for Wardyn features
                  {!operator && <OperatorOnlyHint />}
                </DropdownMenuItem>
              </>
            )}
            {stored && (
              <DropdownMenuItem disabled={!operator} onClick={() => onSetDisabled(row, !row.wire.disabled)}>
                {row.wire.disabled ? "Enable integration" : "Disable integration"}
                {!operator && <OperatorOnlyHint />}
              </DropdownMenuItem>
            )}
            <DropdownMenuSeparator />
            <DropdownMenuItem className="text-danger focus:text-danger" disabled={!operator} onClick={() => onDelete(row)}>
              <Trash2 className="size-4" /> Delete integration…
              {!operator && <OperatorOnlyHint />}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </div>
  );
}
