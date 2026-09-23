/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #214's two rail surfaces, split out of new-run-rail.test.tsx by seam when
// that file reached the 1000-line gate: Launch's own no-barrier reason, and
// the named-stage failure card a launch that fired and failed gets instead of
// today's bare line. Neither needs the sibling file's /healthz mock, model
// access door or ADO dialog, so the harness here is the rail alone.
import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { RunRail } from "./new-run-rail";
import { NO_BARRIER, RUN } from "../../wardyn/copy";

function renderRail(props: {
  onLaunch?: () => void;
  launchError?: string | null;
  genericFailure?: boolean;
  noBarrier?: boolean;
  onDismissError?: () => void;
}) {
  return render(
    <MemoryRouter>
      <RunRail
        cc="CC1"
        showModelWarning={false}
        startup="It starts."
        showHoldNote={false}
        toolRules={null}
        launch={{
          onLaunch: props.onLaunch ?? (() => {}),
          disabled: false,
          spinning: false,
          inFlight: false,
          problem: null,
          error: props.launchError ?? null,
          genericFailure: props.genericFailure ?? false,
          credentialRefused: false,
          noBarrier: props.noBarrier ?? false,
          warnings: [],
          onOpenRun: null,
          onDismissError: props.onDismissError ?? (() => {}),
        }}
        preflight={{ error: null, result: null }}
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

// #214 — Launch's own reason when no barrier can be built: stated beside it,
// not a tooltip, and carrying a route to the step that fixes it.
describe("RunRail — Launch's no-barrier reason and route (#214)", () => {
  it("states the reason beside Launch with a link to the Environment step", () => {
    renderRail({ noBarrier: true });
    expect(screen.getByText(NO_BARRIER.LAUNCH_REASON, { exact: false })).toBeInTheDocument();
    const link = screen.getByRole("link", { name: NO_BARRIER.CTA });
    expect(link).toHaveAttribute("href", NO_BARRIER.ROUTE);
  });

  it("says nothing when a barrier can be built", () => {
    renderRail({ noBarrier: false });
    expect(screen.queryByText(NO_BARRIER.LAUNCH_REASON, { exact: false })).toBeNull();
    expect(screen.queryByRole("link", { name: NO_BARRIER.CTA })).toBeNull();
  });
});

// #214 — a launch that fired and failed anyway. `genericFailure` is the ONE
// shape with no server-composed reason (today's bare "Failed to launch
// run." line, with no role, no route, and nothing to announce it) —
// replaced by the named-stage failure card. Every OTHER server message keeps
// rendering verbatim (new-run-screen.test.tsx's own "renders a drive refusal
// verbatim" pin covers that path end to end); this file only proves the rail
// picks the right one of the two and announces both.
describe("RunRail — the launch failure card (#214)", () => {
  it("says Wardyn did not answer, without claiming a run exists, and routes to the Runs board — announced", () => {
    renderRail({ launchError: "Failed to launch run.", genericFailure: true });
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent(RUN.LAUNCH_FAILED_TITLE);
    expect(alert).toHaveTextContent(RUN.LAUNCH_FAILED_BODY);
    expect(within(alert).getByRole("link", { name: /Open Runs/ })).toHaveAttribute("href", "/runs");
    // Today's bare line is gone.
    expect(screen.queryByText("Failed to launch run.")).toBeNull();
  });

  it("dismiss clears the card", async () => {
    const onDismissError = vi.fn();
    renderRail({ launchError: "Failed to launch run.", genericFailure: true, onDismissError });
    await userEvent.click(screen.getByRole("button", { name: RUN.LAUNCH_FAILED_DISMISS }));
    expect(onDismissError).toHaveBeenCalledTimes(1);
  });

  it("a server-composed reason keeps rendering verbatim, now announced", () => {
    renderRail({ launchError: "workspaces[0]: unknown secret \"prod-db\"", genericFailure: false });
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent('workspaces[0]: unknown secret "prod-db"');
    expect(screen.queryByText(RUN.LAUNCH_FAILED_TITLE)).toBeNull();
  });

  it("no error, no card", () => {
    renderRail({});
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
