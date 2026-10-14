/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pool form's pure model: wire <-> display units round trip, and every refusal the form shows.
// The sentences are written out so a drift in copy/runner-pools.ts fails here.
import { describe, expect, it } from "vitest";
import type { RunnerPoolLimits } from "../../../../lib/types/runner-pools";
import { blankRt, fromPool, runnersBiggest, toLimits, validate, type FormValues } from "./pool-form-model";

const FULL: RunnerPoolLimits = {
  background: {
    cpu_millis: { default: 1000, cap: 4000 },
    memory_mib: { default: 2048, cap: 16384 },
    disk_mib: { default: 20480, cap: 102400 },
    lifetime: { default_sec: 7200, max_sec: 43200 },
  },
  interactive: {
    memory_mib: { default: 4096, cap: 8192 },
    lifetime: { default_sec: 28800, max_sec: 86400 },
    idle: { default_sec: 1800, max_sec: 7200 },
  },
  barriers: ["CC1", "CC2"],
  max_concurrent_runs: 10,
};

const fresh = (over: Partial<FormValues> = {}): FormValues => ({ ...fromPool(), name: "Laptop", ...over });

describe("fromPool and toLimits", () => {
  it("round-trips a pool's own limits through display units", () => {
    expect(toLimits(fromPool({ name: "ci", limits: FULL }))).toEqual(FULL);
  });

  it("shows CPUs, GB and the largest whole unit of each duration", () => {
    const v = fromPool({ name: "ci", limits: FULL });
    expect(v.bg).toMatchObject({ cpuD: "1", cpuC: "4", memD: "2", memC: "16", diskD: "20", diskC: "100", lifeDn: "2", lifeDu: "hours", lifeMn: "12", lifeMu: "hours" });
    expect(v.it).toMatchObject({ idleDn: "30", idleDu: "minutes", idleMn: "2", idleMu: "hours", lifeDn: "8", lifeMn: "1", lifeMu: "days" });
    expect([v.cc1, v.cc2, v.cc3, v.bgOn, v.itOn, v.maxruns]).toEqual([true, true, false, true, true, "10"]);
  });

  it("starts a new pool as the mock's blank run types, both on, Fence and Wall", () => {
    const v = fromPool();
    expect(v).toMatchObject({ name: "", bgOn: true, itOn: true, cc1: true, cc2: true, cc3: false, maxruns: "" });
    expect(v.bg).toEqual(blankRt(false));
    expect(v.it).toEqual(blankRt(true));
    expect(toLimits({ ...v, name: "x" })).toEqual({
      barriers: ["CC1", "CC2"],
      background: { lifetime: { default_sec: 3600, max_sec: 86400 } },
      interactive: { lifetime: { default_sec: 28800, max_sec: 86400 }, idle: { default_sec: 1800, max_sec: 7200 } },
    });
  });

  it("leaves a duration unset when editing a pool that sets none", () => {
    const v = fromPool({ name: "plain", limits: { background: { memory_mib: { default: 1024, cap: 2048 } }, barriers: ["CC2"] } });
    expect(validate(v)).toEqual({});
    expect(toLimits(v)).toEqual({ barriers: ["CC2"], background: { memory_mib: { default: 1024, cap: 2048 } } });
  });

  it("reads a pool naming no run type and no barrier as taking all of them", () => {
    const v = fromPool({ name: "open", limits: { barriers: [] } });
    expect([v.bgOn, v.itOn, v.cc1, v.cc2, v.cc3]).toEqual([true, true, true, true, true]);
    expect(validate(v)).toEqual({});
  });

  it("writes No end as an unlimited lifetime with a default of 0, and No maximum with its default", () => {
    const base = fresh({ itOn: false });
    expect(toLimits({ ...base, bg: { ...base.bg, lifeUnl: true, lifeNoEnd: true } }).background?.lifetime).toEqual({ default_sec: 0, unlimited: true });
    expect(toLimits({ ...base, bg: { ...base.bg, lifeUnl: true } }).background?.lifetime).toEqual({ default_sec: 3600, unlimited: true });
    const it = fresh({ bgOn: false });
    expect(toLimits({ ...it, it: { ...it.it, idleUnl: true } }).interactive?.idle).toEqual({ default_sec: 1800, unlimited: true });
  });

  it("round-trips No end", () => {
    const limits: RunnerPoolLimits = { background: { lifetime: { default_sec: 0, unlimited: true } }, barriers: ["CC2", "CC3"] };
    const v = fromPool({ name: "app", limits });
    expect(v.bg.lifeNoEnd).toBe(true);
    expect(toLimits(v)).toEqual(limits);
  });

  it("omits the run types that are off and a Runs at once left empty", () => {
    const v = fresh({ itOn: false });
    const l = toLimits(v);
    expect(l.interactive).toBeUndefined();
    expect(l.max_concurrent_runs).toBeUndefined();
    expect(toLimits({ ...v, maxruns: "3" }).max_concurrent_runs).toBe(3);
  });
});

