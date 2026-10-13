/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the limits dialog refuses before the server is asked. Strings are written out.
import { describe, expect, it } from "vitest";
import { limitsToSave, validateLimits } from "./limits-dialog";

const vals = (over: Partial<Parameters<typeof validateLimits>[0]> = {}) => ({ rn: "3", tc: "6", tm: "12", td: "200", pc: "2", pm: "4", pd: "50", ...over });
const reported = { cpu_millis: 8000, memory_mib: 16 * 1024, disk_mib: 500 * 1024 };

describe("limits the dialog refuses", () => {
  it("takes a whole set that fits what the computer reports", () => {
    expect(validateLimits(vals(), reported)).toEqual({});
    // Memory and CPU may be fractions; the server stores MiB and milli-CPU.
    expect(validateLimits(vals({ tm: "12.5", pc: "1.5" }), reported)).toEqual({});
  });

  it("takes disk in whole GB only, in total and per run, because the server does", () => {
    expect(validateLimits(vals({ td: "200.5" }), reported)).toEqual({ td: "Disk is a whole number of GB." });
    expect(validateLimits(vals({ pd: "66.7" }), reported)).toEqual({ pd: "Disk is a whole number of GB." });
  });

  it("refuses a total above what the computer reports, a per-run above the total, and a Runs at once that is not a whole number of 1 or more", () => {
    expect(validateLimits(vals({ tm: "64" }), reported)).toEqual({ tm: "Not allowed: this computer reports 16 GB memory." });
    expect(validateLimits(vals({ pm: "20" }), reported)).toEqual({ pm: "Per run can't be more than the total." });
    expect(validateLimits(vals({ rn: "0" }))).toEqual({ rn: "Runs at once is a whole number, 1 or more." });
    expect(validateLimits(vals({ rn: "2.5" }))).toEqual({ rn: "Runs at once is a whole number, 1 or more." });
    expect(validateLimits(vals({ tc: "", pc: "0" }))).toEqual({ tc: "Set a number above 0.", pc: "Set a number above 0." });
    // A remote-hosted runner reports nothing to hold its totals to.
    expect(validateLimits(vals({ tm: "640", td: "9000" }))).toEqual({});
  });
});

// The dialog shows CPUs and GB to one decimal. A field still showing what it loaded is sent exactly as
// loaded; only a field the person changed converts from what they typed.
describe("what a save sends", () => {
  const LOADED = { runs_at_once: 3, total: { cpu_millis: 6000, memory_mib: 12288, disk_mib: 204800 }, per_run: { cpu_millis: 1250, memory_mib: 1700, disk_mib: 51200 } };
  const shown = vals({ pc: "1.3", pm: "1.7" });

  it("an edit to Runs at once leaves 1250 milli-CPU and 1700 MiB as they were", () => {
    expect(limitsToSave({ ...shown, rn: "2" }, { vals: shown, limits: LOADED })).toEqual({ ...LOADED, runs_at_once: 2 });
  });

  it("a changed field converts as typed", () => {
    expect(limitsToSave({ ...shown, pm: "2" }, { vals: shown, limits: LOADED }).per_run).toEqual({ cpu_millis: 1250, memory_mib: 2048, disk_mib: 51200 });
  });
});

// An unchanged amount is checked at its exact loaded value, not the rounded display: 32 MiB shows as 0 GB.
describe("what a save validates", () => {
  const LOADED = { runs_at_once: 3, total: { cpu_millis: 6000, memory_mib: 32, disk_mib: 204800 }, per_run: { cpu_millis: 1250, memory_mib: 32, disk_mib: 51200 } };
  const shown = vals({ tm: "0", pm: "0", pc: "1.3" });
  const loaded = { vals: shown, limits: LOADED };

  it("takes an unchanged 32 MiB shown as 0 GB, and still refuses a typed 0", () => {
    expect(validateLimits(shown, undefined, loaded)).toEqual({});
    expect(validateLimits({ ...shown, rn: "2" }, undefined, loaded)).toEqual({});
    expect(validateLimits({ ...shown, tm: "0", pm: "0.0" }, undefined, loaded)).toEqual({ pm: "Set a number above 0." });
  });

  it("still checks an unchanged field against the others at its exact value", () => {
    expect(validateLimits({ ...shown, tm: "0.01" }, undefined, loaded)).toEqual({ pm: "Per run can't be more than the total." });
  });
});

// The reported amount is compared exactly, as the server does; only its words round.
describe("what a save checks against the reported amount", () => {
  it("takes an unchanged total equal to the reported MiB when only Runs at once is edited", () => {
    const loaded = { vals: vals({ tm: "15.5", pm: "2" }), limits: { runs_at_once: 3, total: { cpu_millis: 6000, memory_mib: 15900, disk_mib: 204800 }, per_run: { cpu_millis: 2000, memory_mib: 2048, disk_mib: 51200 } } };
    const rep = { cpu_millis: 8000, memory_mib: 15900, disk_mib: 500 * 1024 };
    expect(validateLimits({ ...loaded.vals, rn: "2" }, rep, loaded)).toEqual({});
    expect(validateLimits({ ...loaded.vals, rn: "2", tm: "15.6" }, rep, loaded)).toEqual({ tm: "Not allowed: this computer reports 15.5 GB memory." });
  });
});
