/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Integration detail — base-component model (B3). One shape, one detail page:
// Secrets (w/ delivery + never-resident language), Egress, Config,
// Verification (w/ re-test), Used-by, delete-409 note. No capability table,
// no per-AI-type builder — that was the legacy screen's own richness; a
// generic kind is base-only, and the seven closed kinds differ only in
// prefill, not in which sections exist. Self-sufficient: re-reads
// GET /api/v1/integrations and looks up `id` in it, so a direct link/refresh
// needs no router state hand-off.
import * as React from "react";
import { useNavigate, useParams } from "react-router-dom";
import { Cable, MoreHorizontal, RotateCw, Trash2 } from "lucide-react";
import { toast } from "sonner";
import {
  baseBlastRadius,
  baseSummary,
  genericIntegrations,
  genericIntegrationsApi,
  integrationsApi,
  probeChip,
  SECRET_DELIVERY_NOTE,
  type GenericIntegrationRow,
} from "../../../lib/api/integrations";
import { RESIDENCY_META, T } from "../../../lib/integrations";
import { getErrorMessage, clockTime } from "../../../lib/format";
import { HttpError } from "../../../lib/api/core";
import { Button } from "../../ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "../../ui/dropdown-menu";
import { Mono } from "../../wardyn/code-block";
import { Chip, OperatorOnlyHint, SectionLabel } from "../../wardyn/primitives";
import { EmptyState, ErrorState, TableSkeleton } from "../../wardyn/states";
import { DeleteConfirmDialog } from "../../wardyn/delete-confirm-dialog";
import { useOperator } from "../../wardyn/operator-context";
import { deleteWireRow, setDefaultFor, toggleDisabled } from "./actions";

function Region({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <section className="space-y-2">
      <SectionLabel>{label}</SectionLabel>
      {children}
    </section>
  );
}

/** The delivery language for one secret row — the never-resident/resident/
 *  brokered claim, straight off the secret's own `delivery` field when it
 *  carries one, or the row's overall (bespoke-lane) delivery otherwise. */
function secretDeliveryNote(mode: string | undefined, rowDelivery: string): string {
  if (mode === "proxy_header") return SECRET_DELIVERY_NOTE.proxy_header;
  if (mode === "resident_file" || mode === "resident_env") return SECRET_DELIVERY_NOTE.resident;
  if (rowDelivery === "brokered_mint") return SECRET_DELIVERY_NOTE.brokered;
  if (rowDelivery === "resident_mount") return SECRET_DELIVERY_NOTE.resident;
  return "No delivery lane stated for this secret.";
}

