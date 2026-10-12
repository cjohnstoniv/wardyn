/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// POST /templates/import with kind Run: reads one document the way every door
// does and says what is wrong with it. It stores nothing.
import { asJson, wfetch } from "../../../../lib/api/core";
import type { TemplateImportResult } from "../../../../lib/types/templates";
import { RUN_DOCUMENT_KIND } from "../../../../lib/types/templates";

export type ImportDocument = (format: "yaml" | "json", source: string, signal?: AbortSignal) => Promise<TemplateImportResult>;

export const importRunDocument: ImportDocument = async (format, source, signal) =>
  asJson<TemplateImportResult>(
    await wfetch("/templates/import", { method: "POST", body: JSON.stringify({ format, source, kind: RUN_DOCUMENT_KIND }), signal }),
  );
