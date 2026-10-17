/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { HttpError } from "./api/core";
import {
  entryErrorMessage,
  mayEnterRun,
  RUN_OWNER_ONLY,
  runEntryOwnerLine,
  runEntryRefusalLine,
} from "./run-entry";

// #1476: entry is owner-gated. An admin keeps entry only to a run no person
// owns; on another person's run they keep Kill/Approve/Policy and lose entry.
describe("mayEnterRun", () => {
  const person = { created_by: "priya@acme.io" };
  const service = { created_by: "svc", operator_owned: true };

  it("the run's own person may, admin or not", () => {
    expect(mayEnterRun(person, "priya@acme.io", false)).toBe(true);
    expect(mayEnterRun(person, "priya@acme.io", true)).toBe(true);
  });

  it("a super admin may NOT enter another person's run", () => {
    expect(mayEnterRun(person, "sam@acme.io", true)).toBe(false);
  });

  it("a super admin keeps entry to an operator-owned run; a non-admin does not", () => {
    expect(mayEnterRun(service, "sam@acme.io", true)).toBe(true);
    expect(mayEnterRun(service, "sam@acme.io", false)).toBe(false);
  });

  it("an unknown principal or creator is never ownership", () => {
    expect(mayEnterRun(person, "", true)).toBe(false);
    expect(mayEnterRun(person, null, true)).toBe(false);
    expect(mayEnterRun({}, "", false)).toBe(false);
  });
});

describe("refusal wording", () => {
  it("a person's run names the person; a run no person owns keeps the admin-role line", () => {
    expect(runEntryRefusalLine({ created_by: "priya@acme.io" })).toBe(
      "Only priya@acme.io can open this run's terminal, apps and SSH.",
    );
    expect(runEntryOwnerLine("priya@acme.io")).toBe("Only priya@acme.io can open this run's terminal, apps and SSH.");
    expect(runEntryRefusalLine({ created_by: "svc", operator_owned: true })).toBe("Requires the admin role.");
    expect(runEntryRefusalLine({})).toBe("Requires the admin role.");
  });

  it("run_owner_only maps from the reason, never from the server's lowercase text", () => {
    const wire = new HttpError(403, "only the person who started this run can open it interactively", "run_owner_only");
    expect(entryErrorMessage(wire, () => "fallback")).toBe(RUN_OWNER_ONLY);
    expect(RUN_OWNER_ONLY).toBe("Only the person who started this run can open it interactively.");
    expect(entryErrorMessage(new HttpError(403, "nope", "other"), (e) => (e as Error).message)).toBe("nope");
    expect(entryErrorMessage(new Error("x"), () => "fallback")).toBe("fallback");
  });
});
