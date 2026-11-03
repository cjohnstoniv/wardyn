/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// KM-2: Review's lines for the machine a run keeps or resumes, in Where it runs.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { SCENARIO_PREFLIGHT } from "../../../../../lib/new-run-fixtures/scenarios/preflight";
import { shortDate } from "../../../../../lib/format";
import { aheadByHours } from "../../../../../lib/test-clock";
import type { KeptMachine } from "../../../../../lib/types";
import { gbOf, KEEP_NEW_RUN, KEEP_REVIEW as T } from "../../../../wardyn/copy/keep-machine";
import { initialDraft, type RunIntent } from "../../draft/run-intent";
import { KeptMachinesContext, type KeptMachinesRead } from "../../steps/run/kept-machines-provider";
import { MacOSOfferContext, type MacOSOffer } from "../../steps/run/macos-offer";
import { WhereGroup } from "./where-group";

afterEach(cleanup);

const GB = 1024 ** 3;
const MAC = "Maya's MacBook";
const OFFER: MacOSOffer = { ok: true, guestOS: "macOS 26.1", minCPUs: 4, minMemoryMiB: 8192, harnesses: ["none"], runner: MAC, poolScope: "user" };
const BASE: RunIntent = { shape: "agent_task", experience: "interactive", machine_kind: "macos_vm" };
const dev = (over: Partial<KeptMachine> = {}): KeptMachine => ({
  id: "m-dev", name: "dev", owner_subject: "me", runner_id: "r1", base: "Standard", base_prepared_at: aheadByHours(-500), state: "kept", scrubbed: true,
  disk_bytes: 12 * GB, created_at: aheadByHours(-600), last_used_at: aheadByHours(-48), expires_at: aheadByHours(24 * 28), runner_name: MAC, ...over,
});
const read = (machines: KeptMachine[] = [dev()]): KeptMachinesRead => ({ status: "ready", data: { machines, allowed: true, count: machines.length, limit: 2, worst_bytes: 51 * GB } });

function view(intent: RunIntent, kept: KeptMachinesRead = read()) {
  render(
    <MacOSOfferContext.Provider value={OFFER}>
      <KeptMachinesContext.Provider value={kept}>
        <WhereGroup draft={{ ...initialDraft("alice"), intent }} pre={SCENARIO_PREFLIGHT.member} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} />
      </KeptMachinesContext.Provider>
    </MacOSOfferContext.Provider>,
  );
}
const used = () => shortDate(dev().last_used_at);

describe("Review: where it runs, the machine", () => {
  it("km2-keep-named: K-25 with the name and K-20 under it", () => {
    view({ ...BASE, machine: { keep: true, name: "scratch-1" } });
    expect(screen.getByText(T.MACHINE("scratch-1")).tagName).toBe("SPAN");
    expect(screen.getByText(T.KEEP(MAC, gbOf(51 * GB)))).toBeInTheDocument();
  });
  it("km2-keep-unnamed: K-26 stands in for the name", () => {
    view({ ...BASE, machine: { keep: true } });
    expect(screen.getByText(T.MACHINE(T.NEW_UNNAMED))).toBeInTheDocument();
  });
  it("km2-resume: K-22", () => {
    view({ ...BASE, machine: { resume: "m-dev" } });
    expect(screen.getByText(T.MACHINE("dev"))).toBeInTheDocument();
    expect(screen.getByText(T.RESUME("dev", used()))).toBeInTheDocument();
  });
  it("km2-resume-not-cleaned: K-23", () => {
    view({ ...BASE, machine: { resume: "m-dev" } }, read([dev({ scrubbed: false })]));
    expect(screen.getByText(T.RESUME_NOT_CLEANED("dev", used()))).toBeInTheDocument();
    expect(screen.queryByText(T.RESUME("dev", used()))).toBeNull();
  });
  it("km2-fresh: K-24, on the machine's own Mac", () => {
    view({ ...BASE, machine: { resume: "m-dev", fresh: true } });
    expect(screen.getByText(T.FRESH("dev", MAC))).toBeInTheDocument();
  });
  it("a choice the server's answer cleared leaves its sentence where it was made", () => {
    const draft = { ...initialDraft("alice"), intent: BASE, guided: { ...initialDraft("alice").guided, machineCleared: KEEP_NEW_RUN.GONE } };
    render(
      <MacOSOfferContext.Provider value={OFFER}>
        <KeptMachinesContext.Provider value={read()}>
          <WhereGroup draft={draft} pre={SCENARIO_PREFLIGHT.member} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} />
        </KeptMachinesContext.Provider>
      </MacOSOfferContext.Provider>,
    );
    expect(screen.getByText(KEEP_NEW_RUN.GONE)).toBeInTheDocument();
  });
  it("a draft that is no longer interactive says nothing of a machine it held", () => {
    view({ ...BASE, experience: "background", machine: { keep: true } });
    expect(screen.queryByText(/^Machine · /)).toBeNull();
  });
  it("km2-none: no machine line when the run keeps nothing, or the machine is no longer listed, or the read isn't in", () => {
    view(BASE);
    view({ ...BASE, machine: { resume: "m-gone" } });
    view({ ...BASE, machine: { keep: true } }, { status: "loading" });
    expect(screen.queryByText(/^Machine · /)).toBeNull();
  });
});
