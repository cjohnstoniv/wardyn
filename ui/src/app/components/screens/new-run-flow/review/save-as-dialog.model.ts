/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the Save-as dialog stores: which parts of the draft a template or a
// policy can hold, the audiences the saver may write, and the save request.
// Pure functions; the dialog adds only state and markup.

import { TEMPLATE_POLICY_FIELDS } from "../../../../lib/template-fields";
import { TEMPLATE_REASON } from "../../../../lib/template-refusals";
import {
  TEMPLATE_API_VERSION,
  TEMPLATE_KIND,
  type TemplateCoverage,
  type TemplateKind,
  type TemplatePublish,
  type TemplateSaveRequest,
  type TemplateScope,
  type TemplateSetupNeed,
} from "../../../../lib/types/templates";
import { FIELD_HOME, REVIEW_GROUP_OF_FIELD, type ReviewGroupId } from "../draft/field-homes";
import type { RunIntent } from "../draft/run-intent";

/** The template checklist is the Review groups; the policy checklist is its three parts. */
export type SavePartId = ReviewGroupId | "network" | "credentials" | "access_rules";

/** One checklist row: the document fields it carries, written `inline_policy.<name>` inside the overlay. */
export interface SavePart {
  id: SavePartId;
  fields: string[];
}

const POLICY_PART_OF_REGISTRY: Record<string, SavePartId> = { egress: "network", credentials: "credentials", access_rules: "access_rules" };

/**
 * The policy keys a policy may hold, read from the field registry the way the
 * server reads it: run-wide keys of the network, credential and access-rule
 * parts that are carried and not pending.
 */
export const POLICY_KEY_PART: Readonly<Record<string, SavePartId>> = Object.fromEntries(
  TEMPLATE_POLICY_FIELDS.filter((r) => r.part && r.part in POLICY_PART_OF_REGISTRY && r.section === "run_wide" && !r.excluded && !r.pending)
    .map((r) => [r.name, POLICY_PART_OF_REGISTRY[r.part as string]]),
);

const PART_ORDER: SavePartId[] = ["run", "where", "work", "tools", "access", "limits", "network", "credentials", "access_rules"];

const present = (v: unknown) => v !== undefined;

/** The fields of an intent by checklist row, plus the fields no row owns (kept by "Everything" only). */
export function savePartsOf(kind: TemplateKind, intent: RunIntent): { parts: SavePart[]; unassigned: string[] } {
  const byId = new Map<SavePartId, string[]>();
  const unassigned: string[] = [];
  const add = (id: SavePartId | undefined, field: string) => {
    if (!id) unassigned.push(field);
    else byId.set(id, [...(byId.get(id) ?? []), field]);
  };
  const overlay = (intent.inline_policy ?? {}) as Record<string, unknown>;
  if (kind === "policy") {
    for (const k of Object.keys(overlay)) if (present(overlay[k]) && POLICY_KEY_PART[k]) add(POLICY_KEY_PART[k], `inline_policy.${k}`);
  } else {
    for (const [k, v] of Object.entries(intent)) {
      if (!present(v) || FIELD_HOME[k] === "excluded") continue;
      if (k !== "inline_policy") add(REVIEW_GROUP_OF_FIELD[k], k);
      else for (const p of Object.keys(overlay)) if (present(overlay[p])) add(REVIEW_GROUP_OF_FIELD[`inline_policy.${p}`] ?? "access", `inline_policy.${p}`);
    }
  }
  const parts = PART_ORDER.filter((id) => byId.has(id)).map((id) => ({ id, fields: byId.get(id) as string[] }));
  return { parts, unassigned };
}

/** The intent holding exactly `fields`; absent stays absent, a kept false, 0 or [] is kept. */
export function pickIntent(intent: RunIntent, fields: readonly string[]): RunIntent {
  const out: Record<string, unknown> = {};
  const overlay: Record<string, unknown> = {};
  const source = intent as Record<string, unknown>;
  const policy = (intent.inline_policy ?? {}) as Record<string, unknown>;
  for (const f of fields) {
    if (f.startsWith("inline_policy.")) overlay[f.slice("inline_policy.".length)] = policy[f.slice("inline_policy.".length)];
    else out[f] = source[f];
  }
  if (Object.keys(overlay).length > 0) out.inline_policy = overlay;
  return out as RunIntent;
}

