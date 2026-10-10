/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The template refusals the server and the console say in the same bytes: the
// wire reasons, and the sentences. The console prints the server's sentence on
// a refusal and uses these for its own pre-checks, so the two never disagree.
// Both sides are pinned to template-refusals.golden.json
// (internal/api/template_refusals_test.go, template-refusals.test.ts); change a
// sentence in template_refusals.go, here and in that table together.

/** The wire reasons (internal/api/reasons.go). The console matches on these, never on the text. */
export const TEMPLATE_REASON = {
  DOCUMENT_INVALID: "template_document_invalid",
  VERSION_UNSUPPORTED: "template_version_unsupported",
  FIELD_UNKNOWN: "template_field_unknown",
  FIELD_INVALID: "template_field_invalid",
  FIELD_EXCLUDED: "template_field_excluded",
  SECRET_REFUSED: "template_secret_refused",
  RUN_STATE_REFUSED: "template_run_state_refused",
  METADATA_IN_CONTENT: "template_metadata_in_content",
  /** A valid field the server cannot honour yet: refused, never dropped. */
  FIELD_UNAVAILABLE: "template_field_unavailable",
  DEPENDENCY_MISSING: "template_dependency_missing",
  COVERAGE_INVALID: "template_coverage_invalid",
  SHARED_FIELD_REFUSED: "template_shared_field_refused",
  SCOPE_FORBIDDEN: "template_scope_forbidden",
  GROUP_UNVERIFIED: "template_group_unverified",
  NOT_FOUND: "template_not_found",
  REVISION_CONFLICT: "template_revision_conflict",
} as const;

export type TemplateReason = (typeof TEMPLATE_REASON)[keyof typeof TEMPLATE_REASON];

/** The sentences, byte for byte what internal/api/template_refusals.go says. */
export const TEMPLATE_REFUSAL = {
  DOCUMENT_INVALID: (detail: string) => `This is not a template document: ${detail}.`,
  VERSION_UNSUPPORTED: (what: string) =>
    `${what} is not a template format this server reads. Use api_version wardyn/v1 and kind RunTemplate.`,
  FIELD_UNKNOWN: (path: string) => `${path} is not a template field. Remove it, or check the spelling.`,
  FIELD_INVALID: (path: string, detail: string) => `${path} ${detail}.`,
  FIELD_EXCLUDED: (path: string) => `${path} is never part of a template: it belongs to one launch, or it is retired.`,
  SECRET_REFUSED: (path: string) =>
    `${path} holds what looks like a secret. A template keeps references to secrets, never their values.`,
  RUN_STATE_REFUSED: (path: string) =>
    `${path} is state of one run. A template holds reusable choices, not a run's history or authority.`,
  METADATA_IN_CONTENT: (path: string) => `${path} is set by the server from who saves the template. Remove it from the document.`,
  FIELD_UNAVAILABLE: (path: string) => `${path} cannot be used in a template on this server yet. Remove it for now: nothing was dropped.`,
  DEPENDENCY_MISSING: (path: string, needs: string) => `${path} needs ${needs}. Include it, or list it under needs_setup.`,
  COVERAGE_INVALID: (detail: string) => `The template's coverage does not fit its content: ${detail}.`,
  SHARED_FIELD_REFUSED: (path: string, scope: string) =>
    `${path} names something only one person has, so it cannot be in a ${scope} template.`,
  SCOPE_FORBIDDEN_ORG: () => "Only an organisation administrator can publish templates for the organisation.",
  SCOPE_FORBIDDEN_GROUP: () => "Only an administrator of that group can publish templates to it.",
  SCOPE_FORBIDDEN_PERSON: () => "A personal template belongs to the person who owns it.",
  GROUP_UNVERIFIED: () => "Your group membership cannot be checked from this sign-in. Sign in again, then retry.",
  NOT_FOUND: () => "That template does not exist, or you cannot see it.",
  REVISION_CONFLICT: (current: string) =>
    `This template changed to revision ${current} since you opened it. Reload it, then apply your edit again.`,
} as const;

