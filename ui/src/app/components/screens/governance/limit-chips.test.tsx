/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import type { GovernanceLimits } from "../../../lib/api/governance";
import { GOVERNANCE as GOV, LIMITS_CHIP } from "../../../lib/governance-copy";
import { limitChips, limitsBoundNothing } from "./limit-chips";

// #2111/#2506: every GovernanceLimits field is a limit, and "None" is the claim that a profile bounds none of
// them. A profile bounding only a placement, the UI apps, CPU, memory or a disk cap used to read "None" while
// the server enforced it.
describe("limitChips and limitsBoundNothing", () => {
  it.each([
    ["deny_placements", { deny_placements: ["remote"] }, LIMITS_CHIP.PLACEMENT("remote")],
    ["deny_ui_apps", { deny_ui_apps: true }, LIMITS_CHIP.UI_APPS],
    ["max_cpu_millis", { max_cpu_millis: 2000 }, `${LIMITS_CHIP.CPU}: 2000`],
    ["max_memory_mib", { max_memory_mib: 2048 }, `${LIMITS_CHIP.MEMORY}: 2048`],
    ["max_ephemeral_disk_mib", { max_ephemeral_disk_mib: 4096 }, `${GOV.LIMIT_EPHEMERAL_LABEL}: 4096`],
    ["max_drive_size_mib", { max_drive_size_mib: 1024 }, `${GOV.LIMIT_DRIVE_SIZE_LABEL}: 1024`],
  ] as [string, GovernanceLimits, string][])("a profile whose only limit is %s draws a chip and is not 'None'", (_field, limits, chip) => {
    render(<>{limitChips(limits)}</>);
    expect(screen.getByText(chip)).toBeInTheDocument();
    expect(limitsBoundNothing(limits)).toBe(false);
  });

  it("a run limit the summary chip does not name still counts, and no limits at all is None", () => {
    expect(limitsBoundNothing({ default_end_sec: 86400 } as GovernanceLimits)).toBe(false);
    expect(limitsBoundNothing({})).toBe(true);
    expect(limitsBoundNothing({ max_cpu_millis: 0, deny_placements: [] })).toBe(true);
  });
});
