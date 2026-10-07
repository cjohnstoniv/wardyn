/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, expect, it, vi } from "vitest";
import * as yaml from "yaml";
import { editPolicySource, parsePolicySource } from ".";

afterEach(() => vi.restoreAllMocks());

it.each(["parse", "compose", "AST", "toJS"])("contains an unexpected %s exception at the source boundary", (phase) => {
  const crash = () => { throw new Error(`failed during ${phase}`); };
  if (phase === "parse") vi.spyOn(yaml.Parser.prototype, "parse").mockImplementationOnce(crash);
  if (phase === "compose") vi.spyOn(yaml.Composer.prototype, "compose").mockImplementationOnce(crash);
  if (phase === "AST") {
    const document = yaml.parseDocument("a: 1");
    const scalar = document.get("a", true);
    if (!yaml.isScalar(scalar)) throw new Error("expected scalar fixture");
    Object.defineProperty(scalar, "value", { get: crash });
    vi.spyOn(yaml.Composer.prototype, "compose").mockImplementationOnce(function* () { yield document; });
  }
  if (phase === "toJS") vi.spyOn(yaml.Document.prototype, "toJS").mockImplementationOnce(crash);
  expect(parsePolicySource("a: 1")).toEqual({ ok: false, line: 1, column: 1, message: `failed during ${phase}` });
});

it("contains serialization failures without replacing the authored source", () => {
  vi.spyOn(yaml.Document.prototype, "toString").mockImplementationOnce(() => { throw new Error("cannot serialize"); });
  expect(editPolicySource("a: 1", ["a"], 2, "yaml")).toEqual({
    ok: false, line: 1, column: 1, message: "cannot serialize",
  });
});

it("contains a non-Error thrown by the parser", () => {
  vi.spyOn(yaml.Parser.prototype, "parse").mockImplementationOnce(() => { throw "failed"; });
  expect(parsePolicySource("a: 1")).toEqual({
    ok: false, line: 1, column: 1, message: "Policy source could not be read.",
  });
});

it("refuses a tag that the library reports only as a warning", () => {
  const document = yaml.parseDocument("a: !unknown value", { version: "1.2", schema: "core", customTags: [] });
  expect(document.errors).toEqual([]);
  expect(document.warnings.map((warning) => warning.code)).toEqual(["TAG_RESOLVE_FAILED"]);
  expect(parsePolicySource("a: !unknown value")).toMatchObject({ ok: false });
});
