/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Run: cd ui && pnpm vitest run src/app/lib/run-document-canon.test.ts

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { RUN_DOCUMENT_CANON, RUN_END_CONFLICT_REASON, runDocumentCanon, runDocumentErrorText } from "./run-document-canon";
import { TEMPLATE_REASON } from "./template-refusals";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found");
}

describe("run document canon errors", () => {
  it("maps each of the four server diagnostics to its canon sentence", () => {
    for (const path of ["title", "description", "intent.title", "intent.description"]) {
      for (const format of ["json", "yaml"] as const) {
        expect(runDocumentCanon(TEMPLATE_REASON.FIELD_EXCLUDED, path, format), path).toBe(RUN_DOCUMENT_CANON.TITLE_OR_DESCRIPTION);
      }
    }
    for (const path of ["intent", "", "intent.no_end"]) {
      expect(runDocumentCanon(RUN_END_CONFLICT_REASON, path, "yaml"), path).toBe(RUN_DOCUMENT_CANON.END_CONFLICT);
    }
    expect(runDocumentCanon(TEMPLATE_REASON.DOCUMENT_INVALID, "", "json")).toBe(RUN_DOCUMENT_CANON.JSON_INVALID);
    expect(runDocumentCanon(TEMPLATE_REASON.VERSION_UNSUPPORTED, "api_version", "yaml")).toBe(RUN_DOCUMENT_CANON.API_VERSION);
  });

  it("leaves everything else to the server's sentence", () => {
    expect(runDocumentCanon(TEMPLATE_REASON.FIELD_EXCLUDED, "intent.preset", "json")).toBeNull();
    expect(runDocumentCanon(TEMPLATE_REASON.DOCUMENT_INVALID, "", "yaml")).toBeNull();
    expect(runDocumentCanon(TEMPLATE_REASON.VERSION_UNSUPPORTED, "kind", "json")).toBeNull();
    expect(runDocumentCanon(TEMPLATE_REASON.FIELD_UNKNOWN, "name", "json")).toBeNull();
    const d = { reason: TEMPLATE_REASON.VERSION_UNSUPPORTED, path: "kind", message: "Job is not a document this server reads." };
    expect(runDocumentErrorText(d, "json")).toBe(d.message);
    expect(runDocumentErrorText({ ...d, path: "api_version" }, "json")).toBe(RUN_DOCUMENT_CANON.API_VERSION);
  });

  it("names a reason the server sends", () => {
    const go = readFileSync(join(repoRoot(), "internal/api/reasons_routes.go"), "utf8");
    expect(go).toContain(`"${RUN_END_CONFLICT_REASON}"`);
  });
});