describe("validate", () => {
  const withBg = (patch: Partial<FormValues["bg"]>, over: Partial<FormValues> = {}) => {
    const v = fresh({ itOn: false, ...over });
    return { ...v, bg: { ...v.bg, ...patch } };
  };

  it("accepts a new pool as prefilled", () => {
    expect(validate(fresh())).toEqual({});
  });

  it("refuses a missing, padded or too long name", () => {
    expect(validate(fresh({ name: "" })).name).toBe("Name the pool in 1 to 64 characters, without leading or trailing spaces.");
    expect(validate(fresh({ name: " Laptop" })).name).toBe("Name the pool in 1 to 64 characters, without leading or trailing spaces.");
    expect(validate(fresh({ name: "x".repeat(65) })).name).toBe("A pool name is at most 64 characters.");
    expect(validate(fresh({ name: "x".repeat(64) })).name).toBeUndefined();
  });

  it("needs a run type and a barrier", () => {
    const e = validate(fresh({ bgOn: false, itOn: false, cc1: false, cc2: false }));
    expect(e.types).toBe("A pool allows at least one run type: choose background, interactive or both.");
    expect(e.barriers).toBe("A pool allows at least one barrier.");
  });

  it("takes Runs at once as a whole number of 1 or more, or empty", () => {
    for (const bad of ["0", "1.5", "-2"]) expect(validate(fresh({ maxruns: bad })).maxruns).toBe("Set 1 or more, or leave it empty to follow its runners.");
    expect(validate(fresh({ maxruns: "" })).maxruns).toBeUndefined();
    expect(validate(fresh({ maxruns: "40" })).maxruns).toBeUndefined();
  });

  it("never checks a pool limit against its runners (a wider one is accepted)", () => {
    expect(validate(withBg({ memD: "4", memC: "9999" }))).toEqual({});
    expect(validate(fresh({ maxruns: "100000" }))).toEqual({});
  });

  it("needs both Default and Maximum per run, or neither", () => {
    expect(validate(withBg({ cpuD: "2" }))["bg.cpu"]).toBe("Background tasks, CPU: set both a default and a maximum, or leave both empty.");
    expect(validate(withBg({ memC: "8" }))["bg.mem"]).toBe("Background tasks, memory: set both a default and a maximum, or leave both empty.");
    expect(validate(withBg({}))["bg.cpu"]).toBeUndefined();
  });

  it("needs amounts above 0 and a default no more than the maximum", () => {
    expect(validate(withBg({ cpuD: "1", cpuC: "0" }))["bg.cpu"]).toBe("Background tasks, CPU: set a maximum above 0, or leave it empty.");
    expect(validate(withBg({ cpuD: "0", cpuC: "2" }))["bg.cpu"]).toBe("Background tasks, CPU: set a default above 0, or leave it empty.");
    expect(validate(withBg({ memD: "4", memC: "2" }))["bg.mem"]).toBe("Background tasks, memory: the default (4) can't be more than the maximum (2).");
    expect(validate(withBg({ memD: "2", memC: "2" }))["bg.mem"]).toBeUndefined();
  });

  it("takes disk in whole GB", () => {
    expect(validate(withBg({ diskD: "10.5", diskC: "20" }))["bg.disk"]).toBe("Background tasks, disk: set whole numbers of GB.");
    expect(validate(withBg({ diskD: "10", diskC: "20.5" }))["bg.disk"]).toBe("Background tasks, disk: set whole numbers of GB.");
    expect(validate(withBg({ diskD: "10", diskC: "20" }))["bg.disk"]).toBeUndefined();
    expect(validate(withBg({ cpuD: "0.5", cpuC: "1.5" }))["bg.cpu"]).toBeUndefined();
  });

  it("checks a lifetime: default at least a minute, a maximum or No maximum, default within maximum", () => {
    expect(validate(withBg({ lifeDn: "0" }))["bg.life"]).toBe("Background tasks, lifetime: set a default of at least 1 minute.");
    expect(validate(withBg({ lifeMn: "" }))["bg.life"]).toBe("Background tasks, lifetime: set a maximum, or choose No maximum.");
    expect(validate(withBg({ lifeDn: "3", lifeMn: "2" }))["bg.life"]).toBe("Background tasks, lifetime: the default (3 hours) can't be longer than the maximum (2 hours).");
    expect(validate(withBg({ lifeMn: "", lifeUnl: true }))["bg.life"]).toBeUndefined();
  });

  it("accepts No end only with No maximum, and a default of 0 then", () => {
    expect(validate(withBg({ lifeDn: "", lifeUnl: true, lifeNoEnd: true }))["bg.life"]).toBeUndefined();
    expect(validate(withBg({ lifeDn: "", lifeUnl: false, lifeNoEnd: true }))["bg.life"]).toBe("Background tasks, lifetime: set a default of at least 1 minute.");
  });

  it("checks an idle stop the same way and words its absent maximum as Never", () => {
    const it = fresh({ bgOn: false });
    const idle = (patch: Partial<FormValues["it"]>) => validate({ ...it, it: { ...it.it, ...patch } })["it.idle"];
    expect(idle({ idleMn: "" })).toBe("Interactive environments, idle stop: set a maximum, or choose Never.");
    expect(idle({ idleDn: "3", idleDu: "hours", idleMn: "2" })).toBe("Interactive environments, idle stop: the default (3 hours) can't be longer than the maximum (2 hours).");
    expect(idle({ idleDn: "0" })).toBe("Interactive environments, idle stop: set a default of at least 1 minute.");
    expect(idle({ idleMn: "", idleUnl: true })).toBeUndefined();
  });

  it("lets a duration pair stay wholly empty while editing a pool that sets none", () => {
    const v = fromPool({ name: "plain", limits: { interactive: { memory_mib: { default: 1024, cap: 2048 } }, barriers: ["CC1"] } });
    expect(validate(v)).toEqual({});
    expect(validate({ ...v, it: { ...v.it, idleUnl: true } })["it.idle"]).toBe("Interactive environments, idle stop: set a default of at least 1 minute.");
  });

  it("does not check a run type that is off", () => {
    const v = fresh({ itOn: false });
    expect(validate({ ...v, it: { ...v.it, cpuD: "1", idleDn: "x" } })).toEqual({});
  });
});

