/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The template field registry as the console reads it: every request and
// policy field a template can carry, its owning wizard tab and refinement
// part, and what leaving it out (or giving it an empty value) means. The Go
// registry (internal/api/template_fields.go) is the source; this file is its
// typed view of template-fields.golden.json, regenerated with
//   WARDYN_UPDATE_GOLDEN=1 go test ./internal/api -run TestTemplateFieldsGolden
//
// Omission is explicit. A field left out of a template never means "off": the
// template does not specify it, and the launch baseline (which the
// organisation's current defaults can change) or an unset control takes over.

import golden from "./template-fields.golden.json";
import type { TemplatePart } from "./types/templates";

export type TemplateTab = "info" | "runner" | "repositories_drives" | "tools_image" | "access";

/** What leaving the field out means. */
export type TemplateOmission = "baseline" | "unset_required" | "unset_optional";

/** What a present empty, false or zero value means. */
export type TemplateEmptyMeaning = "same_as_omitted" | "value" | "all" | "never";

export type TemplateExclusion = "secret" | "origin" | "retired";

export interface TemplateFieldRule {
  name: string;
  tab?: TemplateTab;
  part?: TemplatePart;
  omitted?: TemplateOmission;
  empty?: TemplateEmptyMeaning;
  /** The sentence the refinement flow shows for the omission. */
  meaning?: string;
  /** Free text a person writes for one run: kept only when they choose to include it. */
  sensitive?: boolean;
  /** Names something one person has: a shared (org or group) template cannot carry it. */
  person_only?: boolean;
  /** Set when a template never carries the field. */
  excluded?: TemplateExclusion;
  why?: string;
}

/** Every CreateRunRequest field, plus `pool_id`. */
export const TEMPLATE_REQUEST_FIELDS = golden.request as readonly TemplateFieldRule[];

/** Every RunPolicySpec field, written `inline_policy.<name>` in a template. */
export const TEMPLATE_POLICY_FIELDS = golden.policy as readonly TemplateFieldRule[];

/** Nested closed types and the fields of each a template carries. */
export const TEMPLATE_NESTED_CARRIED = golden.nested_carried as Readonly<Record<string, readonly string[]>>;

/** Nested fields a template never carries, by `Type.field`. */
export const TEMPLATE_NESTED_EXCLUDED = golden.nested_excluded as readonly { path: string; excluded: TemplateExclusion; why: string }[];

/** The rule of a request field (`agent`) or a policy field (`inline_policy.allowed_domains`). */
export function templateFieldRule(name: string): TemplateFieldRule | undefined {
  const policy = name.startsWith("inline_policy.") ? name.slice("inline_policy.".length) : undefined;
  return (policy === undefined ? TEMPLATE_REQUEST_FIELDS : TEMPLATE_POLICY_FIELDS).find((r) => r.name === (policy ?? name));
}

/** The fields a template can specify: carried ones only. */
export function templateCarriedFields(): string[] {
  return [
    ...TEMPLATE_REQUEST_FIELDS.filter((r) => !r.excluded).map((r) => r.name),
    ...TEMPLATE_POLICY_FIELDS.filter((r) => !r.excluded).map((r) => `inline_policy.${r.name}`),
  ];
}

/** The parts a document includes: the refinement group of each field it specifies, in first-seen order. */
export function templateIncludes(intent: Record<string, unknown>): TemplatePart[] {
  const parts: TemplatePart[] = [];
  const add = (p?: TemplatePart) => {
    if (p && !parts.includes(p)) parts.push(p);
  };
  for (const name of Object.keys(intent)) {
    if (name === "inline_policy" && typeof intent[name] === "object" && intent[name] !== null) {
      for (const field of Object.keys(intent[name] as object)) add(templateFieldRule(`inline_policy.${field}`)?.part);
    } else {
      add(templateFieldRule(name)?.part);
    }
  }
  return parts.sort();
}
