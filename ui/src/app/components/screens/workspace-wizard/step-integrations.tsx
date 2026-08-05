/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step ③ Integrations — what this workspace CONNECTS THROUGH, chosen before
// the Requirements step so the selection can shape it. AI arrives here: an
// API-key/token integration, a harness login, or a tool your base image
// already carries. Naming an integration writes an `integration:<id>` row
// into the workspace's contract (required = every run gets its wiring;
// optional = a run opts in) — one name, and the hosts and credential ride
// along instead of being restated on every workspace.
import * as React from "react";
import { integrationsApi, type IntegrationRow as AiScmRow } from "../../../lib/api/integrations";
import type { SetupStatus } from "../../../lib/types";
import { Mono } from "../../wardyn/code-block";
import { Button } from "../../ui/button";
import { IntegrationRequirements } from "./integration-requirements";
import { RequiredOptionalToggle } from "./step-requirements";
import { requirementKey, type RequirementLevel, type WorkspaceRequirementsMap } from "./wizard-types";

export const INTEGRATIONS_BLURB =
  "Pick what this workspace connects through — AI via an API key, token, or harness login (or a tool your base image already carries), plus git hosts and feeds. The selection joins your sources and base image in shaping the next step.";

function NamedRow({
  id,
  name,
  typeLabel,
  level,
  onLane,
  onClear,
}: {
  id: string;
  name: string;
  typeLabel?: string;
  level: RequirementLevel | undefined;
  onLane: (level: RequirementLevel) => void;
  onClear: () => void;
}) {
  return (
    <div className="flex flex-wrap items-center gap-2 p-2.5">
      <div className="min-w-0 flex-1">
        <p className="text-xs font-medium text-foreground">{name}</p>
        {typeLabel && <Mono className="text-[0.6875rem] text-muted-foreground">{typeLabel}</Mono>}
      </div>
      {level ? (
        <>
          <RequiredOptionalToggle idPrefix={`int-${id}`} label={`${name} requirement`} value={level} onChange={onLane} />
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

export function StepIntegrations({
  status,
  requirements,
  setLane,
  clear,
}: {
  status: SetupStatus | null;
  requirements: WorkspaceRequirementsMap;
  setLane: (key: string, level: RequirementLevel) => void;
  clear: (key: string) => void;
}) {
  // AI providers + SCM hosts — the two categories genericIntegrations
  // deliberately excludes, and exactly the ones an operator means first when
  // they say "integrations". Best-effort: a failed fetch leaves the generic
  // section (below) as the whole surface.
  const [ai, setAi] = React.useState<AiScmRow[]>([]);
  const [scm, setScm] = React.useState<AiScmRow[]>([]);
  React.useEffect(() => {
    let live = true;
    integrationsApi
      .list()
      .then((d) => {
        if (!live) return;
        setAi(d.ai);
        setScm(d.scm);
      })
      .catch(() => {});
    return () => {
      live = false;
    };
  }, []);

  const rowFor = (row: AiScmRow) => {
    const key = requirementKey("integration", row.id);
    return (
      <NamedRow
        key={row.id}
        id={row.id}
        name={row.name}
        typeLabel={row.typeLabel}
        level={requirements[key]?.level}
        onLane={(l) => setLane(key, l)}
        onClear={() => clear(key)}
      />
    );
  };

  return (
    <div className="space-y-4">
      {ai.length > 0 && (
        <section className="space-y-2" data-testid="ai-integrations">
          <p className="text-xs font-medium text-foreground">AI &amp; model access</p>
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">
            A named AI integration gives runs here its wiring — key or session injected proxy-side,
            never resident. A workspace with none still runs governed commands; a base image can also
            carry its own tool.
          </p>
          <div className="divide-y divide-border rounded-lg border border-border">{ai.map(rowFor)}</div>
        </section>
      )}
      {scm.length > 0 && (
        <section className="space-y-2" data-testid="scm-integrations">
          <p className="text-xs font-medium text-foreground">Source-control hosts</p>
          <div className="divide-y divide-border rounded-lg border border-border">{scm.map(rowFor)}</div>
        </section>
      )}
      {/* Package feeds, registries, data stores, MCP, … — the generic
          categories, same component that used to hide on the Reach tab. */}
      <IntegrationRequirements status={status} requirements={requirements} setLane={setLane} clear={clear} />
      {ai.length === 0 && scm.length === 0 && (
        <p className="text-[0.6875rem] leading-snug text-muted-foreground">
          Nothing connected yet? Integrations are configured once under Integrations in the sidebar,
          then selected here per workspace.
        </p>
      )}
    </div>
  );
}
