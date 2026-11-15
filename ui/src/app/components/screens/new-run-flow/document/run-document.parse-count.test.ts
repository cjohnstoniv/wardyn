/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it, vi } from "vitest";
import { isScalar, parseDocument } from "yaml";
import { placeDiagnostics } from "./run-document";

// Counts the parses and the key reads that placing diagnostics takes; each call still does the real work.
vi.mock("yaml", async (importOriginal) => {
  const yaml = await importOriginal<typeof import("yaml")>();
  return { ...yaml, parseDocument: vi.fn(yaml.parseDocument), isScalar: vi.fn(yaml.isScalar) };
});

// The server sends one diagnostic per unknown key, with no cap.
const KEYS = 2000;
const TEXT = `api_version: wardyn/v1\nkind: Run\n${Array.from({ length: KEYS }, (_, i) => `extra_${i}: 1\n`).join("")}`;
const DIAGNOSTICS = Array.from({ length: KEYS }, (_, i) => ({ path: `extra_${i}`, reason: "template_field_unknown", message: `extra_${i} is not a field` }));

describe("placing many diagnostics", () => {
  it("parses the document once and reads each key a bounded number of times", () => {
    vi.mocked(parseDocument).mockClear();
    vi.mocked(isScalar).mockClear();
    const placed = placeDiagnostics(TEXT, "yaml", DIAGNOSTICS);
    expect(placed.map((d) => d.line)).toEqual(Array.from({ length: KEYS }, (_, i) => i + 3));
    expect(vi.mocked(parseDocument)).toHaveBeenCalledTimes(1);
    // The parser's repeated-key check compares each key with every earlier one.
    expect(vi.mocked(parseDocument).mock.calls[0][1]).toMatchObject({ uniqueKeys: false });
    expect(vi.mocked(isScalar).mock.calls.length).toBeLessThanOrEqual(4 * KEYS);
  });
});
