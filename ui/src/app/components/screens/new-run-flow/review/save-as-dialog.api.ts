/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// POST /templates, with the refusal's diagnostics kept: HttpError carries the
// first sentence and reason only, and a save that left a dependency out needs
// every diagnostic to list what to set up. Moves into lib/api/templates.ts
// when that fetcher lands.

import { HttpError, wfetch } from "../../../../lib/api/core";
import type { Template, TemplateDiagnostic, TemplateSaveRequest } from "../../../../lib/types/templates";

export class TemplateSaveError extends HttpError {
  diagnostics: TemplateDiagnostic[];
  constructor(status: number, message: string, reason: string, diagnostics: TemplateDiagnostic[]) {
    super(status, message, reason);
    this.diagnostics = diagnostics;
    this.name = "TemplateSaveError";
  }
}

export type SaveTemplate = (req: TemplateSaveRequest) => Promise<Template>;

export const saveTemplate: SaveTemplate = async (req) => {
  const res = await wfetch("/templates", { method: "POST", asPerson: true, body: JSON.stringify(req) });
  if (res.ok) return (await res.json()) as Template;
  let body: Partial<{ error: string; reason: string; diagnostics: TemplateDiagnostic[] }> = {};
  try {
    body = JSON.parse(await res.text());
  } catch {
    // not the refusal envelope: the status text stands in
  }
  throw new TemplateSaveError(res.status, body.error || res.statusText, body.reason ?? "", Array.isArray(body.diagnostics) ? body.diagnostics : []);
};
