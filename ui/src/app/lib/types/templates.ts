/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run-template wire contract (types only): the import document, the
// catalogue entries and the custom-component configuration schema. Every
// interface mirrors one Go struct (pkg/client/templates.go,
// internal/types/component_config_schema.go) and wire-parity.test.ts pins its
// json tags to that struct's source. These shapes are the contract the server's
// template store fills in: until it lands the routes answer 501 templates_unavailable.
//
// A template is content, never an admission. It carries no secret value, run
// identity, session or claim state, and using one re-checks the launcher's own
// current policy, credentials, components, pools and drives. It carries no run
// title or description either: a run is named in the dialog at launch.

export type TemplateScope = "person" | "org" | "group";

/** What a template intends to hold: a whole setup, or a mix of parts that may list what it needs set up when used. */
export type TemplateCoverage = "full" | "partial";

export type TemplateFormat = "json" | "yaml";

/** The refinement groups of a template (Go: client.TemplatePart). */
export type TemplatePart =
  | "runner"
  | "resources"
  | "repositories"
  | "drives"
  | "tools_image"
  | "egress"
  | "credentials"
  | "components"
  | "access_rules"
  | "policy_ref";

/**
 * The fields a template specifies. PRESENCE is meaning: a key that is absent
 * means "this template does not specify it", and one that is present with
 * false, 0, "" or [] is a choice. Never default an absent key to a value, and
 * never drop a present one. The names are the request fields and `inline_policy`
 * (template-fields.golden.json lists them, with what omitting each means). A
 * pool is the request's own `runner_pool_id`, pending until the pools lane lands.
 */
export type TemplateIntent = Record<string, unknown>;

export interface TemplateSetupNeed {
  /** An intent field, or `inline_policy.<field>`, the intent does not specify. */
  field: string;
  reason?: string;
}

/** The import and export document. Owner, scope, group and revision are the catalogue's, never the document's. */
export interface TemplateDocument {
  api_version: string;
  kind: string;
  name?: string;
  description?: string;
  coverage: TemplateCoverage;
  intent: TemplateIntent;
  needs_setup?: TemplateSetupNeed[];
}

export const TEMPLATE_API_VERSION = "wardyn/v1";
export const TEMPLATE_KIND = "RunTemplate";

/** Pins a revision in a new-run draft; a later edit of the template does not move it. */
export interface TemplateRef {
  id: string;
  revision: number;
}

export interface Template {
  id: string;
  scope: TemplateScope;
  /** The owning person's subject, for person scope. */
  owner_id?: string;
  /** The group's canonical subject, for group scope. */
  group_id?: string;
  name: string;
  description?: string;
  revision: number;
  coverage: TemplateCoverage;
  document: TemplateDocument;
  /** Whether the caller may update or delete it. */
  writable: boolean;
  created_at: string;
  updated_at: string;
  created_by?: string;
  updated_by?: string;
}

export interface TemplateSummary {
  id: string;
  scope: TemplateScope;
  group_id?: string;
  name: string;
  description?: string;
  revision: number;
  coverage: TemplateCoverage;
  includes: TemplatePart[];
  needs_setup: string[];
  writable: boolean;
  updated_at: string;
}

export interface TemplateList {
  templates: TemplateSummary[];
}

export interface TemplateSaveRequest {
  scope: TemplateScope;
  group_id?: string;
  name: string;
  description?: string;
  document: TemplateDocument;
  /** Set to update: the revision the caller read. Any other current revision is a conflict. */
  expected_revision?: number;
}

export interface TemplateImportRequest {
  format: TemplateFormat;
  source: string;
}

/** One problem with a document: where (a JSON path), why (a closed reason) and a sentence. */
export interface TemplateDiagnostic {
  path: string;
  reason: string;
  message: string;
}

/** `document` is set only when `diagnostics` is empty. */
export interface TemplateImportResult {
  document?: TemplateDocument;
  diagnostics: TemplateDiagnostic[];
}

/** Copies (publishes) a template into another scope; the caller reads the source and writes the target. */
export interface TemplateCopyRequest {
  scope: TemplateScope;
  group_id?: string;
  name?: string;
}

/** GET /admin/template-group-admins. */
export interface TemplateGroupAdmins {
  grants: TemplateGroupAdmin[];
}

/** A bounded template-management grant: one person, one group's templates. */
export interface TemplateGroupAdmin {
  group_id: string;
  person: string;
  granted_by?: string;
}

// The custom-component configuration schema (Go: types.ComponentConfigSchema).

export type ConfigFieldKind = "string" | "boolean" | "integer" | "number" | "enum" | "string_list" | "secret_ref";

export type ConfigBindTarget = "config" | "secret";

export interface ConfigGroup {
  id: string;
  label: string;
  description?: string;
}

export interface ConfigOption {
  value: string;
  label: string;
}

export interface ConfigCondition {
  field: string;
  equals: unknown;
}

export interface ConfigBinding {
  target: ConfigBindTarget;
  /** The environment variable name; it follows the component env-name rules, reserved names included. */
  key: string;
}

export interface ConfigField {
  id: string;
  kind: ConfigFieldKind;
  label: string;
  description?: string;
  group?: string;
  required?: boolean;
  read_only?: boolean;
  default?: unknown;
  min?: number;
  max?: number;
  max_len?: number;
  max_items?: number;
  options?: ConfigOption[];
  visible_when?: ConfigCondition;
  bind: ConfigBinding;
}

export interface ComponentConfigSchema {
  schema_version: number;
  groups?: ConfigGroup[];
  fields: ConfigField[];
}

/** An attachment's values, keyed by field id, each typed by its field's kind. */
export type ComponentConfigValues = Record<string, unknown>;

export interface ConfigIssue {
  path: string;
  message: string;
}

/** The schema's bounds (Go: types.MaxConfigSchema*). */
export const CONFIG_SCHEMA_BOUNDS = {
  version: 1,
  maxBytes: 64 * 1024,
  maxFields: 64,
  maxGroups: 16,
  maxOptions: 64,
  maxListItems: 32,
  maxLabelRunes: 80,
  maxDescriptionRunes: 500,
  maxOptionValueRunes: 128,
} as const;
