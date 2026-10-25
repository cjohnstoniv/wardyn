/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pool list's `macos_vm` read as the offer the OS row and the harness list use (mock M-N1).
import { describe, expect, it } from "vitest";
import type { PoolMacOSVM, PoolMacOSVMRefusal } from "../../../../../lib/types/runner-pools";
import { harnessRefusal, macOSOfferOf, refusalText } from "./macos-offer";

const refusal = (over: Partial<PoolMacOSVMRefusal>): PoolMacOSVMRefusal => ({ runner: "mac-studio-07", why: "not_offered", detail: "", ...over });
const one = (r: Partial<PoolMacOSVMRefusal>): PoolMacOSVM => ({ offered: false, runner: "mac-studio-07", refusals: [refusal(r)] });
const text = (pool: PoolMacOSVM) => {
  const offer = macOSOfferOf(pool);
  return offer && !offer.ok ? refusalText(offer.refusal) : undefined;
};

describe("macOSOfferOf", () => {
  it("is no offer while the pool says nothing", () => {
    expect(macOSOfferOf(undefined)).toBeUndefined();
  });
  it("offered: the guest, the smallest run and the harnesses, with Your own tools always among them", () => {
    expect(macOSOfferOf({ offered: true, guest_os: "macOS 27.0.1 (26A434)", min_cpus: 4, min_memory_mib: 8192, harnesses: ["claude-code"], runner: "mac-studio-07" })).toEqual({
      ok: true, guestOS: "macOS 27.0.1 (26A434)", minCPUs: 4, minMemoryMiB: 8192, harnesses: ["none", "claude-code"], runner: "mac-studio-07",
    });
    expect(macOSOfferOf({ offered: true, harnesses: ["none"] })).toMatchObject({ ok: true, guestOS: "", harnesses: ["none"], runner: undefined });
  });
  it("a pool of one Mac gets that Mac's own refusal, by why", () => {
    expect(text(one({ why: "not_offered" }))).toBe("mac-studio-07 doesn't offer macOS VMs: it is older than 0.10, or macOS VMs aren't turned on for it.");
    expect(text(one({ why: "preparing", progress: 42 }))).toBe("mac-studio-07 is still preparing macOS (42%).");
    expect(text(one({ why: "at_limit" }))).toBe("mac-studio-07 already runs 2 macOS VMs, the most Apple's licence allows.");
    expect(text(one({ why: "no_room" }))).toBe("mac-studio-07 has no room for another macOS VM right now.");
    expect(text(one({ why: "no_base" }))).toBe("mac-studio-07 has no macOS base image ready yet.");
    expect(text(one({ why: "no_base", error: "disk full" }))).toBe("mac-studio-07 has no macOS base image ready yet: disk full.");
    expect(text(one({ why: "no_posture" }))).toContain("mac-studio-07 has no MDM posture file");
  });
  it("not managed names the trust command only when the wire gave the runner id (a security operator)", () => {
    expect(text(one({ why: "not_managed", runner_id: "r-1" }))).toBe("mac-studio-07 is not marked as company-managed. Mark it with `wardyn runner trust r-1 managed_device`.");
    expect(text(one({ why: "not_managed" }))).toBe("mac-studio-07 is not marked as company-managed. Ask a security operator to mark it.");
  });
  it("several Macs get the pool form: MV-N05 when one does not offer macOS VMs, else MV-N14", () => {
    const refusals = (...whys: PoolMacOSVMRefusal["why"][]) => whys.map((why, i) => refusal({ why, runner: `mac-${i}` }));
    expect(text({ offered: false, refusals: refusals("not_offered") })).toMatch(/^Every runner in this pool must offer macOS VMs/);
    expect(text({ offered: false, refusals: refusals("preparing", "not_offered") })).toMatch(/^Every runner in this pool must offer macOS VMs/);
    expect(text({ offered: false, refusals: refusals("preparing", "at_limit") })).toMatch(/^Every runner in this pool must be able to take a macOS VM/);
    // One refusal among several Macs is still the pool's: the single-Mac form needs the pool to name its one Mac.
    expect(text({ offered: false, refusals: refusals("at_limit") })).toMatch(/^Every runner in this pool must be able to take a macOS VM/);
  });
  it("a pool with no Mac to ask is MV-N05", () => {
    expect(text({ offered: false })).toMatch(/^Every runner in this pool must offer macOS VMs/);
  });
  it("the harness refusal names the one Mac, or speaks for the pool", () => {
    const offered = (runner?: string) => macOSOfferOf({ offered: true, harnesses: ["none"], runner });
    expect(harnessRefusal(offered("mac-studio-07"), "claude-code", "Claude Code")).toBe("macOS VMs on mac-studio-07 offer only Your own tools until Apple's Command Line Tools finish installing.");
    expect(harnessRefusal(offered(), "claude-code", "Claude Code")).toMatch(/^Every runner in this pool must offer Claude Code in a macOS VM/);
    expect(harnessRefusal(offered(), "none", "Your own tools")).toBeUndefined();
  });
});
