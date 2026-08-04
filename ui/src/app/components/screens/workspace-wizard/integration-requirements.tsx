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
import { CATALOG_COPY, DELIVERY_META } from "../../../lib/integration-catalog";
import type { SetupStatus } from "../../../lib/types";
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
}: {
  /** Absent while status is still loading — the section simply doesn't render. */
  status: SetupStatus | null;
  requirements: WorkspaceRequirementsMap;
  setLane: (key: string, level: RequirementLevel) => void;
  clear: (key: string) => void;
}) {
  const available = React.useMemo(() => (status ? genericIntegrations(status) : []), [status]);
  const named = namedIntegrationIds(requirements);

  // Nothing configured and nothing named: say nothing. An empty picker on a
  // system with no integrations is noise, and the Integrations page is where
  // that gap belongs.
  if (available.length === 0 && named.length === 0) return null;

  const byId = new Map(available.map((r) => [r.wire.id, r]));

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
        {/* A named integration that no longer exists still shows: the contract
            states an intent, and it opens nothing until the row is back. */}
        {named
          .filter((id) => !byId.has(id))
          .map((id) => (
            <div key={id} className="flex flex-wrap items-center gap-2 p-2.5">
              <div className="min-w-0">
                <Mono className="text-xs text-foreground">{id}</Mono>
                <p className="text-[0.6875rem] text-warning">Not configured — this opens nothing until it exists.</p>
              </div>
              <span className="ml-auto" />
              <button
                type="button"
                className="text-[0.6875rem] text-muted-foreground underline"
                onClick={() => clear(requirementKey("integration", id))}
              >
                Remove
              </button>
            </div>
          ))}
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
  const delivery = DELIVERY_META[row.delivery];
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
      <Chip tone={delivery.tone} title={delivery.line}>
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
          <button type="button" className="text-[0.6875rem] text-muted-foreground underline" onClick={onClear}>
            Remove
          </button>
        </>
      ) : (
        <button
          type="button"
          className="rounded-md border border-border px-2 py-1 text-[0.6875rem] text-foreground"
          onClick={() => onLane("required")}
        >
          Add to this workspace
        </button>
      )}
    </div>
  );
}
