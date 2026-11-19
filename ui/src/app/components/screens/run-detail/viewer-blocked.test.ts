/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import type { ApprovalRequest } from "../../../lib/types";
import { viewerBlockedOnDecision } from "./viewer-blocked";

const kubeWrite = {
  id: "ap-1",
  kind: "tool_call",
  requested_scope: { call_sha256: "ab12", cluster: "dev" },
} as unknown as ApprovalRequest;

describe("viewerBlockedOnDecision on a held kubectl write", () => {
  it("does not call the run's owner blocked when the row leaves the verdict to ownership", () => {
    expect(viewerBlockedOnDecision([kubeWrite], false, true)).toBe(false);
  });

  it("calls a viewer who does not own the run blocked", () => {
    expect(viewerBlockedOnDecision([kubeWrite], false, false)).toBe(true);
  });

  it("follows the server's per-row verdict over ownership", () => {
    expect(viewerBlockedOnDecision([{ ...kubeWrite, decidable: false } as ApprovalRequest], false, true)).toBe(true);
  });
});
