/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "This workspace needs Artifactory" — naming an integration in the
// requirements contract instead of restating its hosts and secret names. One
// name, and the hosts and the credential ride along.
//
// It lives on the Egress tab because that is where a workspace decides what it
// may REACH, and an integration is the tidiest way to say it: the alternative
// is hand-listing the same hosts on every workspace that needs the same feed,
// with a separately-named secret that nothing ties to them.
import * as React from "react";
import { genericIntegrations, type GenericIntegrationRow } from "../../../lib/api/integrations";
import { CATALOG_COPY } from "../../../lib/integration-catalog";
import { RESIDENCY_META } from "../../../lib/integrations";
import type { SetupStatus } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Chip } from "../../wardyn/primitives";
import { Mono } from "../../wardyn/code-block";
import { RequiredOptionalToggle } from "./step-requirements";
import { requirementKey, type RequirementLevel, type WorkspaceRequirementsMap } from "./wizard-types";

/** The integration ids a requirements map names, in map order. */
export function namedIntegrationIds(requirements: WorkspaceRequirementsMap): string[] {
  return Object.keys(requirements)
    .filter((k) => k.startsWith("integration:"))
    .map((k) => k.slice("integration:".length))
    .filter(Boolean);
}

export function IntegrationRequirements({
  status,
  requirements,
  setLane,
  clear,
  namedOnly = false,
}: {
  /** Absent while status is still loading — the section simply doesn't render. */
  status: SetupStatus | null;
  requirements: WorkspaceRequirementsMap;
  setLane: (key: string, level: RequirementLevel) => void;
  clear: (key: string) => void;
  /**
   * Suppress the picker and show only the rows the contract already names —
   * for a surface whose PICKING lives elsewhere (the wizard's step ③) but
   * which must still tell the truth about a named row's existence.
   */
  namedOnly?: boolean;
}) {
  const available = React.useMemo(
    () => (status && !namedOnly ? genericIntegrations(status.integrations ?? []) : []),
    [status, namedOnly],
  );
  const named = namedIntegrationIds(requirements);

  // Nothing configured and nothing named: say nothing. An empty picker on a
  // system with no integrations is noise, and the Integrations page is where
  // that gap belongs.
  if (available.length === 0 && named.length === 0) return null;

  const byId = new Map(available.map((r) => [r.wire.id, r]));
  // EXISTENCE is judged against every STORED integration, not the generic
  // subset above — an adopted AI/SCM row (e.g. anthropic_subscription:managed)
  // is picked on step ③, not here, but a contract naming it must not be told
  // "not configured" by the one surface that filtered it out of its picker.
  const storedById = new Map((status?.integrations ?? []).map((w) => [w.id, w]));

  return (
    <section className="space-y-2 rounded-lg border border-border p-3" data-testid="integration-requirements">
      <p className="text-xs font-medium text-foreground">Integrations</p>
      <p className="text-[0.6875rem] leading-snug text-muted-foreground">{CATALOG_COPY.WS_SEAM}</p>

      <div className="divide-y divide-border rounded-lg border border-border">
        {available.map((row) => (
          <IntegrationRow
            key={row.wire.id}
            row={row}
            level={requirements[requirementKey("integration", row.wire.id)]?.level}
            onLane={(l) => setLane(requirementKey("integration", row.wire.id), l)}
            onClear={() => clear(requirementKey("integration", row.wire.id))}
          />
        ))}
        {/* Named rows the generic picker doesn't carry: a stored AI/SCM row
            renders configured (it was picked on step ③); a truly-absent id
            keeps the honest warning — the contract states an intent, and it
            opens nothing until the row is back. */}
        {named
          .filter((id) => !byId.has(id))
          .map((id) => {
            const wire = storedById.get(id);
            const key = requirementKey("integration", id);
            const level = requirements[key]?.level;
            return (
              <div key={id} className="flex flex-wrap items-center gap-2 p-2.5">
                <div className="min-w-0">
                  {wire ? (
                    <>
                      <span className="text-xs text-foreground">{wire.name || id}</span>
                      <Mono className="ml-2 text-[0.6875rem] text-muted-foreground">{id}</Mono>
                      <p className="text-[0.6875rem] text-muted-foreground">Its wiring rides along.</p>
                    </>
                  ) : (
                    <>
                      <Mono className="text-xs text-foreground">{id}</Mono>
                      <p className="text-[0.6875rem] text-warning">Not configured — this opens nothing until it exists.</p>
                    </>
                  )}
                </div>
                <span className="ml-auto" />
                {wire && level && (
                  <RequiredOptionalToggle
                    idPrefix={`integration-${id}`}
                    label={`${wire.name || id} lane`}
                    value={level}
                    onChange={(l) => setLane(key, l)}
                  />
                )}
                <button
                  type="button"
                  className="text-[0.6875rem] text-muted-foreground underline"
                  onClick={() => clear(key)}
                >
                  Remove
                </button>
              </div>
            );
          })}
      </div>
    </section>
  );
}

function IntegrationRow({
  row,
  level,
  onLane,
  onClear,
}: {
  row: GenericIntegrationRow;
  level?: RequirementLevel;
  onLane: (level: RequirementLevel) => void;
  onClear: () => void;
}) {
  const delivery = RESIDENCY_META[row.delivery];
  return (
    <div className="flex flex-wrap items-center gap-2 p-2.5">
      <div className="min-w-0">
        <span className="text-xs text-foreground">{row.name}</span>
        {row.hosts.length > 0 && (
          <Mono className="ml-2 text-[0.6875rem] text-muted-foreground">{row.hosts.join(" · ")}</Mono>
        )}
        <p className="text-[0.6875rem] text-muted-foreground">
          {level ? "Its hosts and credential ride along." : "Not in this contract."}
        </p>
      </div>
      <span className="ml-auto" />
      <Chip tone={delivery.tone} title={delivery.tooltip}>
        {delivery.label}
      </Chip>
      {level ? (
        <>
          <RequiredOptionalToggle
            idPrefix={`integration-${row.wire.id}`}
            label={`${row.name} lane`}
            value={level}
            onChange={onLane}
          />
          {/* ui-wsWizard-3: same shared Button + label pair step-integrations.tsx's
              NamedRow uses for the identical add/remove-integration action —
              two button systems for one action, stacked in the same step. */}
          <Button type="button" size="sm" variant="ghost" className="h-7" onClick={onClear}>
            Not used
          </Button>
        </>
      ) : (
        <Button type="button" size="sm" variant="outline" className="h-7" onClick={() => onLane("required")}>
          Use in this workspace
        </Button>
      )}
    </div>
  );
}
