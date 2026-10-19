/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { HttpError } from "./api/core";
import {
  entryErrorMessage,
  mayEnterRun,
  mayEnterRunOrUnknown,
  RUN_OWNER_ONLY,
  SERVICE_ATTACH_LINE,
  SERVICE_ENTRY_LINE,
  sponsorOnlyEntry,
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

// A service identity's run is entered by its sponsor under service_attach, a grant the console cannot see: the
// server's ticket mint decides, so the console must not refuse in the sponsor's place.
describe("a service identity's run", () => {
  const svc = { created_by: "service:ci-bot" };

  it("is left to the server for any resolved viewer, and still refused to nobody by the console", () => {
    expect(mayEnterRun(svc, "sam@acme.io", false)).toBe(false);
    expect(mayEnterRunOrUnknown(svc, "sam@acme.io", false, true)).toBe(true);
    expect(mayEnterRunOrUnknown({ created_by: "priya@acme.io" }, "sam@acme.io", false, true)).toBe(false);
  });

  it("names the sponsor arm only for a viewer the owner arm does not already admit", () => {
    expect(sponsorOnlyEntry(svc, "sam@acme.io", false)).toBe(true);
    expect(sponsorOnlyEntry({ ...svc, operator_owned: true }, "sam@acme.io", true)).toBe(false);
    expect(sponsorOnlyEntry({ created_by: "priya@acme.io" }, "sam@acme.io", false)).toBe(false);
  });

  it("never names the service principal as the person who may enter", () => {
    expect(runEntryRefusalLine(svc)).toBe(SERVICE_ENTRY_LINE);
    expect(runEntryRefusalLine(svc)).not.toMatch(/service:ci-bot/);
  });

  it("maps the sponsor arm's 404 to the service_attach line, not the raw text", () => {
    const wire = new HttpError(404, "run not found", "run_not_found");
    expect(entryErrorMessage(wire, () => "fallback", svc)).toBe(SERVICE_ATTACH_LINE);
    expect(entryErrorMessage(new HttpError(404, "run not found"), () => "fallback", svc)).toBe("fallback");
    // A person's run has no sponsor arm: the same 404 is a missing run, not a feature to ask an admin for.
    expect(entryErrorMessage(wire, () => "fallback", { created_by: "priya@acme.io" })).toBe("fallback");
    expect(entryErrorMessage(wire, () => "fallback")).toBe("fallback");
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
