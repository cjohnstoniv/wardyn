/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The four errors the run-document editor says in the approved mock's words.
// The server answers with a reason and a path (POST /templates/import,
// internal/api/template_document.go); this maps the four it has a canon string
// for, and every other diagnostic shows the server's own sentence.
// TestRunDocumentCanonTriggers (internal/api/run_document_test.go) fires each of
// the four Go triggers.

import { TEMPLATE_REASON } from "./template-refusals";

export type RunDocumentFormat = "json" | "yaml";

/** run_end_conflicting_fields (internal/api/reasons_routes.go); the document door reports it at intent. */
export const RUN_END_CONFLICT_REASON = "run_end_conflicting_fields";

export const RUN_DOCUMENT_CANON = {
  TITLE_OR_DESCRIPTION:
    "Templates and imports do not carry a run title or description. Remove them; you name the run when you launch it.",
  END_CONFLICT: "ends_after_sec and no_end cannot be used together.",
  JSON_INVALID: "This JSON document isn't valid. Check the highlighted structure.",
  API_VERSION: "api_version must be wardyn/v1.",
} as const;

const TITLE_OR_DESCRIPTION_PATHS = ["title", "description", "intent.title", "intent.description"];

/** The canon sentence for one server diagnostic, or null when the server's own sentence is shown. */
export function runDocumentCanon(reason: string, path: string, format: RunDocumentFormat): string | null {
  if (reason === TEMPLATE_REASON.FIELD_EXCLUDED && TITLE_OR_DESCRIPTION_PATHS.includes(path)) {
    return RUN_DOCUMENT_CANON.TITLE_OR_DESCRIPTION;
  }
  if (reason === RUN_END_CONFLICT_REASON) return RUN_DOCUMENT_CANON.END_CONFLICT;
  if (reason === TEMPLATE_REASON.DOCUMENT_INVALID && format === "json") return RUN_DOCUMENT_CANON.JSON_INVALID;
  if (reason === TEMPLATE_REASON.VERSION_UNSUPPORTED && path === "api_version") return RUN_DOCUMENT_CANON.API_VERSION;
  return null;
}

/** What the editor shows for a diagnostic: the canon sentence when there is one, else the server's. */
export function runDocumentErrorText(d: { reason: string; path: string; message: string }, format: RunDocumentFormat): string {
  return runDocumentCanon(d.reason, d.path, format) ?? d.message;
}
