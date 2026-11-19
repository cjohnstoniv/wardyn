/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One case per row of the deciders editor's rules table, and per helper: what the server refuses
// (internal/types/deciders.go) is stopped here first.
import { describe, expect, it } from "vitest";
import type { DeciderRule } from "../../../lib/api/deciders";
import type { GovernanceUnit } from "../../../lib/api/governance";
import {
  MAX_WHO,
  OWN_RULE_KINDS,
  OWNER_REFUSED_KINDS,
  kindBlock,
  kindsHeldElsewhere,
  lockedBy,
  ownerAllowed,
  ruleProblems,
  scopeRowShown,
  secondHumanState,
  toggleKind,
  whoPhrase,
} from "./decider-rules";

const rule = (r: Partial<DeciderRule> = {}): DeciderRule => ({ kinds: [], who: [], ...r });
const elevation = (r: Partial<DeciderRule> = {}): DeciderRule =>
  rule({ kinds: ["elevation"], who: ["unit_admins"], max_elevation: "4h", eligible_requesters: ["group:Eng"], ...r });

describe("constants", () => {
  it("name the own-rule kinds, the owner-refused kinds and the who limit", () => {
    expect(OWN_RULE_KINDS).toEqual(["elevation", "image", "file_download"]);
    expect(OWNER_REFUSED_KINDS).toEqual(["credential", "push_content", "elevation", "image", "file_download"]);
    expect(MAX_WHO).toBe(8);
  });
});

describe("each kind in at most one rule", () => {
  it("shows a kind in rule 2 as 2 from rule 1's view, never its own rule's kinds", () => {
    const draft = [rule({ kinds: ["egress_domain"] }), rule({ kinds: ["mcp_call", "kube_call"] })];
    expect(kindsHeldElsewhere(draft, 0)).toEqual({ mcp_call: 2, kube_call: 2 });
    expect(kindsHeldElsewhere(draft, 1)).toEqual({ egress_domain: 1 });
  });
  it("maps a kind two other rules name to the first", () => {
    const draft = [rule(), rule({ kinds: ["mcp_call"] }), rule({ kinds: ["mcp_call"] })];
    expect(kindsHeldElsewhere(draft, 0)).toEqual({ mcp_call: 2 });
  });
});

describe("kindBlock", () => {
  it("says in_rule for a kind another rule names", () => {
    expect(kindBlock(rule(), "mcp_call", { mcp_call: 3 })).toEqual({ reason: "in_rule", n: 3 });
  });
  it("says not_with for any other kind while an own-rule kind is checked", () => {
    expect(kindBlock(elevation(), "mcp_call", {})).toEqual({ reason: "not_with" });
    expect(kindBlock(rule({ kinds: ["image"] }), "elevation", {})).toEqual({ reason: "not_with" });
  });
  it("says own_rule for an own-rule kind while another kind is checked", () => {
    expect(kindBlock(rule({ kinds: ["mcp_call"] }), "image", {})).toEqual({ reason: "own_rule" });
  });
  it("is null when the kind is on offer, and for the own-rule kind already checked", () => {
    expect(kindBlock(rule({ kinds: ["mcp_call"] }), "kube_call", {})).toBeNull();
    expect(kindBlock(elevation(), "elevation", {})).toBeNull();
  });
});

