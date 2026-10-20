/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The effective policy card's rows: what the sandbox can actually do, and
// which of it the person changed from where the draft started.

import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { NR_RUN } from "../../../wardyn/copy/new-run/run";
import { NR_TOOLS } from "../../../wardyn/copy/new-run/tools";
import { NR_SHARED } from "../../../wardyn/copy/new-run/shared";
import type { PolicyPreviewResult } from "../../../../lib/types/policy-preview";
import type { ConfinementClass } from "../../../../lib/types/runs";
import { sameValue } from "../draft/draft-reducer";
import { innerWallsText } from "./format";
import type { NewRunDraft, RunIntent } from "../draft/run-intent";
import { changedFromOrigin, draftSource, intentAt, originSource, serverSource } from "./provenance";
import type { ReviewPreflight } from "./review-seams";

export type PolicyRowId = "barrier" | "network" | "credentials" | "files" | "tools";

export interface PolicyRow {
  id: PolicyRowId;
  label: string;
  value?: string;
  barrier?: ConfinementClass;
  detail?: string;
}

/** The tool default's effect in the canon's words: Hold and Run automatically as Review has them, Block it as the tool rules do. */
const EFFECT_VALUE: Record<string, string> = { allow: NR_REVIEW.POLICY_AUTOMATIC, hold: NR_REVIEW.POLICY_HOLD, deny: NR_TOOLS.EFFECT_DENY };

/** The server's source id for an interactive Claude Code default whose settings accept file edits unasked (run_access_facts.go). */
const ACCEPT_EDITS_SOURCE = "accept_edits";

const joined = (...parts: (string | undefined)[]): string | undefined =>
  parts.filter((p): p is string => !!p).reduce<string | undefined>((all, p) => (all ? NR_SHARED.JOIN(all, p) : p), undefined);

/** The draft keys each row stands for. */
const ROW_KEYS: Record<PolicyRowId, string[]> = {
  barrier: ["confinement_class", "runner_pool_id"],
  network: ["overrides.hosts", "inline_policy.allowed_domains", "inline_policy.denied_domains", "inline_policy.allow_all_egress", "inline_policy.first_use_approval"],
  credentials: ["model_provider", "integration_id", "overrides.agent.add_secrets", "overrides.agent.remove_secrets", "overrides.git_pat", "overrides.azure_devops"],
  files: ["workspaces", "drive", "start_folder", "no_repositories_or_drives", "keep_work_folder", "resources.disk_mib"],
  tools: ["tool_approvals", "overrides.agent.tool_rules", "overrides.push_rules", "inline_policy.tool_rules", "inline_policy.push_rules"],
};

/** The rows whose keys differ from what the origin loaded. Per-tool provider and rules count too. */
export function changedRows(intent: RunIntent, base: RunIntent): Set<PolicyRowId> {
  const perTool = (i: RunIntent, key: "model_provider" | "tool_rules" | "default_effect") => (i.tools ?? []).map((t) => t[key]);
  const changed = new Set<PolicyRowId>();
  for (const [id, keys] of Object.entries(ROW_KEYS) as [PolicyRowId, string[]][]) {
    if (keys.some((k) => !sameValue(intentAt(intent, k), intentAt(base, k)))) changed.add(id);
  }
  if (!sameValue(perTool(intent, "model_provider"), perTool(base, "model_provider"))) changed.add("credentials");
  if (!sameValue(perTool(intent, "tool_rules"), perTool(base, "tool_rules")) || !sameValue(perTool(intent, "default_effect"), perTool(base, "default_effect"))) changed.add("tools");
  return changed;
}

const FILE_KEYS = ROW_KEYS.files;

