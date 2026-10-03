/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// P7: the quota sentences are the server's (internal/api/run_fit.go); the console adds no string of
// its own, so this pins that the rail shows them verbatim, the refusal as the preflight alert and the
// advisories in the warnings list. Its own file: new-run-rail.test.tsx is at the file-size cap.
import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

vi.mock("../../../lib/api/health", () => ({
  health: { health: () => Promise.resolve({ components: { recording: { selected: "fs" } } }) },
}));

import { RunRail } from "./new-run-rail";
import type { PreflightResult } from "../../../lib/types";

const REFUSAL =
  "this run needs 2 CPU, 4Gi memory, more than quota runs-quota has left (1 CPU, 2Gi memory). Stop a run, or ask your admin to raise the quota.";
const NEAR_FULL =
  "this run would fill quota runs-quota to 94% (0.5 CPU, 1Gi memory left after it) — later runs may be refused.";
const NODE_FIT =
  "no node this run may be placed on is large enough for 2 CPU, 4Gi memory. It may wait unscheduled until one is.";

function rail(preflight: { error: string | null; result: PreflightResult | null }) {
  return render(
    <MemoryRouter>
      <RunRail
        cc="CC1"
        showModelWarning={false}
        startup="It starts."
        showHoldNote={false}
        toolRules={null}
        unattended={false}
        launch={{
          onLaunch: () => {},
          disabled: false,
          spinning: false,
          inFlight: false,
          problem: null,
          error: null,
          errorSeq: 0,
          credentialRefused: false,
        }}
        preflight={{ ...preflight, errorSeq: 1 }}
        adoDialog={{
          open: false,
          connecting: false,
          org: "",
          blockedUrl: null,
          onConfirm: () => {},
          onFallbackClick: () => {},
          onCancel: () => {},
        }}
      />
    </MemoryRouter>,
  );
}

describe("RunRail — namespace quota sentences", () => {
  it("shows the server's refusal as the preflight alert, verbatim", () => {
    rail({ error: REFUSAL, result: null });
    expect(screen.getByRole("alert")).toHaveTextContent(REFUSAL);
    expect(screen.queryByTestId("preflight-result")).toBeNull();
  });

  it("lists the near-full and node-fit advisories in the warnings, verbatim", () => {
    rail({
      error: null,
      result: { setup_items: [], enforced_confinement_class: "CC1", warnings: [NEAR_FULL, NODE_FIT] },
    });
    const items = within(screen.getByTestId("preflight-result")).getAllByRole("listitem");
    expect(items.map((li) => li.textContent)).toEqual([NEAR_FULL, NODE_FIT]);
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
