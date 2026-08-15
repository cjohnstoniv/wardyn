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
  integrationTypeById,
  typesInGroup,
  searchIntegrationTypes,
  integrationGroup,
} from "./integration-catalog";
import { RESIDENCY_META } from "./integrations";

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
      expect(RESIDENCY_META[t.delivery], `${t.id} has delivery meta`).toBeDefined();
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

  // Every remaining catalog type has a real credential lane — the ten
  // "notbuilt" rows (cloud providers, data stores, ECR, GAR) and the one
  // unsupported row (a self-hosted endpoint) were deleted with the
  // base-component rebuild: a generic kind is base-only (secret + egress),
  // so a type with no header lane has no home in the catalog any more.
  it("carries no dead 'notbuilt' or 'unsupported' rows", () => {
    for (const t of INTEGRATION_TYPES) {
      expect(t.delivery, `${t.id}`).not.toBe("notbuilt");
    }
    expect(typesInGroup("cloud")).toHaveLength(0);
    expect(typesInGroup("data")).toHaveLength(0);
  });

  // A generic-lane type is written straight through PUT /integrations, so it
  // MUST carry the wire type. The two typed categories have their own flows.
  it("gives every generic-lane type an apiType, and never offers a broken Add", () => {
    for (const t of INTEGRATION_TYPES) {
      if (t.addLane === "generic") {
        expect(t.apiType, `${t.id} is written through the generic flow`).toBeTruthy();
      }
    }
    // The model and scm sections are the TYPED backend categories.
    for (const t of typesInGroup("scm")) expect(t.addLane).toBe("scm");
    for (const t of typesInGroup("model")) expect(t.addLane).toBe("ai");
  });

  it("finds a type by product name, by host, and by what people actually type", () => {
    expect(searchIntegrationTypes("jfrog").map((t) => t.id)).toContain("artifactory");
    expect(searchIntegrationTypes("ghcr.io").map((t) => t.id)).toContain("ghcr");
    expect(searchIntegrationTypes("model context protocol").map((t) => t.id)).toContain("mcp");
    // Anything unmatched falls to the generic escape hatch, not an error.
    expect(searchIntegrationTypes("zzzznope")).toHaveLength(0);
    expect(integrationTypeById("other")?.addLane).toBe("generic");
    // Capped at the mock's own cut-off.
    expect(searchIntegrationTypes("a").length).toBeLessThanOrEqual(7);
  });

  // W11-S1-1: "Other service"'s lead promised the credential "works today" for
  // ANY host — but the proxy only ever reads plaintext to inject it on plain
  // HTTP or inside a TLS-MITM'd tunnel, and MITM never opens for a generic
  // integration's own egress (internal/api/integrations_run.go's HONEST
  // CEILING doc; internal/egress/proxy/mitm.go's isMITMHost). https is the
  // whole shipped catalog, so the old claim was false for virtually every row.
  it("'Other service' no longer over-promises the credential 'works today' on an unlisted https host", () => {
    const other = integrationTypeById("other");
    expect(other?.lead).toBeDefined();
    expect(other!.lead!.toLowerCase()).not.toContain("works today");
    expect(other!.lead!).toMatch(/https?/i);
    expect(other!.lead!.toLowerCase()).toMatch(/mitm|opaque/);
  });
});
