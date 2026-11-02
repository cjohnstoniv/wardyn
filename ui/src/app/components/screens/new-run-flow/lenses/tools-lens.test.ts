/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { SCENARIO_ACCESS } from "../../../../lib/new-run-fixtures/scenarios/tools";
import type { AccessFacts, AccessHost } from "../../../../lib/types/new-run-access";
import type { RunIntent } from "../draft/run-intent";
import { addAllowedHost, narrowsEgress, setNarrowEgress, wallsLeaveOut, wallsLeaveOutKnobs } from "./tools-lens";

const START = { task: "t" } as unknown as RunIntent;

describe("Only the sites listed (T4-b)", () => {
  it("turns on as overrides.egress.allow_all_egress: false, beside the hosts already added", () => {
    const withHost = addAllowedHost(START, "pypi.org");
    const on = setNarrowEgress(withHost, true);
    expect(on.overrides).toEqual({ hosts: { add_allowed: ["pypi.org"] }, egress: { allow_all_egress: false } });
    expect(narrowsEgress(on)).toBe(true);
    expect(narrowsEgress(withHost)).toBe(false);
  });

  it("keeps the other egress choice and prunes what turning off empties", () => {
    const both = { ...START, overrides: { egress: { first_use_approval: "always_deny", allow_all_egress: false } } } as RunIntent;
    expect(setNarrowEgress(both, false).overrides).toEqual({ egress: { first_use_approval: "always_deny" } });
    expect(setNarrowEgress(setNarrowEgress(START, true), false).overrides).toBeUndefined();
  });
});

const host = (h: string, verdict: string): AccessHost => ({ ...SCENARIO_ACCESS.member.hosts[0], host: h, verdict });
const access = (...hosts: AccessHost[]): AccessFacts => ({ ...SCENARIO_ACCESS.member, hosts });

describe("wallsLeaveOut", () => {
  const hosts = access(host("pypi.org", "not_allowed"), host("api.example.com", "allowed"), host("paste.example.net", "not_allowed"), host("sub.docs.example.org", "not_allowed"));

  it("names a not-allowed host outside the walls' list, or inside their denies, and no other", () => {
    const walls = { allowed_domains: ["api.example.com", "*.docs.example.org"], denied_domains: ["paste.example.net"] };
    expect(wallsLeaveOut(walls, hosts, true)).toEqual(["pypi.org", "paste.example.net"]);
  });

  it("says nothing for walls with no list, before the server has answered, or with no walls", () => {
    expect(wallsLeaveOut({ first_use_approval: "always_deny" }, hosts, true)).toEqual([]);
    expect(wallsLeaveOut({ allowed_domains: [] }, hosts, false)).toEqual([]);
    expect(wallsLeaveOut(undefined, hosts, true)).toEqual([]);
  });

  it("names the walls' list that takes each host out, so a line for one list does not stand for the other", () => {
    expect(wallsLeaveOutKnobs({ denied_domains: ["paste.example.net"] }, hosts, true)).toEqual(["denied_domains"]);
    expect(wallsLeaveOutKnobs({ allowed_domains: ["api.example.com", "paste.example.net", "*.docs.example.org"] }, hosts, true)).toEqual(["allowed_domains"]);
    expect(wallsLeaveOutKnobs({ allowed_domains: ["api.example.com"], denied_domains: ["paste.example.net"] }, hosts, true)).toEqual(["allowed_domains", "denied_domains"]);
    expect(wallsLeaveOutKnobs(undefined, hosts, true)).toEqual([]);
  });

  it("an empty list leaves out every host the server refuses, none it allows", () => {
    expect(wallsLeaveOut({ allowed_domains: [] }, hosts, true)).toEqual(["pypi.org", "paste.example.net", "sub.docs.example.org"]);
  });

  // The server's rule (internal/composer/meet_domains.go covers, domainmatch.MatchWild): `*.example.org` matches subdomains only, never `example.org`.
  it("a wildcard covers its subdomains and not its bare domain", () => {
    expect(wallsLeaveOut({ allowed_domains: ["*.example.org"] }, access(host("example.org", "not_allowed")), true)).toEqual(["example.org"]);
    expect(wallsLeaveOut({ allowed_domains: ["*.example.org"] }, access(host("a.example.org", "not_allowed")), true)).toEqual([]);
    expect(wallsLeaveOut({ denied_domains: ["*.example.org"] }, access(host("example.org", "not_allowed")), true)).toEqual([]);
  });

  it("compares host and entry lower-cased", () => {
    expect(wallsLeaveOut({ denied_domains: ["Paste.Example.NET"] }, access(host("paste.example.net", "not_allowed")), true)).toEqual(["paste.example.net"]);
    expect(wallsLeaveOut({ allowed_domains: ["*.Example.ORG"] }, access(host("A.example.org", "not_allowed")), true)).toEqual([]);
  });
});
