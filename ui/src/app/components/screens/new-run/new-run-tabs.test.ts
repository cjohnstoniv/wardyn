/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { NEW_RUN_FIXTURES } from "../../../lib/new-run-fixtures";
import { emptyRunContractDraft } from "../../../lib/run-contract-draft";
import { NEW_RUN_REASON } from "../../../lib/new-run-refusals";
import { fieldsOfTab, NEW_RUN_TABS, TAB_OF_CONTRACT, TAB_OF_FIELD } from "./new-run-tabs";
import { initialWizardState } from "./wizard-types";

describe("New Run tabs", () => {
  it("are Info, Workspaces, Runner, Access, Policy, in that order", () => {
    expect([...NEW_RUN_TABS]).toEqual(["info", "workspaces", "runner", "access", "policy"]);
  });

  it("own every form field the wizard holds, exactly once", () => {
    const held = Object.keys(initialWizardState("CC1")).filter((k) => k !== "contract");
    expect(held.filter((k) => !(k in TAB_OF_FIELD))).toEqual([]);
    const all = NEW_RUN_TABS.flatMap((t) => fieldsOfTab(t));
    expect(new Set(all).size).toBe(all.length);
    expect(all.sort()).toEqual(Object.keys(TAB_OF_FIELD).sort());
    expect(Object.keys(TAB_OF_CONTRACT).sort()).toEqual(Object.keys(emptyRunContractDraft()).sort());
  });

  it("put the drive under Workspaces, and lifetime, image, CPU and memory under Runner", () => {
    expect(TAB_OF_FIELD.driveEnabled).toBe("workspaces");
    expect(TAB_OF_FIELD.driveReadOnly).toBe("workspaces");
    for (const k of ["image", "confinementClass", "lifecycle", "autoStopMinutes"] as const) expect(TAB_OF_FIELD[k]).toBe("runner");
    expect(TAB_OF_CONTRACT.runner).toBe("runner");
    expect(TAB_OF_CONTRACT.access).toBe("access");
  });

  it("place every fixture in its tab", () => {
    const tabOfPrefix: Record<string, string> = { info: "info", workspaces: "workspaces", run: "runner", runner: "runner", access: "access", policy: "policy" };
    for (const f of NEW_RUN_FIXTURES) {
      const tab = f.refusal?.reason === NEW_RUN_REASON.IMAGE_CONFLICT ? "runner" : tabOfPrefix[f.route.split("/")[0]];
      expect(f.tab, f.route).toBe(tab);
    }
  });
});
