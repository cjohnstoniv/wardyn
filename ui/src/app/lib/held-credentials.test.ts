/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { AuditEvent, CredentialGrant } from "./types";
import { heldCredentials } from "./held-credentials";

const grant = (id: string, audience: string, host?: string): CredentialGrant => ({
  id,
  scope: audience,
  audience,
  state: "active",
  host,
});
const mint = (grantID: string, outcome: AuditEvent["outcome"] = "success", extra: Record<string, unknown> = {}): AuditEvent => ({
  id: `m-${grantID}-${outcome}`,
  time: "2026-10-01T00:00:00Z",
  actor_type: "agent",
  actor: "spiffe://x",
  action: "credential.mint",
  outcome,
  data: { grant_id: grantID, ...extra },
});

describe("heldCredentials", () => {
  it("reads kind and host from the grant the mint row names", () => {
    const got = heldCredentials(
      [grant("g1", "github_token"), grant("g2", "git_pat", "dev.azure.com"), grant("g3", "ssh_key", "github.com")],
      [mint("g1"), mint("g2"), mint("g3")],
    );
    expect(got).toEqual({
      readable: true,
      items: [{ kind: "github_token" }, { kind: "git_pat", host: "dev.azure.com" }, { kind: "ssh_key", host: "github.com" }],
    });
  });

  it("api_key-only gives no per-kind item, and an unminted grant gives none either", () => {
    expect(heldCredentials([grant("g1", "api_key"), grant("g2", "git_pat", "h")], [mint("g1")])).toEqual({
      readable: true,
      items: [],
    });
  });

  it("only success rows count, and a repeat mint is one item", () => {
    expect(heldCredentials([grant("g1", "github_token")], [mint("g1", "failure")])).toEqual({ readable: true, items: [] });
    expect(heldCredentials([grant("g1", "github_token")], [mint("g1"), { ...mint("g1"), id: "again" }])).toEqual({
      readable: true,
      items: [{ kind: "github_token" }],
    });
  });

  it("an env_secret grant is held without any mint row", () => {
    expect(heldCredentials([grant("g1", "env_secret")], [])).toEqual({ readable: true, items: [{ kind: "env_secret" }] });
  });

  it("a git token or SSH key whose grant carries no host gives no line rather than an invented one", () => {
    expect(heldCredentials([grant("g1", "git_pat")], [mint("g1")])).toEqual({ readable: true, items: [] });
  });

  it("unreadable when either fetch failed, or a mint names a grant the run does not list", () => {
    expect(heldCredentials(undefined, [])).toEqual({ readable: false });
    expect(heldCredentials([], undefined)).toEqual({ readable: false });
    expect(heldCredentials([grant("g1", "github_token")], [mint("ghost")])).toEqual({ readable: false });
  });

  it("no value, jti or scope reaches the result", () => {
    const got = heldCredentials([grant("g1", "git_pat", "dev.azure.com")], [mint("g1", "success", { jti: "J-SECRET", token: "T-SECRET" })]);
    expect(JSON.stringify(got)).not.toMatch(/SECRET/);
  });
});