export function IntegrationDetailScreen() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const operator = useOperator();
  const [state, setState] = React.useState<"loading" | "error" | "ready">("loading");
  const [rows, setRows] = React.useState<GenericIntegrationRow[] | null>(null);
  const [testing, setTesting] = React.useState(false);
  const [confirmDelete, setConfirmDelete] = React.useState(false);

  const load = React.useCallback(() => {
    setState("loading");
    genericIntegrationsApi
      .list()
      .then((wireRows) => {
        setRows(genericIntegrations(wireRows, { allKinds: true }));
        setState("ready");
      })
      .catch(() => setState("error"));
  }, []);
  React.useEffect(load, [load]);

  if (state === "loading" || !rows) {
    return (
      <div className="mx-auto max-w-[1100px] px-6 py-6">
        <TableSkeleton rows={3} cols={2} />
      </div>
    );
  }
  if (state === "error") {
    return (
      <div className="mx-auto max-w-[1100px] px-6 py-6">
        <ErrorState onRetry={load} />
      </div>
    );
  }

  const row = id ? rows.find((r) => r.wire.id === id) : undefined;

  if (!row) {
    return (
      <div className="mx-auto max-w-[1100px] px-6 py-6">
        <EmptyState
          icon={Cable}
          title="Integration not found"
          description="It may have been removed, or its credential deleted from Secrets."
          action={
            <Button variant="outline" onClick={() => navigate("/integrations")}>
              Back to Integrations
            </Button>
          }
        />
      </div>
    );
  }

  const { wire } = row;
  const stored = wire.source === "stored";
  const isAi = row.group.id === "model";
  const defAgent = !!wire.default_for?.includes("agent_runs");
  const defFeat = !!wire.default_for?.includes("wardyn_features");
  const resMeta = RESIDENCY_META[row.delivery];
  const probe = probeChip(wire.probe_status);

  const adopt = async () => {
    try {
      await integrationsApi.adoptIntegration(wire.id);
      load();
    } catch (e) {
      toast.error("Couldn't adopt the integration", { description: getErrorMessage(e) });
    }
  };
  // handleTestIntegration's response body IS the fresh probe_status — patch it
  // straight into the loaded row rather than a full reload for one field.
  const runTest = async () => {
    setTesting(true);
    try {
      const status = await genericIntegrationsApi.test(wire.id);
      setRows((prev) => prev?.map((r) => (r.wire.id === wire.id ? { ...r, wire: { ...r.wire, probe_status: status } } : r)) ?? prev);
    } catch (e) {
      toast.error("Test failed", { description: getErrorMessage(e) });
    } finally {
      setTesting(false);
    }
  };
  const toggleDefault = async (mark: "agent_runs" | "wardyn_features", on: boolean) => {
    try {
      await setDefaultFor(wire, mark, on);
      load();
    } catch (e) {
      toast.error("Couldn't update the default", { description: getErrorMessage(e) });
    }
  };
  const toggleOff = async () => {
    try {
      await toggleDisabled(wire, !wire.disabled);
      load();
    } catch (e) {
      toast.error("Couldn't update the integration", { description: getErrorMessage(e) });
    }
  };
  const runDelete = async () => {
    try {
      await deleteWireRow(wire);
    } catch (e) {
      // 409 = a real conflict the server raised (something still depends on this
      // row) — test the typed status, not a substring of the message (a host or
      // detail that merely contains "409" would false-positive).
      if (e instanceof HttpError && e.status === 409) {
        toast.error("Still in use", { description: "Something depends on this integration — see Used-by above." });
      }
      throw e;
    }
  };

  return (
    <div className="mx-auto max-w-[1100px] space-y-6 px-6 py-6">
      <button
        type="button"
        onClick={() => navigate("/integrations")}
        className="text-xs text-muted-foreground transition-colors hover:text-foreground"
      >
        ← Integrations
      </button>

      {!operator && (
        <div className="flex items-center gap-2">
          <Chip tone="neutral" dot>
            Viewer role
          </Chip>
          <p className="text-xs text-muted-foreground">{T.VIEWER_LINE}</p>
        </div>
      )}

      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold text-foreground">{row.name}</h1>
        <Mono className="text-sm">{wire.kind}</Mono>
        {wire.disabled && <Chip tone="neutral">Off</Chip>}
        <span className="flex-1" />
        {!stored ? (
          <Button size="sm" variant="outline" disabled={!operator} onClick={adopt}>
            Adopt to edit
            {!operator && <OperatorOnlyHint />}
          </Button>
        ) : (
          <>
            <Chip tone="neutral">stored</Chip>
            <Button size="sm" variant="outline" disabled={!operator} onClick={() => void toggleOff()}>
              {wire.disabled ? "Enable" : "Disable"}
              {!operator && <OperatorOnlyHint />}
            </Button>
          </>
        )}
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon" className="size-8" aria-label="Integration actions">
              <MoreHorizontal className="size-4" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem className="text-danger focus:text-danger" disabled={!operator} onClick={() => setConfirmDelete(true)}>
              <Trash2 className="size-4" /> Delete integration…
              {!operator && <OperatorOnlyHint />}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>

      <p className="text-[0.8125rem] leading-snug text-muted-foreground">{baseSummary(wire, row.delivery)}</p>

      <Region label="Secrets">
        {wire.secrets && wire.secrets.length > 0 ? (
          <div className="divide-y divide-border rounded-lg border border-border">
            {wire.secrets.map((s, i) => (
              <div key={i} className="space-y-1 p-3">
                <div className="flex flex-wrap items-center gap-2">
                  <Chip tone="neutral" mono>
                    {s.role}
                  </Chip>
                  <Mono className="text-foreground">{s.secret_name}</Mono>
                  {s.delivery?.header && <span className="text-[0.6875rem] text-muted-foreground">presented as {s.delivery.header}</span>}
                </div>
                <p className="text-[0.6875rem] leading-snug text-muted-foreground">{secretDeliveryNote(s.delivery?.mode, row.delivery)}</p>
              </div>
            ))}
          </div>
        ) : (
          <p className="text-sm text-muted-foreground">
            {wire.kind === "anthropic_subscription" ? "No stored credential — Wardyn detects or captures this automatically." : "No secret required."}
          </p>
        )}
        <p className="text-[0.6875rem] leading-snug text-muted-foreground">The store is write-only: a secret's value can't be read back.</p>
      </Region>

      <Region label="Egress">
        {wire.egress && wire.egress.length > 0 ? (
          <div className="flex flex-wrap gap-1.5">
            {wire.egress.map((h) => (
              <Mono key={h} className="rounded-md border border-border px-2 py-1 text-foreground">
                {h}
              </Mono>
            ))}
          </div>
        ) : (
          <p className="text-sm text-muted-foreground">No egress allowlist entries — this kind reaches its host through a bespoke lane, not the generic proxy allowlist.</p>
        )}
      </Region>

      {wire.config && Object.keys(wire.config).length > 0 && (
        <Region label="Config">
          <div className="divide-y divide-border rounded-lg border border-border">
            {Object.entries(wire.config).map(([k, v]) => (
              <div key={k} className="flex items-center justify-between gap-3 px-3 py-2 text-sm">
                <Mono className="text-muted-foreground">{k}</Mono>
                <span className="text-foreground">{String(v)}</span>
              </div>
            ))}
          </div>
        </Region>
      )}

      <Region label="Residency">
        <div className="flex items-start gap-2.5">
          <Chip tone={resMeta.tone}>{resMeta.label}</Chip>
          <p className="flex-1 text-xs leading-snug text-muted-foreground">{resMeta.tooltip}</p>
        </div>
      </Region>

      <Region label="Verification">
        <div className="flex items-center gap-2.5">
          <Chip tone={probe.tone}>{probe.label}</Chip>
          {wire.probe_status?.checked_at && (
            <span className="text-[0.6875rem] text-muted-foreground">checked {clockTime(wire.probe_status.checked_at)}</span>
          )}
          {wire.probe ? (
            <Button size="sm" variant="outline" onClick={() => void runTest()} disabled={testing}>
              <RotateCw className="size-3.5" /> {testing ? "Testing…" : "Re-test"}
            </Button>
          ) : (
            <span className="text-[0.6875rem] text-muted-foreground">No probe configured for this kind.</span>
          )}
        </div>
        {probe.detail && <p className="text-[0.6875rem] leading-snug text-muted-foreground">{probe.detail}</p>}
      </Region>

      <Region label="Used-by">
        {isAi && (defAgent || defFeat) ? (
          <ul className="space-y-1 pl-4 text-[0.8125rem] text-foreground">
            {defAgent && <li className="list-disc">Default for agent runs.</li>}
            {defFeat && <li className="list-disc">Default for Wardyn features.</li>}
          </ul>
        ) : (
          <p className="text-sm text-muted-foreground">Not currently the default for agent runs or Wardyn features.</p>
        )}
        {isAi && stored && (
          <div className="flex flex-wrap gap-2">
            <Button size="sm" variant="outline" disabled={!operator} onClick={() => toggleDefault("agent_runs", !defAgent)}>
              {defAgent ? "Unset" : "Set"} default for agent runs
              {!operator && <OperatorOnlyHint />}
            </Button>
            <Button size="sm" variant="outline" disabled={!operator} onClick={() => toggleDefault("wardyn_features", !defFeat)}>
              {defFeat ? "Unset" : "Set"} default for Wardyn features
              {!operator && <OperatorOnlyHint />}
            </Button>
          </div>
        )}
        <p className="text-[0.6875rem] leading-snug text-muted-foreground">
          A workspace requirement can also name this integration directly — that binding isn't surfaced here yet.
        </p>
      </Region>

      <Region label="Danger zone">
        <div className="space-y-2.5 rounded-xl border border-danger/40 p-4">
          <p className="text-sm font-medium text-foreground">Delete this integration</p>
          <ul className="space-y-1 pl-4">
            {baseBlastRadius(row, { isDefaultAgent: defAgent, isDefaultFeatures: defFeat }).map((line, i) => (
              <li key={i} className="list-disc text-[0.6875rem] leading-snug text-muted-foreground">
                {line}
              </li>
            ))}
          </ul>
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">
            A 409 here means something still depends on this integration — resolve that first, then retry.
          </p>
          <Button
            size="sm"
            variant="outline"
            className="border-danger/50 text-danger hover:bg-danger-subtle hover:text-danger"
            disabled={!operator}
            onClick={() => setConfirmDelete(true)}
          >
            <Trash2 className="size-3.5" /> Delete integration…
            {!operator && <OperatorOnlyHint />}
          </Button>
        </div>
      </Region>

      <DeleteConfirmDialog
        name={confirmDelete ? row.name : null}
        entity="integration"
        description={
          <ul className="space-y-1">
            {baseBlastRadius(row, { isDefaultAgent: defAgent, isDefaultFeatures: defFeat }).map((line, i) => (
              <li key={i}>{line}</li>
            ))}
          </ul>
        }
        onOpenChange={(o) => !o && setConfirmDelete(false)}
        onDelete={runDelete}
        onDeleted={() => navigate("/integrations")}
      />
    </div>
  );
}
