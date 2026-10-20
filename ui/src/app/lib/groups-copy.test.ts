// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from "vitest";
import { GROUPS_READ_AT_SIGNIN } from "./groups-copy";
import { GOVERNANCE } from "./governance-copy";
import { DRIVES } from "./user-drives-copy";
import { PERM } from "./permissions-copy";

// #3060: the groups sign-in caveat has one source, shown by Governance, User drives and Permissions.
describe("groups-copy: one sign-in caveat", () => {
  it("is the sentence all three screens show", () => {
    expect(GOVERNANCE.SIGNIN_NOTE).toBe(GROUPS_READ_AT_SIGNIN);
    expect(DRIVES.SIGNIN_NOTE).toBe(GROUPS_READ_AT_SIGNIN);
    expect(PERM.SNAPSHOT_BODY).toBe(GROUPS_READ_AT_SIGNIN);
  });
});
