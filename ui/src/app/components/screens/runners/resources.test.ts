/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { RemoteRunnerView, RunnerPool, RunnerPoolMemberView } from "../../../lib/types/runner-pools";
import type { RunnerLimits, RunnerUsage, RunnerView } from "../../../lib/types/runners";
import {
  amountsText, barriersText, fmtAmount, fmtDur, fmtInUse, membersOf, narrowShort, narrowText, perRunShare, poolRes, proposeLimits, runnerRes, runTypesText, usedPct,
} from "./resources";

const G = 1024;
const limits = (runs: number, tc: number, tm: number, td: number, pc: number, pm: number, pd: number): RunnerLimits => ({
  runs_at_once: runs,
  total: { cpu_millis: tc * 1000, memory_mib: tm * G, disk_mib: td * G },
  per_run: { cpu_millis: pc * 1000, memory_mib: pm * G, disk_mib: pd * G },
});
const usage = (online: boolean, l: RunnerLimits, runs: number, c: number, m: number, d: number): RunnerUsage => ({
  online,
  in_use: { runs, cpu_millis: c * 1000, memory_mib: m * G, disk_mib: d * G },
  available: online
    ? { runs: l.runs_at_once - runs, cpu_millis: l.total.cpu_millis - c * 1000, memory_mib: l.total.memory_mib - m * G, disk_mib: l.total.disk_mib - d * G }
    : { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 },
  disk_capped: true,
});
const laptop = limits(3, 6, 12, 200, 2, 4, 50);
const shared = limits(6, 24, 96, 1000, 4, 16, 100);
const member = (id: string, kind: RunnerPoolMemberView["kind"], l: RunnerLimits | null, u: RunnerUsage): RunnerPoolMemberView => ({ kind, id, name: id, online: u.online, limits: l, usage: u });
const pool = (over: Partial<RunnerPool> = {}): RunnerPool => ({
  id: "p1", name: "Laptop", scope: "user", owner: "dana@example.com", state: "active", revision: 4, created_at: "2026-10-01T00:00:00Z", updated_at: "2026-10-12T00:00:00Z", ...over,
});

describe("a runner's resources", () => {
  it("reads Total, Per run, In use and Available from its limits and usage", () => {
    const r = runnerRes(laptop, usage(true, laptop, 2, 4, 8, 100))!;
    expect(r.counted).toBe(true);
    expect(r.rows.cpu).toEqual({ total: 6, perRun: 2, inUse: 4, available: 2 });
    expect(r.rows.runs).toEqual({ total: 3, perRun: null, inUse: 2, available: 1 });
    expect(fmtInUse("cpu", r.rows.cpu)).toBe("4 of 6 CPUs");
    expect(fmtInUse("mem", r.rows.mem)).toBe("8 GB of 12 GB");
    expect(fmtInUse("runs", r.rows.runs)).toBe("2 of 3");
    expect(fmtAmount("disk", r.rows.disk.available)).toBe("100 GB");
    expect(usedPct(r.rows.cpu)).toBe(67);
  });

  it("is not counted while offline, and has no table without limits", () => {
    expect(runnerRes(laptop, usage(false, laptop, 0, 0, 0, 0))!.counted).toBe(false);
    expect(runnerRes(null, usage(true, laptop, 0, 0, 0, 0))).toBeNull();
  });

  it("writes amounts in CPUs and GB, one CPU in the singular", () => {
    expect(amountsText({ cpu_millis: 8000, memory_mib: 16 * G, disk_mib: 500 * G })).toBe("8 CPUs · 16 GB memory · 500 GB disk");
    expect(amountsText({ cpu_millis: 1000, memory_mib: 1536, disk_mib: G })).toBe("1 CPU · 1.5 GB memory · 1 GB disk");
  });
});

