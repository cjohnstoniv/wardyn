/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { readableDiff } from "./readable-diff";

describe("readableDiff", () => {
  it("returns nothing for two identical values", () => {
    expect(readableDiff({ a: 1, b: "x" }, { a: 1, b: "x" })).toEqual([]);
  });

  it("names the changed top-level field, not the untouched ones", () => {
    const before = { default_branch: "main", clone_depth: 1 };
    const after = { default_branch: "release/0.8", clone_depth: 1 };
    expect(readableDiff(before, after)).toEqual(["default_branch: main → release/0.8"]);
  });

  it("nests the path through objects — never the whole draft as one JSON blob", () => {
    const before = { storage: { ephemeral: { default_disk_mib: 1024 } } };
    const after = { storage: { ephemeral: { default_disk_mib: 2048 } } };
    expect(readableDiff(before, after)).toEqual(["storage.ephemeral.default_disk_mib: 1024 → 2048"]);
  });

  it("walks equal-length arrays index by index", () => {
    const before = { git: [{ id: "a", base_urls: ["https://x"] }] };
    const after = { git: [{ id: "a", base_urls: ["https://y"] }] };
    expect(readableDiff(before, after)).toEqual(["git[0].base_urls[0]: https://x → https://y"]);
  });

  it("collapses an array whose length changed to one readable line", () => {
    const before = { base_urls: ["https://x"] };
    const after = { base_urls: ["https://x", "https://y"] };
    expect(readableDiff(before, after)).toEqual([
      'base_urls: ["https://x"] → ["https://x","https://y"]',
    ]);
  });

  it("reads an empty string and undefined as something a person can see", () => {
    expect(readableDiff({ a: "" }, { a: "x" })).toEqual(["a: (empty) → x"]);
    expect(readableDiff({ a: undefined }, { a: "x" })).toEqual(["a: (none) → x"]);
  });
});
