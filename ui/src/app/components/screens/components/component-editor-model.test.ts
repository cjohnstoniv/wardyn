/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import type { Component } from "../../../lib/types";
import { blankSecret, blankSetting, draftOf, headerHostChoices, newUuid, requestOf, requestRulesRefusal, withStored } from "./component-editor-model";
import { aheadByHours } from "../../../lib/test-clock";

const stored: Component = {
  id: "5b1f3c1e-7f0a-4a52-9d57-0c1d2e3f4a5b",
  name: "Payments API",
  version: 3,
  created_at: aheadByHours(-24),
  updated_at: aheadByHours(-24),
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

describe("withStored", () => {
  const cliRow: Component = {
    ...stored,
    definition: {
      hosts: [],
      cli: { kind: "azure", azure: { subscriptions: ["11111111-2222-4333-8444-555555555555"], access: "read" } },
    },
  };

  it("sends a stored CLI card back unchanged, which the editor has no field for", () => {
    const req = withStored(requestOf({ ...draftOf(cliRow), description: "Azure, read only" }), cliRow, "");
    expect(req.definition.cli).toEqual(cliRow.definition.cli);
    expect(req.definition.description).toBe("Azure, read only");
  });

  it("adds nothing to a row with neither a CLI card nor an MCP server", () => {
    expect(withStored(requestOf(draftOf(stored)), stored, "aud")).toEqual(requestOf(draftOf(stored)));
  });
});

describe("description", () => {
  it("keeps a stored description, trims an edited one and sends none when cleared", () => {
    const withDesc = { ...stored, definition: { ...stored.definition, description: "Charges cards." } };
    expect(draftOf(withDesc).description).toBe("Charges cards.");
    expect(requestOf({ ...draftOf(withDesc), description: "  Refunds too.  " }).definition.description).toBe("Refunds too.");
    expect(requestOf({ ...draftOf(withDesc), description: "   " }).definition).not.toHaveProperty("description");
  });
});

describe("request rules", () => {
  it("draftOf and requestOf keep request_rules", () => {
    const rules = [
      { host: "api.pay.example", methods: ["GET"], paths: ["/v1"], effect: "allow" as const },
      { host: "api.pay.example", methods: ["POST"], paths: ["/v1/refunds"], effect: "hold" as const },
    ];
    const withRules: Component = { ...stored, definition: { ...stored.definition, request_rules: rules } };
    expect(draftOf(withRules).requestRules).toEqual(rules);
    expect(requestOf(draftOf(withRules)).definition.request_rules).toEqual(rules);
    // The save path sends the form's rules, not the stored row's: withStored carries none of its own.
    const edited = [{ host: "api.pay.example", effect: "deny" as const }];
    expect(withStored(requestOf({ ...draftOf(withRules), requestRules: edited }), withRules, "").definition.request_rules).toEqual(edited);
    // A draft with no rules, or one that never had the field, sends none.
    expect(requestOf(draftOf(stored)).definition).not.toHaveProperty("request_rules");
    expect(requestOf(draftOf(null)).definition).not.toHaveProperty("request_rules");
  });

  it("removing every rule sends none, though the stored row has them", () => {
    const rules = [{ host: "api.pay.example", effect: "deny" as const }];
    const withRules: Component = { ...stored, definition: { ...stored.definition, request_rules: rules } };
    const sent = withStored(requestOf({ ...draftOf(withRules), requestRules: [] }), withRules, "");
    expect(sent.definition).not.toHaveProperty("request_rules");
    expect(sent.definition.hosts).toEqual(stored.definition.hosts);
  });

  it("names the rule and part a server sentence is about, and nothing for other sentences", () => {
    expect(requestRulesRefusal('invalid component: definition.request_rules[1].paths[0]: path "/v1/../admin" is not in normal form')).toEqual({ index: 1, part: "paths" });
    expect(requestRulesRefusal('request_rules[0].host: "x" is not one of the component\'s hosts')).toEqual({ index: 0, part: "host" });
    expect(requestRulesRefusal('invalid component: definition.request_rules[2].methods[0]: "get" must be uppercase ("GET")')).toEqual({ index: 2, part: "methods" });
    expect(requestRulesRefusal('invalid component: definition.request_rules[3].effect: "x" (effects: allow, deny, hold)')).toEqual({ index: 3, part: "effect" });
    expect(requestRulesRefusal("invalid component: definition.request_rules: at most 64 rules, got 65")).toEqual({});
    expect(requestRulesRefusal("This component carries request rules, and request rules are off on this deployment. An admin turns them on with features.component_request_rules.enabled.")).toEqual({});
    expect(requestRulesRefusal("This component carries request rules, and whether request rules are on could not be read. Try again in a moment.")).toEqual({});
    expect(requestRulesRefusal("Someone saved this component since you opened it. Open it again to see their change, then make your edit again.")).toBeNull();
    expect(requestRulesRefusal("invalid component: definition.hosts: at least one host")).toBeNull();
    expect(requestRulesRefusal("invalid component: definition.secrets[0].delivery.var: bad")).toBeNull();
  });
});
