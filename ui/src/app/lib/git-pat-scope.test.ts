/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { gitPATGrants, patAxisError, readPATScope } from "./git-pat-scope";

describe("patAxisError", () => {
  it.each([
    ['eligible_grants[0]: git_pat scope invalid: git_pat scope access "x" is not one of read, write', 0, "access"],
    ['eligible_grants[2]: git_pat scope invalid: git_pat scope forge "x" is not one of generic', 2, "forge"],
    ['eligible_grants[1]: git_pat scope invalid: git_pat scope repos entry "a/.." is malformed', 1, "repos"],
    ["eligible_grants[0]: git_pat scope invalid: git_pat scope api: true is not available on the generic forge (set forge)", 0, "api"],
    ["eligible_grants[3]: git_pat scope api: true on forge bitbucket_server needs a flag", 3, "api"],
    ['eligible grant "git_pat": git_pat repos entry "a/b" is outside the ceiling', null, null],
  ])("%s", (message, index, axis) => {
    const e = patAxisError(message);
    if (axis === null) expect(e).toBeNull();
    else expect(e).toMatchObject({ index, axis });
  });

  it("returns the sentence without the grant prefix", () => {
    expect(patAxisError('eligible_grants[0]: git_pat scope invalid: git_pat scope access "x" is not one of read, write')?.sentence).toBe(
      'git_pat scope invalid: git_pat scope access "x" is not one of read, write',
    );
  });

  it("names no axis for an unknown key, a duplicate-host refusal or an Azure DevOps host", () => {
    expect(patAxisError('eligible_grants[0]: git_pat scope invalid: json: unknown field "repo"')).toBeNull();
    expect(
      patAxisError("eligible_grants[0] and eligible_grants[1]: two git_pat grants for host \"h\" where one sets repos, access read or api"),
    ).toBeNull();
    expect(
      patAxisError('eligible_grants[0]: git_pat scope sets repos, access, api or forge for the Azure DevOps host "dev.azure.com"'),
    ).toBeNull();
    expect(patAxisError(null)).toBeNull();
  });
});

describe("readPATScope / gitPATGrants", () => {
  it("keeps absent repos distinct from an empty list", () => {
    expect(readPATScope({ host: "h", secret_name: "s" }).repos).toBeUndefined();
    expect(readPATScope({ host: "h", secret_name: "s", repos: [] }).repos).toEqual([]);
  });

  it("lists git_pat grants with their eligible_grants index, tolerating a malformed list", () => {
    const grants = [{ kind: "api_key" }, null, { kind: "git_pat", scope: { host: "h" } }];
    expect(gitPATGrants(grants).map((g) => g.index)).toEqual([2]);
    expect(gitPATGrants("nope")).toEqual([]);
    expect(gitPATGrants(undefined)).toEqual([]);
  });
});
