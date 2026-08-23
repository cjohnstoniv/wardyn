/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The only place a member ever learns a capability narrowed their run: under an
// enforced egress_host/secret capability the server drops the values they typed
// and returns them in createRun's `warnings[]` (internal/api/runs.go). The run
// launched, so this is a toast, not a blocker — but it must actually fire, and
// it must carry the server's own text, which is the half that names what went.
import { describe, it, expect, vi, beforeEach } from "vitest";

const warningMock = vi.fn();
vi.mock("sonner", () => ({ toast: { warning: (...a: unknown[]) => warningMock(...a) } }));

import { surfaceRunWarnings } from "./run-warnings";
import type { CreateRunResult } from "../../../lib/types";

const created = (warnings?: string[]) => ({ id: "run-1", warnings }) as CreateRunResult;

beforeEach(() => warningMock.mockReset());

describe("surfaceRunWarnings", () => {
  it("raises one toast per warning, carrying the server's text verbatim", () => {
    surfaceRunWarnings(
      created([
        "egress_host: internal.example.com was dropped — not granted to you",
        "secret: DEPLOY_KEY was dropped — not granted to you",
      ]),
    );
    expect(warningMock).toHaveBeenCalledTimes(2);
    expect(warningMock.mock.calls[0][1]).toEqual({
      description: "egress_host: internal.example.com was dropped — not granted to you",
    });
    expect(warningMock.mock.calls[1][1]).toEqual({
      description: "secret: DEPLOY_KEY was dropped — not granted to you",
    });
  });

  it("stays silent when the response carries no warnings", () => {
    surfaceRunWarnings(created());
    surfaceRunWarnings(created([]));
    expect(warningMock).not.toHaveBeenCalled();
  });
});
