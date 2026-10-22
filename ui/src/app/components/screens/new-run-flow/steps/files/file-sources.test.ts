/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { MeDrive } from "../../../../../lib/api/me-drives";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { DRIVE_MEMBER } from "../../../../../lib/user-drives-copy";
import { driveMountsLine, driveReason } from "./file-sources";

describe("driveReason", () => {
  it("names the profile, not the pool, when the person may not run on remote-hosted runners", () => {
    expect(driveReason({ unavailable_reason: "placement_denied" } as MeDrive)).toBe(NR_RUN.REASON_NOT_ALLOWED_PROFILE);
    expect(driveReason({ unavailable_reason: "not_in_pool", source: "personal" } as MeDrive)).toBe(NR_FILES.NOT_IN_POOL);
  });

  // M10: an organisation drive the pool's runners cannot mount says why, by name.
  it("an organisation drive on a person's own runners mounts only from the organisation's runners, or is bound elsewhere", () => {
    expect(driveReason({ name: "scratch", source: "organisation", unavailable_reason: "not_in_pool" } as MeDrive)).toBe(
      `"scratch" mounts only from the organisation's runners. Choose an organisation pool to mount it, or launch without it.`,
    );
    expect(driveReason({ name: "team-share", source: "team", unavailable_reason: "bound_elsewhere" } as MeDrive)).toBe(
      DRIVE_MEMBER.NR_BOUND_ELSEWHERE("team-share"),
    );
  });
});

describe("driveMountsLine", () => {
  it("says the bound runner and path in its pool, else the server's phrase in a sentence, and nothing for a closed drive", () => {
    expect(driveMountsLine({ mounts_from: "Organisation runners and mbp-dev" } as MeDrive)).toBe("Mounts from organisation runners and mbp-dev.");
    expect(driveMountsLine({ mounts_from: "Organisation runners and mbp-dev", mounted_from: { runner: "mbp-dev", path: "/Volumes/team-share" } } as MeDrive)).toBe(
      "Mounted from mbp-dev, at /Volumes/team-share.",
    );
    expect(driveMountsLine({ mounts_from: "Organisation runners", unavailable_reason: "not_in_pool" } as MeDrive)).toBeUndefined();
    expect(driveMountsLine({} as MeDrive)).toBeUndefined();
  });
});