describe("toggleKind", () => {
  it("clears the other kinds and max_scope when an own-rule kind goes on", () => {
    const next = toggleKind(rule({ kinds: ["mcp_call", "kube_call"], max_scope: "once", second_human: true }), "elevation", true);
    expect(next.kinds).toEqual(["elevation"]);
    expect(next.max_scope).toBeUndefined();
    expect(next.second_human).toBeUndefined();
  });
  it("sets second_human when file_download goes on", () => {
    const next = toggleKind(rule({ kinds: ["credential"], max_scope: "run" }), "file_download", true);
    expect(next.kinds).toEqual(["file_download"]);
    expect(next.second_human).toBe(true);
    expect(next.max_scope).toBeUndefined();
  });
  it("appends any other kind and leaves the rest", () => {
    const next = toggleKind(rule({ kinds: ["credential"], second_human: true, max_scope: "once" }), "push_content", true);
    expect(next).toEqual(rule({ kinds: ["credential", "push_content"], second_human: true, max_scope: "once" }));
  });
  it("drops the elevation-only fields when another kind goes on", () => {
    const was = elevation({ renew_after: "1h", ticket_pattern: "T-\\d+" });
    for (const k of ["image", "mcp_call"] as const) {
      const next = toggleKind({ ...was, kinds: [] }, k, true);
      expect(next.kinds).toEqual([k]);
      expect(next.max_elevation).toBeUndefined();
      expect(next.eligible_requesters).toBeUndefined();
      expect(next.renew_after).toBeUndefined();
      expect(next.ticket_pattern).toBeUndefined();
    }
    expect(toggleKind({ ...was, kinds: [] }, "elevation", true)).toEqual(was);
  });
  it("removes a kind that goes off", () => {
    expect(toggleKind(rule({ kinds: ["mcp_call", "kube_call"] }), "mcp_call", false).kinds).toEqual(["kube_call"]);
  });
});

describe("ownerAllowed", () => {
  it("refuses the owner for credential and push_content with cred", () => {
    expect(ownerAllowed(rule({ kinds: ["credential"] }))).toEqual({ allowed: false, reason: "cred" });
    expect(ownerAllowed(rule({ kinds: ["push_content"] }))).toEqual({ allowed: false, reason: "cred" });
  });
  it("refuses the owner for elevation and image with request", () => {
    expect(ownerAllowed(rule({ kinds: ["elevation"] }))).toEqual({ allowed: false, reason: "request" });
    expect(ownerAllowed(rule({ kinds: ["image"] }))).toEqual({ allowed: false, reason: "request" });
  });
  it("refuses the owner for file_download with download", () => {
    expect(ownerAllowed(rule({ kinds: ["file_download"], second_human: true }))).toEqual({ allowed: false, reason: "download" });
  });
  it("refuses the owner with second when a second person decides", () => {
    expect(ownerAllowed(rule({ kinds: ["mcp_call"], second_human: true }))).toEqual({ allowed: false, reason: "second" });
  });
  it("allows the owner otherwise", () => {
    expect(ownerAllowed(rule({ kinds: ["egress_domain", "mcp_call"] }))).toEqual({ allowed: true });
  });
});

describe("secondHumanState", () => {
  it("hides the row for elevation and image", () => {
    expect(secondHumanState(rule({ kinds: ["elevation"] }))).toBe("hidden");
    expect(secondHumanState(rule({ kinds: ["image"] }))).toBe("hidden");
  });
  it("forces it on for file_download", () => {
    expect(secondHumanState(rule({ kinds: ["file_download"] }))).toBe("forced_download");
  });
  it("forces it on for credential and push_content", () => {
    expect(secondHumanState(rule({ kinds: ["credential"] }))).toBe("forced");
    expect(secondHumanState(rule({ kinds: ["push_content", "mcp_call"] }))).toBe("forced");
  });
  it("is blocked while the run's owner is listed", () => {
    expect(secondHumanState(rule({ kinds: ["mcp_call"], who: ["owner"] }))).toBe("blocked_by_owner");
  });
  it("is free otherwise", () => {
    expect(secondHumanState(rule({ kinds: ["mcp_call"], who: ["unit_admins"] }))).toBe("free");
  });
});

describe("scopeRowShown", () => {
  it("is false for each own-rule kind", () => {
    for (const k of OWN_RULE_KINDS) expect(scopeRowShown(rule({ kinds: [k] }))).toBe(false);
  });
  it("is true for the other kinds", () => {
    expect(scopeRowShown(rule({ kinds: ["egress_domain", "credential"] }))).toBe(true);
    expect(scopeRowShown(rule())).toBe(true);
  });
});

