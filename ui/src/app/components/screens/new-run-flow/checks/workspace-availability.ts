/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What a person can read off a workspace row about their own access: its
// composition, whether a source is refused, and whether the caller can use it.
// One home, so the Workspaces list, a workspace's detail page and the New Run
// pickers never diverge on what "unavailable" means.

import type { MeCapabilities, SetupModelProvider, Workspace, WorkspaceSourceInput } from "../../../../lib/types";
import { capabilityAllowed } from "../../../../lib/capabilities";

function workspaceSources(ws: Workspace): WorkspaceSourceInput[] {
  return ws.sources ?? [];
}

/** A multi-source workspace's composition ("2 dirs · 1 repo"); null for a single-source one, which keeps its kind label. */
export function compositionSummary(ws: Workspace): string | null {
  const sources = workspaceSources(ws);
  if (sources.length <= 1) return null;
  const counts = { local_dir: 0, repo: 0, ephemeral: 0 };
  for (const s of sources) if (s.type in counts) counts[s.type]++;
  const bits: string[] = [];
  if (counts.local_dir) bits.push(`${counts.local_dir} dir${counts.local_dir > 1 ? "s" : ""}`);
  if (counts.repo) bits.push(`${counts.repo} repo${counts.repo > 1 ? "s" : ""}`);
  if (counts.ephemeral) bits.push(`${counts.ephemeral} scratch dir${counts.ephemeral > 1 ? "s" : ""}`);
  return bits.join(" · ");
}

/** Whether any attached source is refused by the workspace-provider policy: `admitted === false` is the only value meaning refused, `undefined` says nothing. */
export function hasSourceNotAdmitted(ws: Workspace): boolean {
  return workspaceSources(ws).some((s) => s.admitted === false);
}

/**
 * Is this workspace pinned to a model provider the caller's own list lacks?
 * `modelProviders` is already narrowed to what this caller may use, so an
 * absent match is the caller's answer. `undefined` (not read yet, or no
 * provider block) fails open; a loaded empty list is "unavailable". The server
 * sends `provider_unavailable` in place of a pin the caller may not see.
 */
export function workspaceModelProviderUnavailable(
  ws: Workspace,
  modelProviders: { id: string }[] | undefined,
): boolean {
  if (!modelProviders) return false;
  if (ws.llm_cred?.provider_unavailable) return true;
  const ref = ws.llm_cred?.provider_ref;
  if (!ref) return false;
  return !modelProviders.some((p) => p.id === ref);
}

/**
 * SetupStatus.model_providers is `omitzero` on the wire: absent means no
 * provider block, a present `[]` means a block that grants this caller
 * nothing. An unreachable status folds to `undefined` too, since a failed read
 * is not a loaded answer.
 */
export function resolvedModelProviders(
  status: { unreachable?: boolean; model_providers?: SetupModelProvider[] } | null | undefined,
): SetupModelProvider[] | undefined {
  if (!status || status.unreachable) return undefined;
  return status.model_providers;
}

/**
 * Every person-side "isn't available to you" reason in one boolean.
 * `available_to_you` is the same decide path launch runs (the workspace's own
 * capability and the git provider its repo sources resolve to); an older
 * server without it falls back to the plain capability check. The
 * model-provider arm is local and agent-only, so it never flashes on for a
 * shell run.
 */
export function workspaceUnavailableToCaller(
  ws: Workspace,
  caps: MeCapabilities | null,
  modelProviders: { id: string }[] | undefined,
  isAgent: boolean,
): boolean {
  const workspaceOrProviderUnavailable =
    ws.available_to_you !== undefined ? !ws.available_to_you : !capabilityAllowed(caps, "workspace", ws.id);
  return workspaceOrProviderUnavailable || workspaceModelProviderUnavailable(ws, isAgent ? modelProviders : undefined);
}
