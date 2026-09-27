/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Display names for "What this type gets" rows: packet A draws a resource by
// its name, while a grant row carries its id. Each name comes from a list this
// caller can already read. A value no list names shows as written: a git
// provider row has no name, an integration's list is composed from super-admin
// reads, and a security admin cannot read the model provider roster
// (operatorOnly) — asking would only write an authz.denied row.
import * as React from "react";
import { modelProviders } from "../../../lib/api/model-providers";
import { policies } from "../../../lib/api/policies";
import { workspaces } from "../../../lib/api/workspaces";
import { EXPLAIN } from "../../../lib/user-types-copy";
import { useOperator } from "../../wardyn/operator-context";
import { agentLabel } from "../../wardyn/primitives";

type Names = Record<string, Record<string, string>>;

const FIXED: Names = {
  workspace: { "*": EXPLAIN.ALL_WORKSPACES },
  image: { "*": EXPLAIN.ALL_IMAGES },
  feature: { "*": EXPLAIN.SSH_AND_TOKENS, ssh_key: EXPLAIN.SSH_KEYS, api_token: EXPLAIN.API_TOKENS },
};

const byId = (rows: { id: string; name?: string }[]) =>
  Object.fromEntries(rows.filter((r) => r.name).map((r) => [r.id, r.name as string]));

// null = no name known; the caller shows the raw value in mono. Best-effort:
// a failed list read leaves its kind on raw values rather than failing the grid.
export function useValueNames(): (kind: string, value: string) => string | null {
  const operator = useOperator();
  const [names, setNames] = React.useState<Names>(FIXED);
  React.useEffect(() => {
    let live = true;
    const add = (kind: string) => (m: Record<string, string>) =>
      live && setNames((prev) => ({ ...prev, [kind]: { ...prev[kind], ...m } }));
    workspaces.listWorkspaces().then(byId).then(add("workspace")).catch(() => {});
    policies.listPolicies().then(byId).then(add("policy")).catch(() => {});
    if (operator) {
      modelProviders
        .getModelProviders()
        .then((l) => byId(l.providers.providers ?? []))
        .then(add("model_provider"))
        .catch(() => {});
    }
    return () => {
      live = false;
    };
  }, [operator]);
  return (kind, value) => names[kind]?.[value] ?? (kind === "agent" ? agentLabel(value) : null);
}
