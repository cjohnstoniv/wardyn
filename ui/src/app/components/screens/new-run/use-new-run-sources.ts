/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { getAuthGeneration, onAuthChange } from "../../../lib/api/core";
import { setup } from "../../../lib/api/setup";
import { policies } from "../../../lib/api/policies";
import { health, type PolicyRef } from "../../../lib/api/health";
import { runs } from "../../../lib/api/runs";
import type { RunPolicySpec, SetupStatus } from "../../../lib/types";
import { useWorkspaceList } from "../../../lib/use-workspace-list";
import { useDefaultPolicy } from "./use-default-policy";
import type { DraftIdentity } from "./use-run-checks";

interface Sources {
  setup?: SetupStatus;
  savedPolicies: { id: string; name: string; spec: RunPolicySpec }[];
  policiesLoaded: boolean;
  knownTitles: string[];
  governanceContact?: PolicyRef;
}
const EMPTY: Sources = { savedPolicies: [], policiesLoaded: false, knownTitles: [] };

/** Reads are scoped to the observed auth generation; advisory POSTs additionally require confirmed identity. */
export function useNewRunSources(identity: DraftIdentity) {
  const [authGeneration, setAuthGeneration] = React.useState(getAuthGeneration);
  const [attempt, setAttempt] = React.useState(0);
  React.useEffect(() => onAuthChange(() => setAuthGeneration(getAuthGeneration())), []);
  const revision = JSON.stringify([identity, authGeneration, attempt]);
  const allowed = identity.authGeneration === authGeneration;
  const [loaded, setLoaded] = React.useState<{ revision: string; sources: Sources; pending: boolean }>();
  const workspaceList = useWorkspaceList(revision);
  const defaultRead = useDefaultPolicy(revision, allowed);
  const { reload } = workspaceList;
  React.useEffect(() => {
    if (!allowed) return;
    let alive = true;
    const generation = getAuthGeneration();
    const update = (patch: Partial<Sources>, pending = true) => {
      if (!alive || generation !== getAuthGeneration()) return;
      setLoaded((old) => ({ revision, sources: { ...(old?.revision === revision ? old.sources : EMPTY), ...patch }, pending }));
    };
    void Promise.allSettled([
      setup.getSetupStatus().then((status) => update({ setup: status })),
      policies.listPolicies().then((saved) => update({ savedPolicies: saved.map(({ id, name, spec }) => ({ id, name, spec })), policiesLoaded: true })),
      health.whoami().then((me) => update({ governanceContact: me?.governance_contact ?? undefined })),
      runs.listRuns().then((titles) => update({ knownTitles: [...new Set(titles.map((run) => (run.title ?? "").trim()).filter(Boolean))].sort() })),
      reload(true),
    ]).then(() => update({}, false));
    return () => { alive = false; };
  }, [revision, allowed, reload]);
  const refresh = React.useCallback(() => setAttempt((n) => n + 1), []);
  const sources = allowed && loaded?.revision === revision ? loaded.sources : EMPTY;
  return {
    ...sources,
    workspaces: workspaceList.workspaces,
    defaultRead,
    revision,
    pending: !allowed || loaded?.revision !== revision || loaded.pending || defaultRead.status === "loading" || workspaceList.loading,
    refresh,
  };
}
