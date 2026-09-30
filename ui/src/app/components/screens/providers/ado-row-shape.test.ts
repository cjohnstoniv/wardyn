/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The valid Azure DevOps row shapes (#1429, #1428): which lane a row must be
// saved in follows its addresses, and only settled addresses reshape it.
import { describe, it, expect } from "vitest";
import type { GitProvider } from "../../../lib/api/providers";
import { isADOServerRow, newADORow, reshapeADORow } from "./ado-row-shape";

const services = (over: Partial<GitProvider> = {}): GitProvider => ({ ...newADORow("azure_devops"), base_urls: ["https://dev.azure.com/acme"], ...over });

describe("newADORow", () => {
  it("is the Services shape the server accepts: entra lane, per person, a token per run, reads only", () => {
    const r = newADORow("azure_devops");
    expect(r).toEqual({
      id: "azure_devops",
      kind: "azure_devops",
      base_urls: ["https://dev.azure.com/"],
      lanes: ["entra"],
      credential_source: "per_user",
      entra: { tenant_id: "", client_id: "", token_mode: "minted_pat", capability_ceiling: ["project_read", "code_read"], default_profile: [] },
    });
  });
});

describe("reshapeADORow", () => {
  it("leaves a settled Services row alone (the same object)", () => {
    const r = services();
    expect(reshapeADORow(r)).toBe(r);
  });
  it("makes a settled Server address a git-only per-person token row and drops the Entra block", () => {
    const out = reshapeADORow(services({ base_urls: ["https://tfs.corp.example/acme"] }));
    expect(out.lanes).toEqual(["pat"]);
    expect(out.credential_source).toBe("per_user");
    expect("entra" in out).toBe(false);
  });
  it("leaves a settled Server row alone", () => {
    const { entra: _e, ...rest } = services();
    void _e;
    const r: GitProvider = { ...rest, base_urls: ["https://tfs.corp.example/acme"], lanes: ["pat"], credential_source: "per_user" };
    expect(reshapeADORow(r)).toBe(r);
  });
  it("a row with any Server address is a Server row, as the server reads it", () => {
    expect(isADOServerRow(services({ base_urls: ["https://dev.azure.com/acme", "https://tfs.corp.example/acme"] }))).toBe(true);
    expect(isADOServerRow(services({ base_urls: ["https://dev.azure.com/acme", "https://contoso.visualstudio.com/x"] }))).toBe(false);
  });
  it("restores the Services shape when a Server row's addresses are all Services again", () => {
    const { entra: _e, ...rest } = services();
    void _e;
    const out = reshapeADORow({ ...rest, lanes: ["pat"] });
    expect(out.lanes).toEqual(["entra"]);
    expect(out.entra?.token_mode).toBe("minted_pat");
  });
  it("leaves an unsettled row alone: an invalid or empty address, or a row that is not Azure DevOps", () => {
    const half = services({ base_urls: ["https://dev.azure.com/"] });
    expect(reshapeADORow(half)).toBe(half);
    const none = services({ base_urls: [] });
    expect(reshapeADORow(none)).toBe(none);
    const gh: GitProvider = { id: "gh", kind: "github", base_urls: ["https://github.com/acme"] };
    expect(reshapeADORow(gh)).toBe(gh);
  });
});