/** Every sentence function by key, for the parity test and for callers that pick one by reason. */
export const TEMPLATE_REFUSAL_BY_KEY: Record<keyof typeof TEMPLATE_REFUSAL, (...args: string[]) => string> = {
  DOCUMENT_INVALID: (...a) => TEMPLATE_REFUSAL.DOCUMENT_INVALID(a[0]),
  VERSION_UNSUPPORTED: (...a) => TEMPLATE_REFUSAL.VERSION_UNSUPPORTED(a[0]),
  FIELD_UNKNOWN: (...a) => TEMPLATE_REFUSAL.FIELD_UNKNOWN(a[0]),
  FIELD_INVALID: (...a) => TEMPLATE_REFUSAL.FIELD_INVALID(a[0], a[1]),
  FIELD_EXCLUDED: (...a) => TEMPLATE_REFUSAL.FIELD_EXCLUDED(a[0]),
  SECRET_REFUSED: (...a) => TEMPLATE_REFUSAL.SECRET_REFUSED(a[0]),
  RUN_STATE_REFUSED: (...a) => TEMPLATE_REFUSAL.RUN_STATE_REFUSED(a[0]),
  METADATA_IN_CONTENT: (...a) => TEMPLATE_REFUSAL.METADATA_IN_CONTENT(a[0]),
  FIELD_UNAVAILABLE: (...a) => TEMPLATE_REFUSAL.FIELD_UNAVAILABLE(a[0]),
  DEPENDENCY_MISSING: (...a) => TEMPLATE_REFUSAL.DEPENDENCY_MISSING(a[0], a[1]),
  COVERAGE_INVALID: (...a) => TEMPLATE_REFUSAL.COVERAGE_INVALID(a[0]),
  SHARED_FIELD_REFUSED: (...a) => TEMPLATE_REFUSAL.SHARED_FIELD_REFUSED(a[0], a[1]),
  SCOPE_FORBIDDEN_ORG: () => TEMPLATE_REFUSAL.SCOPE_FORBIDDEN_ORG(),
  SCOPE_FORBIDDEN_GROUP: () => TEMPLATE_REFUSAL.SCOPE_FORBIDDEN_GROUP(),
  SCOPE_FORBIDDEN_PERSON: () => TEMPLATE_REFUSAL.SCOPE_FORBIDDEN_PERSON(),
  GROUP_UNVERIFIED: () => TEMPLATE_REFUSAL.GROUP_UNVERIFIED(),
  NOT_FOUND: () => TEMPLATE_REFUSAL.NOT_FOUND(),
  REVISION_CONFLICT: (...a) => TEMPLATE_REFUSAL.REVISION_CONFLICT(a[0]),
};

/** The reason each sentence belongs to. */
export const TEMPLATE_REFUSAL_REASON: Record<keyof typeof TEMPLATE_REFUSAL, TemplateReason> = {
  DOCUMENT_INVALID: TEMPLATE_REASON.DOCUMENT_INVALID,
  VERSION_UNSUPPORTED: TEMPLATE_REASON.VERSION_UNSUPPORTED,
  FIELD_UNKNOWN: TEMPLATE_REASON.FIELD_UNKNOWN,
  FIELD_INVALID: TEMPLATE_REASON.FIELD_INVALID,
  FIELD_EXCLUDED: TEMPLATE_REASON.FIELD_EXCLUDED,
  SECRET_REFUSED: TEMPLATE_REASON.SECRET_REFUSED,
  RUN_STATE_REFUSED: TEMPLATE_REASON.RUN_STATE_REFUSED,
  METADATA_IN_CONTENT: TEMPLATE_REASON.METADATA_IN_CONTENT,
  FIELD_UNAVAILABLE: TEMPLATE_REASON.FIELD_UNAVAILABLE,
  DEPENDENCY_MISSING: TEMPLATE_REASON.DEPENDENCY_MISSING,
  COVERAGE_INVALID: TEMPLATE_REASON.COVERAGE_INVALID,
  SHARED_FIELD_REFUSED: TEMPLATE_REASON.SHARED_FIELD_REFUSED,
  SCOPE_FORBIDDEN_ORG: TEMPLATE_REASON.SCOPE_FORBIDDEN,
  SCOPE_FORBIDDEN_GROUP: TEMPLATE_REASON.SCOPE_FORBIDDEN,
  SCOPE_FORBIDDEN_PERSON: TEMPLATE_REASON.SCOPE_FORBIDDEN,
  GROUP_UNVERIFIED: TEMPLATE_REASON.GROUP_UNVERIFIED,
  NOT_FOUND: TEMPLATE_REASON.NOT_FOUND,
  REVISION_CONFLICT: TEMPLATE_REASON.REVISION_CONFLICT,
};
