/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Review's Limits group for a macOS VM (mock M-N1 frames 10 and 11) and the blocker that holds Launch.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { NR_MACHINE } from "../../../../wardyn/copy/new-run/machine";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { initialDraft, type NewRunDraft } from "../../draft/run-intent";
import { MacOSOfferContext, type MacOSOffer } from "../../steps/run/macos-offer";
import { buildBlockers, groupsBlockedBy } from "../blockers";
import { LimitsGroup } from "./limits-group";

afterEach(cleanup);

const OFFER: MacOSOffer = { ok: true, guestOS: "macOS 27.0.1", minCPUs: 4, minMemoryMiB: 8192, harnesses: ["none"] };
const draft = (resources: { cpu_millis: number; memory_mib: number }, machine_kind: "macos_vm" | "linux_vm" = "macos_vm"): NewRunDraft => ({
  ...initialDraft("alice"), intent: { shape: "agent_task", experience: "interactive", machine_kind, resources },
});

function show(d: NewRunDraft, offer: MacOSOffer | null = OFFER) {
  render(
    <MacOSOfferContext.Provider value={offer ?? undefined}>
      <LimitsGroup draft={d} pre={null} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} />
    </MacOSOfferContext.Provider>,
  );
}

describe("Review: Limits for a macOS VM", () => {
  it("frame 10: the pre-filled minimum carries the macOS minimum mark", () => {
    show(draft({ cpu_millis: 4000, memory_mib: 8192 }));
    expect(screen.getByText(NR_REVIEW.LIMITS_DETAIL)).toBeInTheDocument();
    expect(screen.getByText(NR_MACHINE.MACOS_MINIMUM_MARK)).toBeInTheDocument();
    expect(screen.queryByText(NR_MACHINE.MACOS_BELOW_MINIMUM("4", "8192"))).toBeNull();
  });

  it("the mark leaves once the ask is raised above the minimum", () => {
    show(draft({ cpu_millis: 6000, memory_mib: 8192 }));
    expect(screen.queryByText(NR_MACHINE.MACOS_MINIMUM_MARK)).toBeNull();
  });

  it("frame 11: an ask lowered under the minimum names the minimum", () => {
    show(draft({ cpu_millis: 2000, memory_mib: 8192 }));
    expect(screen.getByText(NR_MACHINE.MACOS_BELOW_MINIMUM("4", "8192"))).toBeInTheDocument();
    expect(screen.queryByText(NR_MACHINE.MACOS_MINIMUM_MARK)).toBeNull();
  });

  it("says nothing of macOS for a Linux VM or before the Macs have said their minimum", () => {
    show(draft({ cpu_millis: 4000, memory_mib: 8192 }, "linux_vm"));
    expect(screen.queryByText(NR_MACHINE.MACOS_MINIMUM_MARK)).toBeNull();
    cleanup();
    show(draft({ cpu_millis: 2000, memory_mib: 8192 }), null);
    expect(screen.queryByText(NR_MACHINE.MACOS_BELOW_MINIMUM("4", "8192"))).toBeNull();
  });

  it("frame 11: the sentence is a blocker on the Limits group, so Launch is held", () => {
    const blockers = buildBlockers({
      issues: [], unsupported: [], templateNeeds: [], refusal: null, setupItems: [], stale: false,
      macOSMinimum: NR_MACHINE.MACOS_BELOW_MINIMUM("4", "8192"),
    });
    expect(blockers).toEqual([expect.objectContaining({ kind: "local", text: NR_MACHINE.MACOS_BELOW_MINIMUM("4", "8192"), field: "resources" })]);
    expect([...groupsBlockedBy(blockers)]).toEqual(["limits"]);
  });
});