/**
 * A draft shared with other people leaves out the saver's own member drives:
 * a shared template shows the names it references to its whole audience, and
 * the people it is offered to can never attach them. A start folder on one of
 * them goes with it, the way detaching the drive clears it. `ownDriveIds` undefined means the catalogue is
 * not known (loading, or it failed): then no drive can be told apart as the saver's own, so every drive and
 * the drive start folder go, rather than the unknown being read as "no drives".
 */
export function leaveOutOwnDrives(intent: RunIntent, ownDriveIds: ReadonlySet<string> | undefined): { intent: RunIntent; leftOut: boolean } {
  const isOwn = (id: string) => ownDriveIds === undefined || ownDriveIds.has(id);
  const drives = Array.isArray(intent.drives) ? intent.drives : [];
  const kept = drives.filter((d) => !(d.drive_id && isOwn(d.drive_id)));
  const start = intent.start_folder;
  const startOwn = start?.kind === "attachment" && !!start.drive_id && isOwn(start.drive_id);
  if (kept.length === drives.length && !startOwn) return { intent, leftOut: false };
  const rest: Record<string, unknown> = { ...intent };
  if (kept.length > 0) rest.drives = kept;
  else delete rest.drives;
  if (startOwn) delete rest.start_folder;
  return { intent: rest as RunIntent, leftOut: true };
}

/** An "Available to" option. `person` is the saver's own scope. */
export interface SaveAudience {
  value: string;
  scope: TemplateScope;
  groupId?: string;
  /** The group's label; absent for "Only me" and for a group nobody labelled. */
  label?: string;
}

export const ONLY_ME = "person";

/**
 * Only the audiences the saver can write: themselves, and the groups they
 * manage and are verified in. The organisation is never offered here (it is
 * published from the Templates screen), and no group is offered while the
 * sign-in's group list is unverified.
 */
export function saveAudiences(publish: TemplatePublish): SaveAudience[] {
  const groups = publish.groups_unverified ? [] : publish.groups;
  return [
    { value: ONLY_ME, scope: "person" },
    ...groups.map((g): SaveAudience => ({ value: `group:${g}`, scope: "group", groupId: g, label: publish.group_labels?.[g] })),
  ];
}

export interface BuildSaveInput {
  kind: TemplateKind;
  audience: SaveAudience;
  name: string;
  intent: RunIntent;
  coverage: TemplateCoverage;
  needsSetup?: TemplateSetupNeed[];
}

/** Title and description never reach this: the intent type has neither, and the request names the template only. */
export function buildSaveRequest(i: BuildSaveInput): TemplateSaveRequest {
  return {
    scope: i.audience.scope,
    ...(i.audience.groupId ? { group_id: i.audience.groupId } : {}),
    name: i.name.trim(),
    kind: i.kind,
    document: {
      api_version: TEMPLATE_API_VERSION,
      kind: TEMPLATE_KIND,
      coverage: i.coverage,
      intent: i.intent,
      ...(i.needsSetup?.length ? { needs_setup: i.needsSetup } : {}),
    },
  };
}

// The `needs` word of the dependency sentence the server and the console say in the same bytes.
const DEPENDENCY_SENTENCE = /^\S+ needs (\S+)\. Include it, or list it under needs_setup\.$/;

/** The fields a refused save says it still needs, to list under needs_setup. */
export function missingDependencies(diagnostics: readonly { message: string; reason: string }[]): TemplateSetupNeed[] {
  const needs = new Set<string>();
  for (const d of diagnostics) {
    const m = d.reason === TEMPLATE_REASON.DEPENDENCY_MISSING ? DEPENDENCY_SENTENCE.exec(d.message) : null;
    if (m) needs.add(m[1]);
  }
  return [...needs].sort().map((field) => ({ field }));
}
