/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The eight GENERIC integration sections — package feeds, container
// registries, cloud providers, data stores, MCP servers, work tracking,
// observability, and the catch-all. Everything the Integrations page used to
// be missing when it was "AI providers and git hosts".
//
// A generic row has no client-side derivation behind it, unlike the two legacy
// categories: the server returns it, and it carries its own hosts, header and
// secret ref. That is the whole contract, which is what lets a system Wardyn
// has never heard of be added with no backend change.
import * as React from "react";
import { Plus, Trash2 } from "lucide-react";
import { genericSections, genericIntegrationsApi, proxyHeaderSecret, type GenericIntegrationRow } from "../../../lib/api/integrations";
import { CATALOG_COPY } from "../../../lib/integration-catalog";
import { RESIDENCY_META } from "../../../lib/integrations";
import { Button } from "../../ui/button";
import { Mono } from "../../wardyn/code-block";
import { Chip, SectionLabel } from "../../wardyn/primitives";
import { DeleteConfirmDialog } from "../../wardyn/delete-confirm-dialog";

/** Deleting an integration never deletes its secrets — the surface has to say so. */
export function genericBlastRadius(row: GenericIntegrationRow): string[] {
  const lines = [
    row.hosts.length > 0
      ? `Runs granted this integration stop reaching ${row.hosts.join(", ")}.`
      : "This integration opens no hosts, so nothing loses reach.",
    "Any workspace requirement naming it opens nothing until it is re-added.",
  ];
  const secret = proxyHeaderSecret(row.wire)?.secret_name;
  lines.push(
    secret
      ? `The stored secret ${secret} is not deleted — remove it under Secrets.`
      : "No credential is stored, so nothing leaves the secret store.",
  );
  return lines;
}

export function GenericSections({
  rows,
  operator,
  onAdd,
  onChanged,
}: {
  rows: GenericIntegrationRow[];
  operator: boolean;
  onAdd: () => void;
  onChanged: () => void;
}) {
  const [toDelete, setToDelete] = React.useState<GenericIntegrationRow | null>(null);
  const sections = genericSections(rows);

  return (
    <>
      {sections.map(({ group, rows: groupRows }) => (
        <section className="space-y-2" key={group.id} aria-label={group.label}>
          <div className="flex items-center gap-2">
            <SectionLabel>{group.label}</SectionLabel>
            <span className="text-xs text-muted-foreground">· {groupRows.length}</span>
          </div>
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">{group.desc}</p>
          <div className="overflow-hidden rounded-xl border border-border bg-card">
            {groupRows.map((row, i) => (
              <GenericRow
                key={row.wire.id}
                row={row}
                first={i === 0}
                operator={operator}
                onDelete={() => setToDelete(row)}
              />
            ))}
          </div>
        </section>
      ))}

      {sections.length === 0 && (
        <div className="rounded-xl border border-dashed border-border px-4 py-6 text-center">
          {/* Without its own label this panel reads as more SCM-host copy
              (it sits right under "SCM HOSTS · —") rather than the empty
              state for the eight generic categories. */}
          <SectionLabel>Services</SectionLabel>
          <p className="mt-2 text-[0.8125rem] leading-snug text-muted-foreground">{CATALOG_COPY.EMPTY_BODY}</p>
          <div className="mt-3">
            {/* Same verb as the page header's button — both open this exact
                AddServiceDialog; "Add a service" was a second name for it. */}
            <Button size="sm" variant="outline" onClick={onAdd} disabled={!operator}>
              <Plus className="size-4" /> Add integration
            </Button>
          </div>
        </div>
      )}

      <DeleteConfirmDialog
        name={toDelete?.name ?? null}
        entity="integration"
        description={
          <ul className="space-y-1">
            {(toDelete ? genericBlastRadius(toDelete) : []).map((line, i) => (
              <li key={i}>{line}</li>
            ))}
          </ul>
        }
        onOpenChange={(o) => !o && setToDelete(null)}
        onDelete={async () => {
          await genericIntegrationsApi.remove(toDelete!.wire.id);
        }}
        onDeleted={() => {
          setToDelete(null);
          onChanged();
        }}
      />
    </>
  );
}

function GenericRow({
  row,
  first,
  operator,
  onDelete,
}: {
  row: GenericIntegrationRow;
  first: boolean;
  operator: boolean;
  onDelete: () => void;
}) {
  const delivery = RESIDENCY_META[row.delivery];
  return (
    <div className={`flex items-start gap-3 px-4 py-3 ${first ? "" : "border-t border-border"}`}>
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-sm font-medium text-foreground">{row.name}</span>
          <Mono className="text-xs text-muted-foreground">{row.wire.kind}</Mono>
          {row.wire.disabled && <Chip tone="neutral">Off</Chip>}
        </div>
        {row.hosts.length > 0 && (
          <Mono className="mt-1 block text-xs text-muted-foreground">{row.hosts.join(" · ")}</Mono>
        )}
        <p className="mt-1 text-[0.6875rem] leading-snug text-muted-foreground">
          {row.meta?.powers ?? CATALOG_COPY.EGRESS_LINE}
        </p>
      </div>
      <div className="flex shrink-0 items-center gap-2">
        {/* Delivery is a stated fact, not a setting — the title carries the why. */}
        <Chip tone={delivery.tone} title={delivery.tooltip}>
          {delivery.label}
        </Chip>
        {operator && (
          <Button
            size="sm"
            variant="ghost"
            aria-label={`Delete ${row.name}`}
            onClick={onDelete}
            className="text-muted-foreground"
          >
            <Trash2 className="size-4" />
          </Button>
        )}
      </div>
    </div>
  );
}