describe("ruleProblems", () => {
  it("flags a longest elevation under 15 minutes", () => {
    expect(ruleProblems(elevation({ max_elevation: "14m" }))).toEqual(["elev_max"]);
  });
  it("accepts 15 minutes and 24 hours", () => {
    expect(ruleProblems(elevation({ max_elevation: "15m" }))).toEqual([]);
    expect(ruleProblems(elevation({ max_elevation: "24h" }))).toEqual([]);
  });
  it("flags 24 hours and a minute", () => {
    expect(ruleProblems(elevation({ max_elevation: "24h1m" }))).toEqual(["elev_max"]);
  });
  it("flags an absent or unreadable longest elevation", () => {
    expect(ruleProblems(elevation({ max_elevation: undefined }))).toEqual(["elev_max"]);
    expect(ruleProblems(elevation({ max_elevation: "soon" }))).toEqual(["elev_max"]);
  });
  it("flags empty or absent eligible_requesters", () => {
    expect(ruleProblems(elevation({ eligible_requesters: [] }))).toEqual(["ask_required"]);
    expect(ruleProblems(elevation({ eligible_requesters: undefined }))).toEqual(["ask_required"]);
  });
  it("flags renew_after over 168 hours, and accepts 0s and 168h", () => {
    expect(ruleProblems(elevation({ renew_after: "169h" }))).toEqual(["renew"]);
    expect(ruleProblems(elevation({ renew_after: "168h" }))).toEqual([]);
    expect(ruleProblems(elevation({ renew_after: "0s" }))).toEqual([]);
  });
  it("lists every problem in order", () => {
    expect(ruleProblems(rule({ kinds: ["elevation"], renew_after: "200h" }))).toEqual(["elev_max", "ask_required", "renew"]);
  });
  it("gives [] for a rule that is not an elevation rule", () => {
    expect(ruleProblems(rule({ kinds: ["mcp_call"], max_elevation: "1m" }))).toEqual([]);
  });
});

describe("lockedBy", () => {
  const unit = (id: string, parent_id?: string, locked_knobs?: string[]): GovernanceUnit =>
    ({ id, name: id.toUpperCase(), parent_id, locked_knobs }) as unknown as GovernanceUnit;

  it("finds a locking parent", () => {
    const profiles = [unit("a", undefined, ["deciders"]), unit("b", "a")];
    expect(lockedBy(profiles[1], profiles)?.id).toBe("a");
  });
  it("finds the nearest locking grandparent, passing unlocked units and other knobs", () => {
    const profiles = [unit("a", undefined, ["deciders"]), unit("b", "a", ["limits"]), unit("c", "b")];
    expect(lockedBy(profiles[2], profiles)?.id).toBe("a");
  });
  it("prefers the nearest of two locking ancestors", () => {
    const profiles = [unit("a", undefined, ["deciders"]), unit("b", "a", ["deciders"]), unit("c", "b")];
    expect(lockedBy(profiles[2], profiles)?.id).toBe("b");
  });
  it("is null when no ancestor locks, and for a unit that only locks itself", () => {
    const profiles = [unit("a", undefined, ["deciders"]), unit("b", "x")];
    expect(lockedBy(profiles[0], profiles)).toBeNull();
    expect(lockedBy(profiles[1], profiles)).toBeNull();
  });
  it("stops on a cycle", () => {
    const profiles = [unit("a", "b"), unit("b", "a")];
    expect(lockedBy(profiles[0], profiles)).toBeNull();
  });
});

describe("whoPhrase", () => {
  it("writes one entry", () => {
    expect(whoPhrase(["owner"])).toBe("the run's owner");
  });
  it("joins two with or", () => {
    expect(whoPhrase(["owner", "unit_admins"])).toBe("the run's owner or admins of this unit");
  });
  it("joins three with commas and or, and keeps a group name's case", () => {
    expect(whoPhrase(["owner", "unit_admins", "group:Platform-SRE"])).toBe(
      "the run's owner, admins of this unit or group Platform-SRE",
    );
  });
  it("shows an unknown entry as it is", () => {
    expect(whoPhrase(["Manager"])).toBe("Manager");
  });
});
