/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Desktop block of the attach card for a macOS VM run (mock M-D1 frames 5, 6, 7, 9, 10 and 11).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { RunDetail } from "../../lib/types";
import type { MacOSRunFacts } from "./run-detail/macos-run-facts";

const healthMock = vi.fn();
vi.mock("../../lib/api/health", () => ({ health: { health: (...a: unknown[]) => healthMock(...a) } }));
vi.mock("../../lib/api/ssh-keys", () => ({ sshKeys: { listKeys: async () => [] } }));
vi.mock("../../lib/api/runs", () => ({ runs: { attachTicket: vi.fn() } }));
const signInsGet = vi.fn();
const signInsPause = vi.fn();
vi.mock("../../lib/api/desktop-sign-ins", () => ({
  desktopSignIns: { get: (...a: unknown[]) => signInsGet(...a), pause: (...a: unknown[]) => signInsPause(...a), resume: vi.fn() },
}));

import { ConnectSSHCard } from "./run-detail-ssh";
import { OperatorProvider } from "../wardyn/operator-context";
import { UI_APPS_LANE } from "../wardyn/copy";
import { DESKTOP } from "../wardyn/copy/desktop";
import { aheadByHours } from "../../lib/test-clock";
import { shortTime } from "../../lib/format";

const OWNER = "alice@example.com";
const run: RunDetail = {
  id: "22222222-2222-2222-2222-222222222222",
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
  created_by: OWNER,
  agent: "claude-code",
  repo: "acme/widgets",
  task: "do the thing",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/22222222-2222-2222-2222-222222222222",
  runner_target: "docker",
  placement: "",
  interactive: true,
  machine_kind: "macos_vm",
};
const gatewayOn = { status: "ok", ui_sandbox: { enabled: true, enter_post_url: "http://ui.local/__wardyn/enter" } };
const runDisk = { store: "run_disk", paused: false, can_pause: true, agent: "Claude Code" };

const renderCard = (macOS?: MacOSRunFacts, over: Partial<RunDetail> = {}) =>
  render(
    <MemoryRouter>
      <OperatorProvider operator={false} principal={OWNER}>
        <ConnectSSHCard run={{ ...run, ...over }} macOS={macOS} />
      </OperatorProvider>
    </MemoryRouter>,
  );
const desk = () => screen.findByRole("region", { name: DESKTOP.title });

beforeEach(() => {
  healthMock.mockReset();
  signInsGet.mockReset();
  signInsPause.mockReset();
  healthMock.mockResolvedValue(gatewayOn);
  signInsGet.mockResolvedValue(runDisk);
});

describe("ConnectSSHCard: macOS desktop", () => {
  it("frame 5: Ready, the macOS kind line, the same Open and pause buttons, and the run-disk note last", async () => {
    renderCard();
    const d = await desk();
    expect(await within(d).findByRole("button", { name: DESKTOP.pauseToSignIn })).toBeEnabled();
    expect(within(d).getByText(DESKTOP.ready)).toBeInTheDocument();
    expect(within(d).getByText(DESKTOP.kindMacOS)).toBeInTheDocument();
    expect(within(d).queryByText(DESKTOP.kind)).toBeNull();
    expect(within(d).getByRole("button", { name: UI_APPS_LANE.cta("desktop") })).toBeInTheDocument();
    expect(within(d).getByText(UI_APPS_LANE.newTab)).toBeInTheDocument();
    expect(d.lastElementChild).toBe(within(d).getByText(DESKTOP.signInsNoteRunDisk));
    expect(within(d).queryByText(DESKTOP.signInsNote)).toBeNull();
  });

  it("frame 6: while paused the chip says Agent paused, with today's pause words and the macOS note", async () => {
    signInsGet.mockResolvedValue({ ...runDisk, paused: true });
    renderCard();
    const d = await desk();
    expect(await within(d).findByText(DESKTOP.agentPaused)).toBeInTheDocument();
    expect(within(d).getByText(DESKTOP.pausedNote)).toBeInTheDocument();
    expect(within(d).getByRole("button", { name: DESKTOP.resume })).toBeInTheDocument();
    expect(within(d).getByText(DESKTOP.signInsNoteRunDisk)).toBeInTheDocument();
  });

  it.each<[from: string, facts: MacOSRunFacts | undefined, over: Partial<RunDetail>]>([
    ["a prop", { desktop: "observe" }, {}],
    ["the run's own macos field", undefined, { macos: { desktop: "observe" } }],
  ])("frame 7: a view-only Screen Sharing is Off, says so, and has no Open desktop (from %s)", async (_from, facts, over) => {
    renderCard(facts, over);
    const d = await desk();
    expect(await within(d).findByText(DESKTOP.statusOff)).toBeInTheDocument();
    expect(within(d).getByText(DESKTOP.kindMacOS)).toBeInTheDocument();
    expect(within(d).getByText(DESKTOP.observeOnlyMacOS)).toBeInTheDocument();
    expect(within(d).queryByRole("button")).toBeNull();
    expect(within(d).queryByText(DESKTOP.off)).toBeNull();
  });

  it("a Mac whose Screen Sharing can be controlled keeps the Open desktop", async () => {
    renderCard(undefined, { macos: { desktop: "control" } });
    const d = await desk();
    expect(await within(d).findByRole("button", { name: UI_APPS_LANE.cta("desktop") })).toBeInTheDocument();
    expect(within(d).queryByText(DESKTOP.observeOnlyMacOS)).toBeNull();
  });

  it.each<[frame: string, disk: NonNullable<MacOSRunFacts["disk"]>, line: string]>([
    ["9", { state: "removing" }, DESKTOP.diskRemoving],
    ["10", { state: "removed", at: "14:32" }, DESKTOP.diskRemoved("14:32")],
    ["11", { state: "failed", reason: DESKTOP.diskReasonInUse }, DESKTOP.diskRemoveFailed(DESKTOP.diskReasonInUse)],
  ])("frame %s: at the run's end the block is Off and carries the disk line alone", async (_frame, disk, line) => {
    renderCard({ disk }, { state: "COMPLETED" });
    const d = await desk();
    expect(within(d).getByText(DESKTOP.statusOff)).toBeInTheDocument();
    expect(within(d).getByText(DESKTOP.kindMacOS)).toBeInTheDocument();
    expect(within(d).getByText(line)).toBeInTheDocument();
    expect(within(d).queryByRole("button")).toBeNull();
  });

  const removedAt = aheadByHours(-1);
  it.each<[frame: string, disk: NonNullable<RunDetail["macos_disk"]>, line: string]>([
    ["10", { state: "removed", at: removedAt }, DESKTOP.diskRemoved(shortTime(removedAt))],
    ["11", { state: "kept", reason: "helper_alive" }, DESKTOP.diskRemoveFailed(DESKTOP.diskReasonInUse)],
  ])("frame %s: the disk line comes from the run's own macos_disk, with no prop", async (_frame, disk, line) => {
    renderCard(undefined, { state: "COMPLETED", macos_disk: disk });
    const d = await desk();
    expect(within(d).getByText(DESKTOP.statusOff)).toBeInTheDocument();
    expect(within(d).getByText(line)).toBeInTheDocument();
    expect(within(d).queryByRole("button")).toBeNull();
  });
});
