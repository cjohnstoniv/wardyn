/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The catalog is derived from the approved mock, so these tests pin the
// PROPERTIES that derivation has to preserve — the ones a future hand-edit
// would quietly break — rather than restating all 40 rows.
import { describe, it, expect } from "vitest";
import {
  INTEGRATION_GROUPS,
  INTEGRATION_TYPES,
  DELIVERY_META,
  CATALOG_COPY,
  integrationTypeById,
  typesInGroup,
  searchIntegrationTypes,
  integrationGroup,
} from "./integration-catalog";

describe("integration catalog", () => {
  it("carries all ten sections, each mapped to a real wire category", () => {
    expect(INTEGRATION_GROUPS).toHaveLength(10);
    const ids = INTEGRATION_GROUPS.map((g) => g.id);
    expect(ids).toEqual(["model", "scm", "pkg", "registry", "cloud", "data", "mcp", "work", "obs", "other"]);
    // Every group maps to a DISTINCT category — two sections sharing one
    // category would make a written row unroundtrippable to its section.
    expect(new Set(INTEGRATION_GROUPS.map((g) => g.category)).size).toBe(10);
  });

  it("has unique type ids, each in a real group with a stated delivery mode", () => {
    const ids = INTEGRATION_TYPES.map((t) => t.id);
    expect(new Set(ids).size).toBe(ids.length);
    for (const t of INTEGRATION_TYPES) {
      expect(integrationGroup(t.group).id, `${t.id} names a real group`).toBe(t.group);
      expect(DELIVERY_META[t.delivery], `${t.id} has delivery meta`).toBeDefined();
      expect(t.powers.length, `${t.id} says what it powers`).toBeGreaterThan(0);
    }
  });

  // The one transformation the derivation performs: the mock's single
  // "Authorization: Bearer" string becomes a header NAME plus a Format the
  // secret substitutes into. A format that doesn't carry exactly one %s would
  // either drop the credential or render a literal verb onto the wire.
  it("splits every credential header into a valid name and a single-%s format", () => {
    for (const t of INTEGRATION_TYPES) {
      if (!t.header) {
        expect(t.format, `${t.id} has no header, so no format`).toBeUndefined();
        continue;
      }
      expect(t.header, `${t.id} header is a bare field name`).not.toMatch(/[:\s]/);
      expect(t.format, `${t.id} format`).toBeDefined();
      expect(t.format!.match(/%s/g), `${t.id} format has exactly one %s`).toHaveLength(1);
      expect(t.format, `${t.id} format has no other verb`).not.toMatch(/%[^s]/);
    }
  });

  // Cloud providers and data stores authenticate outside HTTP, so the proxy has
  // nothing to inject. They must stay honest rather than showing an empty
  // credential field — the fact is the design, not a gap to paper over.
  it("keeps cloud and data stores on 'egress only' with a stated reason", () => {
    for (const t of [...typesInGroup("cloud"), ...typesInGroup("data")]) {
      expect(t.delivery, `${t.id}`).toBe("notbuilt");
      expect(t.why, `${t.id} states why`).toBeTruthy();
      expect(t.header, `${t.id} claims no header lane`).toBeUndefined();
    }
    expect(DELIVERY_META.notbuilt.label).toBe("egress only");
    expect(DELIVERY_META.notbuilt.tone).toBe("warning");
  });

  // A generic-lane type is written straight through PUT /integrations, so it
  // MUST carry the wire type. The two typed categories have their own flows.
  it("gives every generic-lane type an apiType, and never offers a broken Add", () => {
    for (const t of INTEGRATION_TYPES) {
      if (t.addLane === "generic") {
        expect(t.apiType, `${t.id} is written through the generic flow`).toBeTruthy();
      }
      if (t.addLane === "unsupported") {
        expect(t.note, `${t.id} says why it can't be added yet`).toBeTruthy();
      }
    }
    // The model and scm sections are the TYPED backend categories.
    for (const t of typesInGroup("scm")) expect(t.addLane).toBe("scm");
    for (const t of typesInGroup("model")) expect(["ai", "unsupported"]).toContain(t.addLane);
  });

  it("finds a type by product name, by host, and by what people actually type", () => {
    expect(searchIntegrationTypes("jfrog").map((t) => t.id)).toContain("artifactory");
    expect(searchIntegrationTypes("ghcr.io").map((t) => t.id)).toContain("ghcr");
    expect(searchIntegrationTypes("psql").map((t) => t.id)).toContain("postgres");
    expect(searchIntegrationTypes("ollama").map((t) => t.id)).toContain("compat");
    expect(searchIntegrationTypes("model context protocol").map((t) => t.id)).toContain("mcp");
    // Anything unmatched falls to the generic escape hatch, not an error.
    expect(searchIntegrationTypes("zzzznope")).toHaveLength(0);
    expect(integrationTypeById("other")?.addLane).toBe("generic");
    // Capped at the mock's own cut-off.
    expect(searchIntegrationTypes("a").length).toBeLessThanOrEqual(7);
  });

  it("keeps the copy canon that states what an integration is", () => {
    expect(CATALOG_COPY.LEDE).toContain("Named connections to the systems outside Wardyn");
    expect(CATALOG_COPY.MOUNT_CAVEAT).toContain("not automatically secret-free");
    // The topology line: the rule that keeps Corporate network and this page apart.
    expect(CATALOG_COPY.CORP_POINTER).toContain("network topology");
    // No test-connect, and one named exception.
    expect(CATALOG_COPY.FOOTNOTE).toContain("doesn't test-connect");
  });
});
