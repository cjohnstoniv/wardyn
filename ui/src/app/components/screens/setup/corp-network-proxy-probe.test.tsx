/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Connectivity tile (#3066): a status pill, one explanation, and the button inside, in each of
// the five states the sheet draws.
import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { T } from "../../../lib/integrations";
import type { ProbeUiState } from "./corp-network-egress";
import { ProxyTestBlock } from "./corp-network-proxy-probe";

const IDLE: ProbeUiState = { kind: "idle" };
const done = (result: object): ProbeUiState => ({ kind: "done", result: { detail: "detail", ...result } }) as ProbeUiState;

function renderTile(state: ProbeUiState, operator = true) {
  const onTest = vi.fn();
  render(
    <ProxyTestBlock
      state={state}
      onTest={onTest}
      onTestCustom={() => {}}
      operator={operator}
      probeLine=""
      customReject={null}
      customDraft=""
      onCustomDraftChange={() => {}}
    />,
  );
  return { tile: screen.getByTestId("connectivity-tile"), onTest };
}

describe("ProxyTestBlock — the Connectivity tile", () => {
  it.each([
    ["not tested", IDLE, "Not tested", "Test connectivity"],
    ["reached via a proxy", done({ state: "reached", via: "proxy" }), "Reached · via proxy", "Test again"],
    ["blocked", done({ state: "blocked" }), "Blocked", "Test again"],
    ["no runner", done({ state: "no_runner" }), "Can't test here", "Test again"],
  ])("%s: the pill and the button are both inside the tile", (_n, state, pill, button) => {
    const { tile } = renderTile(state);
    expect(within(tile).getByRole("heading", { name: T.TILE_TITLE })).toBeInTheDocument();
    expect(within(tile).getByText(pill)).toBeInTheDocument();
    expect(within(tile).getByRole("button", { name: button })).toBeEnabled();
  });

  it("testing: a Testing… pill, and the button is held", () => {
    const { tile } = renderTile({ kind: "running", elapsedSec: 3, custom: false });
    expect(within(tile).getAllByText("Testing…")).toHaveLength(2);
    expect(within(tile).getByRole("button", { name: "Testing…" })).toBeDisabled();
    expect(within(tile).getByText(T.GATE_RUNNING)).toBeInTheDocument();
  });

  it("says what each answer means in one line: untested and reached", () => {
    expect(renderTile(IDLE).tile).toHaveTextContent(T.TILE_UNTESTED);
  });

  it("a blocked probe wears the danger pill, not a warning one", () => {
    const { tile } = renderTile(done({ state: "blocked" }));
    expect(within(tile).getByText("Blocked").className).toMatch(/text-danger/);
  });

  it("a viewer's button is held and says why", () => {
    const { tile } = renderTile(IDLE, false);
    expect(within(tile).getByRole("button", { name: "Test connectivity" })).toBeDisabled();
    expect(within(tile).getByText(T.VIEWER_HINT)).toBeInTheDocument();
  });
});
