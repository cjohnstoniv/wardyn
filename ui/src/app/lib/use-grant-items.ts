/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { agentProviders } from "./api/agent-providers";
import { components } from "./api/components";
import { modelProviders } from "./api/model-providers";
import { policies } from "./api/policies";
import { providers as workspaceProviders } from "./api/providers";
import { setup } from "./api/setup";
import { workspaces } from "./api/workspaces";
import { MODEL_PROVIDERS } from "./model-providers-copy";
import type { CapabilityKind } from "./permissions-copy";
import { useOperator, useSecurityOperator } from "../components/wardyn/operator-context";

// The capability kinds whose values are things Wardyn already lists, so the
// Add grant form can offer them by name. The value is the id the resolver
// compares; the label is what the item's own page shows.
export const PICKABLE_KINDS: readonly CapabilityKind[] = ["workspace", "model_provider", "policy", "component", "agent", "workspace_provider"];

export type GrantItem = { value: string; label: string };

// A name used twice would be two identical choices; the id's head tells them apart.
function labelled(rows: { id: string; name: string }[]): GrantItem[] {
  const seen = new Map<string, number>();
  for (const r of rows) seen.set(r.name, (seen.get(r.name) ?? 0) + 1);
  return rows.map((r) => ({ value: r.id, label: (seen.get(r.name) ?? 0) > 1 ? `${r.name} (${r.id.slice(0, 8)})` : r.name }));
}

async function fetchItems(kind: CapabilityKind): Promise<GrantItem[]> {
  switch (kind) {
    case "workspace":
      return labelled(await workspaces.listWorkspaces());
    case "policy":
      return labelled(await policies.listPolicies());
    case "component":
      return labelled(await components.list());
    case "model_provider": {
      const { providers } = await modelProviders.getModelProviders();
      return labelled((providers.providers ?? []).map((p) => ({ id: p.id, name: p.name || MODEL_PROVIDERS.KIND[p.kind] || p.id })));
    }
    case "workspace_provider": {
      const { providers } = await workspaceProviders.getWorkspaceProviders();
      return labelled((providers.git ?? []).map((p) => ({ id: p.id, name: `${p.kind} ${p.base_urls?.[0] ?? ""}`.trim() })));
    }
    case "agent": {
      const [{ providers }, status] = await Promise.all([agentProviders.getAgentProviders(), setup.getSetupStatus().catch(() => null)]);
      return labelled((providers.agents ?? []).map((a) => ({ id: a.id, name: status?.harnesses?.find((h) => h.id === a.id)?.display || a.id })));
    }
    default:
      return [];
  }
}

// useGrantItems reads the list behind each asked kind, once. A kind maps to
// null until its list has loaded, and stays null when it can't be read or is
// empty: the caller keeps free text for it. Lists behind the admin-only
// routes (model providers, git providers, agents, components) are asked for
// only by a super admin, so a security admin's screen earns no refusal row.
export function useGrantItems(kinds: readonly CapabilityKind[]): Partial<Record<CapabilityKind, GrantItem[] | null>> {
  const operator = useOperator();
  const security = useSecurityOperator();
  const [items, setItems] = React.useState<Partial<Record<CapabilityKind, GrantItem[] | null>>>({});
  const wanted = [...new Set(kinds)].filter((k) => PICKABLE_KINDS.includes(k)).sort().join(",");
  React.useEffect(() => {
    if (!security) return;
    let live = true;
    for (const kind of wanted ? (wanted.split(",") as CapabilityKind[]) : []) {
      if (kind !== "workspace" && kind !== "policy" && !operator) continue;
      fetchItems(kind)
        .then((list) => live && setItems((prev) => ({ ...prev, [kind]: list.length > 0 ? list : null })))
        .catch(() => live && setItems((prev) => ({ ...prev, [kind]: null })));
    }
    return () => {
      live = false;
    };
  }, [wanted, operator, security]);
  return items;
}
