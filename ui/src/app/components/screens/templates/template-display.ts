/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What a Templates row says about a summary. Every sentence is a constant of copy/new-run/templates.ts;
// this file only chooses which one and fills its placeholders.
import { templateFieldRule } from "../../../lib/template-fields";
import type { TemplatePart, TemplateSummary } from "../../../lib/types/templates";
import { NR_TEMPLATES as T } from "../../wardyn/copy/new-run/templates";

// The six "Includes" badges, in the order the screen shows them.
const GROUPS = ["PART_RUN", "PART_WHERE", "PART_TOOLS", "PART_FILES", "PART_ACCESS", "PART_LIMITS"] as const;
type Group = (typeof GROUPS)[number];

const GROUP_OF_PART: Record<TemplatePart, Group> = {
  runner: "PART_WHERE",
  resources: "PART_LIMITS",
  repositories: "PART_FILES",
  drives: "PART_FILES",
  tools_image: "PART_TOOLS",
  components: "PART_TOOLS",
  egress: "PART_ACCESS",
  credentials: "PART_ACCESS",
  access_rules: "PART_ACCESS",
  policy_ref: "PART_ACCESS",
};

// The registry files these under a part that is broader than the badge a reader expects.
const GROUP_OF_FIELD: Record<string, Group> = {
  agent: "PART_RUN",
  task: "PART_RUN",
  task_mode: "PART_RUN",
  interactive: "PART_RUN",
  interactive_start: "PART_RUN",
  workload: "PART_RUN",
  experience: "PART_RUN",
  shape: "PART_RUN",
  startup: "PART_RUN",
  ends_after_sec: "PART_LIMITS",
  no_end: "PART_LIMITS",
};

/** The badges a template carries, from the fields it specifies; the summary's parts when no field is known. */
export function includedGroups(s: Pick<TemplateSummary, "includes" | "fields">): string[] {
  const found = new Set<Group>();
  for (const f of s.fields) {
    const part = templateFieldRule(f)?.part;
    const group = GROUP_OF_FIELD[f] ?? (part && GROUP_OF_PART[part]);
    if (group) found.add(group);
  }
  if (found.size === 0) for (const p of s.includes) if (GROUP_OF_PART[p]) found.add(GROUP_OF_PART[p]);
  return GROUPS.filter((g) => found.has(g)).map((g) => T[g]);
}

/** "inline_policy.model_provider" → "Model provider": the field named, not a sentence about it. */
function fieldLabel(field: string): string {
  const words = field.replace(/^inline_policy\./, "").replace(/_/g, " ");
  return words.charAt(0).toUpperCase() + words.slice(1);
}

export function needsSetupLabel(s: Pick<TemplateSummary, "needs_setup">): string | null {
  return s.needs_setup.length === 0 ? null : T.NEEDS_SETUP(s.needs_setup.map(fieldLabel).join(", "));
}

type Audience = Pick<TemplateSummary, "scope" | "group_id" | "group_label">;

export function scopeLabel(s: Audience): string {
  if (s.scope === "person") return T.SCOPE_ME;
  if (s.scope === "org") return T.SCOPE_ORG;
  return T.GROUP_LABEL(s.group_label || s.group_id || "");
}

/** The second line of "Available to": the organisation's admins or the group's managers, never a person. */
export function ownerLine(s: Audience): string | null {
  if (s.scope === "org") return T.ORG_ADMINS;
  return s.scope === "group" ? T.TEMPLATE_MANAGERS : null;
}

/** Why Rename and Delete are unavailable on a row the caller cannot write. */
export function whyReadOnly(s: Audience): string | null {
  if (s.scope === "org") return T.WHY_ORG;
  return s.scope === "group" ? T.WHY_GROUP(scopeLabel(s)) : null;
}

/** The deleted template's audience in the confirmation: nobody else for "Only me", else the people it was offered to. */
export function deleteBody(s: Audience): string {
  return s.scope === "person" ? T.DELETE_BODY_ME : T.DELETE_BODY_SCOPE(scopeLabel(s));
}

/** Another template of the same audience already has the name (the server compares case-insensitively). */
export function nameTaken(rows: readonly TemplateSummary[], of: TemplateSummary, name: string): boolean {
  const key = name.toLowerCase();
  return rows.some(
    (r) => r.id !== of.id && r.scope === of.scope && (r.group_id ?? "") === (of.group_id ?? "") && r.name.toLowerCase() === key,
  );
}