describe("runnersBiggest", () => {
  it("takes the largest per-run amounts and counts the runners that declare limits", () => {
    const member = (cpu: number, mem: number, limits: boolean) =>
      ({
        kind: "remote_hosted",
        id: String(cpu),
        name: "r",
        online: true,
        limits: limits ? { runs_at_once: 2, total: { cpu_millis: 8000, memory_mib: 16384, disk_mib: 102400 }, per_run: { cpu_millis: cpu, memory_mib: mem, disk_mib: 1024 } } : null,
      }) as never;
    expect(runnersBiggest([member(2000, 4096, true), member(4000, 2048, true), member(9000, 9999, false)])).toEqual({ cpu_millis: 4000, memory_mib: 4096, disk_mib: 1024, count: 2 });
    expect(runnersBiggest([]).count).toBe(0);
  });
});

// An edit sends the pool's limits whole, and the form shows durations in whole minutes, hours or days with
// an unset default as 1 hour. A duration the person did not touch goes back as it was read.
describe("an edit keeps the durations it did not touch", () => {
  const ODD: RunnerPoolLimits = {
    background: { lifetime: { default_sec: 90, max_sec: 150 } },
    interactive: { lifetime: { default_sec: 45, max_sec: 86400 }, idle: { default_sec: 0, unlimited: true } },
    barriers: ["CC1"],
  };

  it("a rename sends 90 s, 150 s, 45 s and an idle stop that never stops by default exactly as read", () => {
    const v = fromPool({ name: "odd", limits: ODD });
    expect(toLimits({ ...v, name: "odder" }, ODD)).toEqual(ODD);
  });

  it("a duration the person changes converts as typed, the others stay as read", () => {
    const v = fromPool({ name: "odd", limits: ODD });
    const l = toLimits({ ...v, bg: { ...v.bg, lifeDn: "3", lifeDu: "minutes", lifeMn: "1", lifeMu: "hours" } }, ODD);
    expect(l.background?.lifetime).toEqual({ default_sec: 180, max_sec: 3600 });
    expect(l.interactive).toEqual(ODD.interactive);
    const never = toLimits({ ...v, it: { ...v.it, idleDn: "15", idleDu: "minutes" } }, ODD);
    expect(never.interactive?.idle).toEqual({ default_sec: 900, unlimited: true });
  });
});

describe("an edit validates and sends what it will send, field by field", () => {
  const ODD: RunnerPoolLimits = { background: { lifetime: { default_sec: 90, max_sec: 150 } }, barriers: ["CC1"] };
  const SHORT: RunnerPoolLimits = { background: { lifetime: { default_sec: 10, max_sec: 20 } }, barriers: ["CC1"] };

  it("a maximum-only change leaves the 90 s default as read", () => {
    const v = fromPool({ name: "odd", limits: ODD });
    const edited = { ...v, bg: { ...v.bg, lifeMn: "1", lifeMu: "hours" as const } };
    expect(validate(edited, ODD)).toEqual({});
    expect(toLimits(edited, ODD).background?.lifetime).toEqual({ default_sec: 90, max_sec: 3600 });
  });

  it("a rename of a pool with a sub-30 s duration saves, sent as read", () => {
    const v = { ...fromPool({ name: "short", limits: SHORT }), name: "shorter" };
    expect(validate(v, SHORT)).toEqual({});
    expect(toLimits(v, SHORT)).toEqual(SHORT);
  });

  it("a default typed over the untouched maximum is checked against the maximum as read", () => {
    const v = fromPool({ name: "short", limits: SHORT });
    const e = validate({ ...v, bg: { ...v.bg, lifeDn: "1", lifeDu: "minutes" } }, SHORT);
    expect(e["bg.life"]).toBe("Background tasks, lifetime: the default (1 minute) can't be longer than the maximum (20 seconds).");
  });
});
