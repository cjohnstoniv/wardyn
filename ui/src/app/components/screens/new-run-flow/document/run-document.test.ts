/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { RUN_DOCUMENT_CANON } from "../../../../lib/run-document-canon";
import { parsePolicySource } from "../../../../lib/policy-document";
import { exampleDocumentText, locatePath, MAX_DOCUMENT_BYTES, placeDiagnostics, runDocumentText, tooLarge } from "./run-document";

const YAML = "api_version: wardyn/v1\nkind: Run\nintent:\n  tools:\n    - id: claude-code\n      kind: harness\n  ends_after_sec: 60\n  no_end: true\n";
const JSON_TEXT = '{\n  "api_version": "wardyn/v1",\n  "intent": {\n    "tools": [\n      { "id": "a" }\n    ]\n  }\n}';

describe("run document text", () => {
  it("writes the intent as a kind Run document that reads back in both formats", () => {
    const intent = { shape: "environment" as const, experience: "interactive" as const, ends_after_sec: 60 };
    for (const format of ["yaml", "json"] as const) {
      const parsed = parsePolicySource(runDocumentText(intent, format), format);
      expect(parsed).toEqual({ ok: true, value: { api_version: "wardyn/v1", kind: "Run", intent } });
    }
  });

  it("an example names the harness as a tool", () => {
    expect(parsePolicySource(exampleDocumentText("codex-cli", "yaml"), "yaml")).toMatchObject({
      ok: true, value: { kind: "Run", intent: { tools: [{ id: "codex-cli", kind: "harness" }] } },
    });
  });

  it("the size cap counts bytes, not characters", () => {
    expect(tooLarge("a".repeat(MAX_DOCUMENT_BYTES))).toBe(false);
    expect(tooLarge("é".repeat(MAX_DOCUMENT_BYTES / 2 + 1))).toBe(true);
  });
});

describe("locatePath", () => {
  it("finds a key, a nested key and a list item in YAML", () => {
    expect(locatePath(YAML, "kind")).toEqual({ line: 2, column: 1 });
    expect(locatePath(YAML, "intent.ends_after_sec")).toEqual({ line: 7, column: 3 });
    expect(locatePath(YAML, "intent.tools[0].kind")).toEqual({ line: 6, column: 7 });
    expect(locatePath(YAML, "intent.tools[0]").line).toBe(5);
  });

  it("reads JSON by the same rule", () => {
    expect(locatePath(JSON_TEXT, "api_version").line).toBe(2);
    expect(locatePath(JSON_TEXT, "intent.tools[0].id").line).toBe(5);
  });

  it("falls back to the deepest key that exists, then to the start", () => {
    expect(locatePath(YAML, "intent.nothing.here").line).toBe(3);
    expect(locatePath(YAML, "").line).toBe(1);
    expect(locatePath("not: [closed", "not").line).toBe(1);
  });
});

describe("placeDiagnostics", () => {
  it("uses the canon sentence where there is one and marks both end keys", () => {
    const placed = placeDiagnostics(YAML, "yaml", [
      { path: "intent", reason: "run_end_conflicting_fields", message: "server" },
      { path: "intent.tools[0].kind", reason: "template_field_invalid", message: "kind must be harness or component" },
    ]);
    expect(placed.map((d) => [d.line, d.text])).toEqual([
      [7, RUN_DOCUMENT_CANON.END_CONFLICT],
      [8, RUN_DOCUMENT_CANON.END_CONFLICT],
      [6, "kind must be harness or component"],
    ]);
  });

  it("keeps the path and reason for the draft", () => {
    expect(placeDiagnostics(YAML, "yaml", [{ path: "kind", reason: "template_version_unsupported", message: "m" }])[0])
      .toMatchObject({ path: "kind", reason: "template_version_unsupported", text: "m" });
  });
});
