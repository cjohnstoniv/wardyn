/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import type { Component } from "../../../lib/types";
import { blankSecret, blankSetting, draftOf, headerHostChoices, newUuid, requestOf } from "./component-editor-model";

const stored: Component = {
  id: "5b1f3c1e-7f0a-4a52-9d57-0c1d2e3f4a5b",
  name: "Payments API",
  version: 3,
  created_at: "2026-10-08T00:00:00Z",
  updated_at: "2026-10-08T00:00:00Z",
  definition: {
    hosts: ["api.pay.example", "*.pay.example", "other.example:8443"],
    secrets: [
      { secret_name: "pay-key", shared: true, delivery: { mode: "header", host: "api.pay.example", header: "X-Api-Key", format: "%s" } },
      { secret_name: "pay-token", delivery: { mode: "env", var: "PAY_TOKEN" } },
      { secret_name: "pay-cert", delivery: { mode: "file", file: "pay.pem" } },
    ],
    config: { PAY_REGION: "eu" },
  },
};

describe("headerHostChoices", () => {
  it("offers bare hosts and :443 hosts without the port, never a wildcard or another port", () => {
    expect(headerHostChoices(["a.example", "b.example:443", "*.c.example", "d.example:8443", "a.example"])).toEqual([
      "a.example",
      "b.example",
    ]);
  });
});

describe("requestOf", () => {
  it("an untouched edit of a stored row sends the same definition back", () => {
    expect(requestOf(draftOf(stored))).toEqual({ name: "Payments API", definition: stored.definition });
  });

  it("writes only the chosen mode's fields, and shared only on a header", () => {
    const s = { ...blankSecret(), secret_name: "k", mode: "env" as const, var: "K", host: "stale.example", header: "H", plain_http: true, shared: true };
    const req = requestOf({ name: " n ", hosts: " a.example \n\n", secrets: [s], settings: [] });
    expect(req).toEqual({
      name: "n",
      definition: { hosts: ["a.example"], secrets: [{ secret_name: "k", delivery: { mode: "env", var: "K" } }] },
    });
  });

  it("a header with one host to choose from names it; blank header and format are left out", () => {
    const s = { ...blankSecret(), secret_name: "k", plain_http: true };
    const req = requestOf({ name: "n", hosts: "only.example:443", secrets: [s], settings: [] });
    expect(req.definition.secrets).toEqual([{ secret_name: "k", delivery: { mode: "header", host: "only.example", plain_http: true } }]);
  });

  it("omits secrets and config when there are none, and drops nameless settings", () => {
    const req = requestOf({ name: "n", hosts: "", secrets: [], settings: [blankSetting(), { ...blankSetting(), name: " K ", value: "v" }] });
    expect(req).toEqual({ name: "n", definition: { hosts: [], config: { K: "v" } } });
  });
});

describe("newUuid", () => {
  const V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

  it("is a v4 uuid", () => {
    expect(newUuid()).toMatch(V4);
  });

  it("still is one where randomUUID is missing (an insecure-context page)", () => {
    vi.stubGlobal("crypto", { getRandomValues: crypto.getRandomValues.bind(crypto) });
    try {
      const a = newUuid();
      expect(a).toMatch(V4);
      expect(newUuid()).not.toBe(a);
    } finally {
      vi.unstubAllGlobals();
    }
  });
});
