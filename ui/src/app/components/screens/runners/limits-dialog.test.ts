/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the limits dialog refuses before the server is asked. Strings are written out.
import { describe, expect, it } from "vitest";
import { validateLimits } from "./limits-dialog";

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
