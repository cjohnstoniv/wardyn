/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { MeDrive } from "../../../../../lib/api/me-drives";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { driveReason } from "./file-sources";

describe("driveReason", () => {
  it("names the profile, not the pool, when the person may not run on remote-hosted runners", () => {
    expect(driveReason({ unavailable_reason: "placement_denied" } as MeDrive)).toBe(NR_RUN.REASON_NOT_ALLOWED_PROFILE);
    expect(driveReason({ unavailable_reason: "not_in_pool" } as MeDrive)).toBe(NR_FILES.NOT_IN_POOL);
  });
});
