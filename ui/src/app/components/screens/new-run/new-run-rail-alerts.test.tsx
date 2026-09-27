/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #459: the rail's launch and preflight failure lines are announced. Split
// from new-run-rail.test.tsx (file-size cap); this seam needs no model-access
// provider, so the rail mounts bare.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

vi.mock("../../../lib/api/health", () => ({
  health: { health: () => Promise.resolve({ components: { recording: { selected: "fs" } } }) },
}));

import { RunRail } from "./new-run-rail";
import { RAIL } from "../../wardyn/copy";

type AlertProps = {
  launchError?: string;
  launchErrorSeq?: number;
  preflightError?: string;
  preflightErrorSeq?: number;
};

function railTree(props: AlertProps) {
  return (
    <MemoryRouter>
      <RunRail
        cc="CC1"
        showModelWarning={false}
        startup="It starts."
        showHoldNote={false}
        toolRules={null}
        launch={{
          onLaunch: () => {},
          disabled: false,
          spinning: false,
          inFlight: false,
          problem: null,
          error: props.launchError ?? null,
          errorSeq: props.launchErrorSeq ?? 0,
          credentialRefused: false,
          warnings: [],
          onOpenRun: null,
        }}
        preflight={{ error: props.preflightError ?? null, errorSeq: props.preflightErrorSeq ?? 0, result: null }}
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
    </MemoryRouter>
  );
}

function renderRail(props: AlertProps) {
  const result = render(railTree(props));
  return { ...result, rerenderWith: (next: AlertProps) => result.rerender(railTree({ ...props, ...next })) };
}

describe("RunRail — failure lines are announced (#459)", () => {
  it("the launch error is an alert carrying the sr-only prefix and the server's sentence", () => {
    renderRail({ launchError: "the server's launch sentence" });
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent(RAIL.LAUNCH_ERROR_LABEL);
    expect(alert).toHaveTextContent("the server's launch sentence");
    // The sentence itself is unchanged and visible — only the prefix hides.
    expect(screen.getByText("the server's launch sentence")).toBeVisible();
  });

  it("the preflight error is an alert carrying the sr-only prefix and the server's sentence", () => {
    renderRail({ preflightError: "the server's preflight sentence" });
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent(RAIL.PREFLIGHT_ERROR_LABEL);
    expect(alert).toHaveTextContent("the server's preflight sentence");
  });

  it("a repeated, identical launch failure remounts the alert region (errorSeq keys it)", () => {
    const r = renderRail({ launchError: "same sentence", launchErrorSeq: 1 });
    const first = screen.getByRole("alert");
    r.rerenderWith({ launchError: "same sentence", launchErrorSeq: 2 });
    const second = screen.getByRole("alert");
    expect(second).not.toBe(first);
  });

  it("a repeated, identical preflight failure remounts the alert region (errorSeq keys it)", () => {
    const r = renderRail({ preflightError: "same sentence", preflightErrorSeq: 1 });
    const first = screen.getByRole("alert");
    r.rerenderWith({ preflightError: "same sentence", preflightErrorSeq: 2 });
    const second = screen.getByRole("alert");
    expect(second).not.toBe(first);
  });
});
