/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pool list's `macos_vm` read as the offer the OS row and the harness list use (mock M-N1).
import { describe, expect, it } from "vitest";
import type { PoolMacOSBase, PoolMacOSVM, PoolMacOSVMRefusal } from "../../../../../lib/types/runner-pools";
import { aheadByHours } from "../../../../../lib/test-clock";
import { NR_MACHINE } from "../../../../wardyn/copy/new-run/machine";
import { harnessRefusal, macOSOfferOf, refusalText } from "./macos-offer";

const AT = aheadByHours(-48);
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
      ok: true, guestOS: "macOS 27.0.1 (26A434)", minCPUs: 4, minMemoryMiB: 8192, harnesses: ["none", "claude-code"], runner: "mac-studio-07", bases: [], standardNotAllowed: false,
    });
    expect(macOSOfferOf({ offered: true, harnesses: ["none"] })).toMatchObject({ ok: true, guestOS: "", harnesses: ["none"], runner: undefined });
  });
  it("offered: the pool's other bases ride on the offer, as the pool sent them", () => {
    const bases: PoolMacOSBase[] = [{ name: "xcode-16", state: "ready", guest_os: "macOS 27.0.1", harnesses: ["none"] }, { name: "qa", state: "rebuilding", progress: 60 }];
    expect(macOSOfferOf({ offered: true, harnesses: ["none"], bases })).toMatchObject({ ok: true, bases });
  });
  it("offered: Standard's prepare date, the caller's own not-allowed answer and the profile's name ride on the offer", () => {
    expect(macOSOfferOf({ offered: true, harnesses: ["none"], created_at: AT, standard_not_allowed: true }, "Platform")).toMatchObject({
      ok: true, createdAt: AT, standardNotAllowed: true, profile: "Platform",
    });
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
  it("a base too old reads MAC-P14 for one Mac, singular at one day, and MAC-P16 when the runner reports no age", () => {
    const tenant = "Its owner runs `wardyn-runnerd macos-vm prepare --new-image`.";
    expect(text(one({ why: "base_too_old", have: 41, need: 30 }))).toBe(`mac-studio-07's macOS base was prepared 41 days ago, and your organisation allows 30. ${tenant}`);
    expect(text(one({ why: "base_too_old", have: 1, need: 0.5 }))).toBe(`mac-studio-07's macOS base was prepared 1 day ago, and your organisation allows 0.5. ${tenant}`);
    expect(text(one({ why: "base_too_old", need: 30 }))).toBe("mac-studio-07 doesn't report its macOS base's age, and your organisation sets a maximum. Update the runner.");
  });
  it("a base too old among several Macs reads MAC-P15, singular at a maximum of one day", () => {
    const old = (need: number) => [refusal({ why: "base_too_old", runner: "mac-1", have: 5, need }), refusal({ why: "at_limit", runner: "mac-2" })];
    expect(text({ offered: false, refusals: old(30) })).toBe("Every runner in this pool must have a macOS base prepared in the last 30 days, and at least one doesn't. Choose another pool, or ask the Mac's owner to prepare a new base.");
    expect(text({ offered: false, refusals: old(1) })).toContain("prepared in the last 1 day, and at least one doesn't.");
  });
  it("a base too old is read from whichever refusal carries it, not only the first", () => {
    const pool: PoolMacOSVM = { offered: false, refusals: [refusal({ why: "at_limit", runner: "mac-2" }), refusal({ why: "base_too_old", runner: "mac-1", have: 41, need: 30 })] };
    expect(text(pool)).toBe(NR_MACHINE.MACOS_BASE_TOO_OLD_POOL(30));
  });
  it("a Mac that refused itself shows the canon line for its reason, with the numbers, and never its own text", () => {
    const refused = (extra: Record<string, unknown>) => one({ why: "runner_refused" as PoolMacOSVMRefusal["why"], detail: "PLANTED runner text", ...extra } as Partial<PoolMacOSVMRefusal>);
    expect(text(refused({ reason: "below_min_memory", have: 6, need: 8 }))).toBe(
      "Per-run memory on this runner is 6 GB; a macOS VM needs at least 8 GB. Its owner raises it to 8 GB or more in Settings \u2192 My runner, or with `wardyn runner set mac-studio-07 --per-run-memory 8`.",
    );
    expect(text(refused({ reason: "below_min_cpu", have: 2, need: 4 }))).toBe(
      "Per-run CPU on this runner is 2 CPUs; a macOS VM needs at least 4. Its owner raises it to 4 or more in Settings \u2192 My runner, or with `wardyn runner set mac-studio-07 --per-run-cpu 4`.",
    );
    const generic = "Every runner in this pool must be able to take a macOS VM, and at least one can't right now. Try again later, or choose another pool.";
    expect(text(refused({ reason: "other" }))).toBe(generic);
    expect(text(refused({ reason: "something_new" }))).toBe(generic);
    expect(text(refused({}))).toBe(generic);
  });
  it("not_offered stays MV-E07, whatever detail it carries", () => {
    expect(text(one({ why: "not_offered", detail: "PLANTED runner text" }))).toBe("mac-studio-07 doesn't offer macOS VMs: it is older than 0.10, or macOS VMs aren't turned on for it.");
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
    expect(text({ offered: false, refusals: [...refusals("preparing"), refusal({ why: "runner_refused" as PoolMacOSVMRefusal["why"], runner: "mac-9", detail: "x" })] })).toMatch(/^Every runner in this pool must be able to take a macOS VM/);
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
