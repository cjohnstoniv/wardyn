/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { RunIntent } from "../draft/run-intent";
import { deleteMachine, keepMachine, nameKeptMachine, resumeMachine, startFresh } from "./machine-lens";
import { chooseMachineKind, chooseMode } from "./run-lens";
import { chooseShape } from "./shape-lens";

const MAC: RunIntent = { shape: "agent_task", experience: "interactive", machine_kind: "macos_vm" };

describe("machine lens", () => {
  it("keeps, names and deletes: an empty name is no name, and delete is the absent key", () => {
    const kept = keepMachine(MAC);
    expect(kept.machine).toEqual({ keep: true });
    expect(nameKeptMachine(kept, "dev").machine).toEqual({ keep: true, name: "dev" });
    expect(keepMachine(nameKeptMachine(kept, "dev")).machine).toEqual({ keep: true, name: "dev" });
    expect(nameKeptMachine(nameKeptMachine(kept, "dev"), "  ").machine).toEqual({ keep: true });
    expect("machine" in deleteMachine(kept)).toBe(false);
  });

  it("resumes by id, and a resumed machine keeps its own base, so a chosen base goes", () => {
    const chosen = { ...MAC, macos_base: "Xcode 26" };
    expect(resumeMachine(chosen, "m-1")).toMatchObject({ machine: { resume: "m-1" } });
    expect("macos_base" in resumeMachine(chosen, "m-1")).toBe(false);
  });

  it("starts fresh only for the machine it was ticked on, and only while resuming", () => {
    const fresh = startFresh(resumeMachine(MAC, "m-1"), true);
    expect(fresh.machine).toEqual({ resume: "m-1", fresh: true });
    expect(resumeMachine(fresh, "m-1").machine).toEqual({ resume: "m-1", fresh: true });
    expect(resumeMachine(fresh, "m-2").machine).toEqual({ resume: "m-2" });
    expect(startFresh(fresh, false).machine).toEqual({ resume: "m-1" });
    expect(startFresh(keepMachine(MAC), true).machine).toEqual({ keep: true });
  });

  it("goes with the macOS VM and with the interactive mode", () => {
    const kept = keepMachine(MAC);
    expect("machine" in chooseMachineKind(kept, "container")).toBe(false);
    expect("machine" in chooseMachineKind(kept, "linux_vm")).toBe(false);
    expect(chooseMachineKind(kept, "macos_vm").machine).toEqual({ keep: true });
    expect("machine" in chooseMode(kept, "autonomous")).toBe(false);
    expect(chooseMode(kept, "interactive").machine).toEqual({ keep: true });
  });

  it("goes with a background shape, and stays with an interactive one or one whose mode is not chosen yet", () => {
    const kept = { ...keepMachine(MAC), shape: "environment" as const };
    expect("machine" in chooseShape(kept, "application")).toBe(false);
    expect(chooseShape(kept, "agent_task").machine).toEqual({ keep: true });
    expect(chooseShape(kept, "environment")).toBe(kept);
    expect(chooseShape({ ...kept, shape: "application" }, "environment").machine).toEqual({ keep: true });
  });
});
