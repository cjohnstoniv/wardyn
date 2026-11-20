/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { alsoChanges, fieldLabel, reviewSentence } from "./change-review";
import type { ChangeReview, GovernanceChange } from "./types";

function change(review: ChangeReview | undefined, over: Partial<GovernanceChange["diff"]> = {}): GovernanceChange {
  return {
    id: "c1",
    target_kind: "governance_unit",
    op: "update",
    target_key: "u1",
    state: "pending",
    proposed_by: "ana@example.com",
    proposed_at: "2026-01-01T00:00:00Z",
    expires_at: "2026-01-02T00:00:00Z",
    diff: {
      before: { name: "Payments", ceiling: { allowed_domains: ["a.example"], min_confinement_class: "CC1" } },
      after: { name: "Payments", ceiling: { allowed_domains: ["a.example", "b.example", "c.example"], min_confinement_class: "CC2" } },
      changed: ["ceiling.allowed_domains", "ceiling.min_confinement_class"],
      review,
      ...over,
    },
  };
}

const HOSTS: ChangeReview = {
  verdict: "widens",
  widens: [{ field: "allowed_domains", entries: ["b.example", "c.example"] }],
};

describe("fieldLabel", () => {
  it("reads a full path", () => {
    expect(fieldLabel("resources.cpu_millis")).toBe("Limits");
  });
  it("reads a path without its first segment", () => {
    expect(fieldLabel("ceiling.allowed_domains")).toBe("Allowed hosts");
  });
  it("lets the boundary's label win", () => {
    expect(fieldLabel("min_confinement_class")).toBe("Barrier floor");
    expect(fieldLabel("ceiling.min_confinement_class")).toBe("Barrier floor");
  });
  it("returns null for a path it does not know", () => {
    expect(fieldLabel("ceiling.nothing_here")).toBeNull();
    expect(fieldLabel("toString")).toBeNull();
  });
});

describe("reviewSentence", () => {
  it("counts the new hosts and names the other change with the barrier's own label", () => {
    expect(reviewSentence(change(HOSTS))).toBe(
      "This lets runs under Payments reach 2 new hosts. It also changes Barrier floor from Fence (CC1) to Wall (CC2).",
    );
  });

  it("says 1 new host for one entry", () => {
    const one = change({ verdict: "widens", widens: [{ field: "allowed_domains", entries: ["b.example"] }] }, { changed: ["ceiling.allowed_domains"] });
    expect(reviewSentence(one)).toBe("This lets runs under Payments reach 1 new host.");
  });

  it("names every other widened field, joined with commas and and", () => {
    const wide = change(
      {
        verdict: "widens",
        widens: [{ field: "eligible_grants" }, { field: "desktop" }, { field: "workspace_repos" }],
      },
      { changed: ["ceiling.eligible_grants", "ceiling.desktop", "ceiling.workspace_repos"] },
    );
    expect(reviewSentence(wide)).toBe("This widens Credentials, Desktop and Repositories for runs under Payments.");
  });

  it("shows a field it has no name for by its path", () => {
    const wide = change({ verdict: "widens", widens: [{ field: "member_cap", entries: ["cpu_millis"] }] }, { changed: [] });
    expect(reviewSentence(wide)).toBe("This widens member_cap for runs under Payments.");
  });

  it("states at most two other changes", () => {
    const many = change(HOSTS, {
      before: { ceiling: { allowed_domains: ["a"], a: 1, b: 1, c: 1 } },
      after: { ceiling: { allowed_domains: ["a", "b"], a: 2, b: 2, c: 2 } },
      changed: ["ceiling.allowed_domains", "ceiling.a", "ceiling.b", "ceiling.c"],
    });
    expect(reviewSentence(many)?.match(/It also changes/g)).toHaveLength(2);
    expect(alsoChanges(many)).toHaveLength(3);
  });

  it("is null for a held narrowing, an unchecked change and an absent review", () => {
    expect(reviewSentence(change({ verdict: "narrows_held", held_because: ["contact"] }))).toBeNull();
    expect(reviewSentence(change({ verdict: "not_checked" }))).toBeNull();
    expect(reviewSentence(change(undefined))).toBeNull();
  });
});

describe("alsoChanges", () => {
  it("lists the changed paths that are not the widening, with label and both sides", () => {
    expect(alsoChanges(change(HOSTS))).toEqual([
      { path: "ceiling.min_confinement_class", label: "Barrier floor", before: "Fence (CC1)", after: "Wall (CC2)" },
    ]);
  });
  it("shows an unset side as Not set", () => {
    const c = change(HOSTS, { before: {}, changed: ["ceiling.min_confinement_class"] });
    expect(alsoChanges(c)[0].before).toBe("Not set");
  });
});