describe("a pool's resources", () => {
  const members = [member("laptop-7", "self_hosted", laptop, usage(true, laptop, 2, 4, 8, 100)), member("shared-docker", "remote_hosted", shared, usage(true, shared, 1, 4, 16, 100))];

  it("adds its runners up and takes the biggest per-run limit", () => {
    const r = poolRes(pool(), members);
    expect(r.rows.cpu).toEqual({ total: 30, perRun: 4, inUse: 8, available: 22 });
    expect(r.rows.runs).toEqual({ total: 9, perRun: null, inUse: 3, available: 6 });
    expect(r).toMatchObject({ runners: 2, offline: 0, narrowed: [], counted: true });
    expect(narrowShort(pool(), r)).toBe("");
    expect(narrowText(pool(), r)).toBe("This pool adds no limits of its own: it follows its runners' limits.");
  });

  it("narrows the biggest run by the pool's own smaller maximum, and never widens it", () => {
    const narrow = pool({
      limits: { barriers: ["CC1"], max_concurrent_runs: 10, background: { memory_mib: { default: 4 * G, cap: 8 * G }, cpu_millis: { default: 2000, cap: 64000 } } },
    });
    const r = poolRes(narrow, members);
    expect(r.narrowed).toEqual(["mem"]);
    expect(r.rows.mem.perRun).toBe(8);
    expect(r.rows.cpu.perRun).toBe(4);
    expect(narrowShort(narrow, r)).toBe("Pool limits: memory per run narrowed; 10 runs maximum");
    expect(narrowText(narrow, r)).toBe("Pool limits: memory per run narrowed by this pool; 10 runs maximum in this pool. Otherwise from its runners.");
  });

  it("counts nothing as available while switched off, and counts its offline runners", () => {
    const off = poolRes(pool({ state: "disabled" }), members);
    expect(off.counted).toBe(false);
    expect(off.rows.cpu.available).toBe(0);
    const one = poolRes(pool(), [member("lab-vm-1", "self_hosted", laptop, usage(false, laptop, 0, 0, 0, 0)), members[1]]);
    expect(one.offline).toBe(1);
    expect(one.rows.cpu).toMatchObject({ total: 30, available: 20 });
  });

  it("names run types and barriers, both when the pool sets none", () => {
    expect(runTypesText(undefined)).toBe("Interactive and background");
    expect(runTypesText({ barriers: ["CC2"], background: {} })).toBe("Background only");
    expect(runTypesText({ barriers: ["CC1"], interactive: {} })).toBe("Interactive only");
    expect(barriersText({ barriers: ["CC1", "CC2"] })).toBe("Fence, Wall");
    expect(barriersText(undefined)).toBe("Fence, Wall, Vault");
  });
});

describe("membership from the runner reads", () => {
  const view = (id: string, state: RunnerView["state"], pools: string[]) =>
    ({ id, name: id, state, online: true, kind: "self_hosted", limits: laptop, usage: usage(true, laptop, 0, 0, 0, 0), pools: pools.map((p) => ({ id: p, name: p })) }) as RunnerView;
  const remote = (id: string, pools: string[]) =>
    ({ id, name: id, kind: "remote_hosted", online: true, configured: true, limits: shared, usage: usage(true, shared, 0, 0, 0, 0), pools: pools.map((p) => ({ id: p, name: p, scope: "user" })) }) as RemoteRunnerView;

  it("lists a pool's runners of both kinds and leaves a revoked one out", () => {
    const got = membersOf("p1", [view("laptop-7", "claimed", ["p1", "p2"]), view("old", "revoked", ["p1"]), view("desk-2", "claimed", ["p2"])], [remote("shared-docker", ["p1"]), remote("eu", [])]);
    expect(got.map((m) => `${m.kind}:${m.id}`)).toEqual(["self_hosted:laptop-7", "remote_hosted:shared-docker"]);
    expect(got[1].configured).toBe(true);
  });
});

describe("limits setup", () => {
  it("shares a total between runs: at least 1 CPU and 1 GB, disk in whole GB, never past the total", () => {
    expect(perRunShare({ cpu: 24, mem: 96, disk: 1000 }, 6)).toEqual({ cpu: 4, mem: 16, disk: 166 });
    expect(perRunShare({ cpu: 6, mem: 12, disk: 200 }, 3)).toEqual({ cpu: 2, mem: 4, disk: 66 });
    expect(perRunShare({ cpu: 2, mem: 2, disk: 3 }, 4)).toEqual({ cpu: 1, mem: 1, disk: 1 });
    expect(perRunShare({ cpu: 0.5, mem: 2, disk: 3 }, 1)).toEqual({ cpu: 0.5, mem: 2, disk: 3 });
    expect(perRunShare({ cpu: 4, mem: 0, disk: 3 }, 2)).toBeNull();
    expect(perRunShare({ cpu: 4, mem: 8, disk: 3 }, 0)).toBeNull();
  });

  it("proposes half of what the computer reports, with Runs at once about half its CPUs", () => {
    const p = proposeLimits({ cpu_millis: 8000, memory_mib: 32 * G, disk_mib: 500 * G });
    expect(p.runs_at_once).toBe(4);
    expect(amountsText(p.total)).toBe("4 CPUs · 16 GB memory · 250 GB disk");
    expect(amountsText(p.per_run)).toBe("1 CPU · 4 GB memory · 62 GB disk");
  });

  it("writes a duration in its largest whole unit", () => {
    expect([fmtDur(3600), fmtDur(86400), fmtDur(1800), fmtDur(172800), fmtDur(90)]).toEqual(["1 hour", "1 day", "30 minutes", "2 days", "90 seconds"]);
  });
});