export function policyRows(
  draft: NewRunDraft,
  pre: ReviewPreflight | null,
  preview: PolicyPreviewResult | null,
  /** A harness's display name; its id until the roster answers. */
  harnessLabel: (id: string) => string = (id) => id,
): PolicyRow[] {
  const { intent } = draft;
  const ctx = { draft, pre, preview };
  const access = pre?.access ?? preview?.access;
  const rows: PolicyRow[] = [];

  const barrier = pre?.enforced_confinement_class ?? intent.confinement_class;
  if (barrier) {
    const fromList = pre?.barrier_options?.find((o) => o.default);
    // A class the server raised past the person's pick is no longer the pick: it says nothing about who chose it.
    const raised = !!intent.confinement_class && !!pre?.enforced_confinement_class && pre.enforced_confinement_class !== intent.confinement_class;
    const how = raised ? undefined
      : changedFromOrigin(draft, ["confinement_class"]) ? NR_REVIEW.PROV_SELECTED
      : intent.confinement_class ? originSource(draft.origin)
      : fromList?.default_source === "organisation" ? NR_REVIEW.PROV_ORG_DEFAULT : NR_RUN.BADGE_DEFAULT;
    rows.push({ id: "barrier", label: NR_REVIEW.POLICY_BARRIER, barrier, detail: joined(how, pre?.runner_pool && NR_REVIEW.WHERE_VALUE(pre.runner_pool.name)) });
  }

  const hosts = access?.hosts.length ?? preview?.spec.allowed_domains?.length;
  if (hosts !== undefined) {
    rows.push({ id: "network", label: NR_REVIEW.POLICY_NETWORK, value: hosts === 1 ? NR_REVIEW.POLICY_HOST_ONE : NR_REVIEW.POLICY_HOSTS(hosts), detail: joined(NR_REVIEW.PROV_SOURCE_POLICY, preview?.source.name) });
  }

  const kinds = (access?.credentials ?? []).map((c) => access?.rows.find((r) => r.id === c.row)?.name).filter((n): n is string => !!n);
  const provider = pre?.model_credential?.provider;
  rows.push({
    id: "credentials",
    label: NR_REVIEW.POLICY_CREDENTIALS,
    value: joined(provider, ...kinds),
    detail: provider ? NR_REVIEW.POLICY_HIDDEN : kinds.length ? undefined : NR_REVIEW.POLICY_NO_CREDENTIALS,
  });

  const names = joined(...(pre?.attachments ?? []).map((a) => a.name));
  rows.push({ id: "files", label: NR_REVIEW.POLICY_FILES, value: names ?? NR_REVIEW.WORK_NONE, detail: serverSource(ctx, { fields: ["workspace_mounts", "workspace_repos"] }) ?? draftSource(draft, FILE_KEYS, NR_REVIEW.PROV_ADDED) });

  // One value per distinct default across the included harnesses, so a mixed set is not summarised by its first.
  // When they differ each says which harness it is for; when they agree it is one value and its source policy.
  const defaults = (access?.harnesses ?? []).flatMap((h) => (h.default ? [{ harness: h.harness, default: h.default }] : []));
  const effectOf = (d: { default: { effective: string } }) => EFFECT_VALUE[d.default.effective] ?? NR_REVIEW.POLICY_HOLD;
  const effects = [...new Set(defaults.map(effectOf))];
  const value = !defaults.length
    ? intent.tool_approvals === "hold" ? NR_REVIEW.POLICY_HOLD : NR_REVIEW.POLICY_AUTOMATIC
    : effects.length > 1
      ? joined(...defaults.map((d) => NR_REVIEW.POLICY_TOOLS_OF(harnessLabel(d.harness), effectOf(d))))
      : effects[0];
  // Each harness's own walls (#2822): one text when they agree, each harness's when they differ.
  const walls = (access?.harnesses ?? []).flatMap((h) => (h.inner_walls ? [{ harness: h.harness, text: innerWallsText(h.inner_walls) }] : []));
  const wallTexts = [...new Set(walls.map((w) => w.text))];
  const wallsDetail = wallTexts.length > 1 ? joined(...walls.map((w) => NR_REVIEW.POLICY_TOOLS_OF(harnessLabel(w.harness), w.text))) : wallTexts[0];
  const toolsDetail = effects.length === 1 ? joined(defaults[0].default.source.name, defaults[0].default.source.id === ACCEPT_EDITS_SOURCE ? NR_REVIEW.POLICY_ACCEPTS_EDITS : undefined) : undefined;
  rows.push({ id: "tools", label: NR_REVIEW.POLICY_TOOLS, value, detail: joined(toolsDetail, wallsDetail) });
  return rows;
}
