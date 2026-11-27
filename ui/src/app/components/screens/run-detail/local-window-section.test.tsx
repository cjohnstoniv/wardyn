/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Desktop tile's window on your computer, for a Linux VM run: the command to copy, the open window in the Mac's
// words with the tile's chip and button following it, the server's refusal in place of the command, and the runs and
// installs that have no section.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { RunDetail, RunLocalWindow, SetupStatus } from "../../../lib/types";

const healthMock = vi.fn();
vi.mock("../../../lib/api/health", () => ({ health: { health: (...a: unknown[]) => healthMock(...a) } }));
vi.mock("../../../lib/api/runs", () => ({ runs: { getPolicy: () => new Promise(() => {}), attachTicket: vi.fn(), watchTicket: vi.fn() } }));
vi.mock("../../../lib/api/desktop-sign-ins", () => ({ desktopSignIns: { get: vi.fn(async () => ({ store: "memory", paused: false, can_pause: true })), pause: vi.fn(), resume: vi.fn() } }));
vi.mock("./desktop-files-panel", () => ({ DesktopFilesPanel: () => null }));

import { DesktopCard } from "../run-detail-desktop";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ModelAccessProvider } from "../../wardyn/model-access-context";
import { RUN_CLI, RUN_SSH, UI_APPS_LANE } from "../../wardyn/copy";
import { DESKTOP } from "../../wardyn/copy/desktop";
import { MAC_WINDOW } from "../../wardyn/copy/mac-window";
import { WINDOW_LOCAL } from "../../wardyn/copy/window-local";
import { baseStatus } from "../../../lib/test-fixtures";
import { shortTime } from "../../../lib/format";
import { aheadByHours } from "../../../lib/test-clock";

const OWNER = "alice@example.com";
const RUNNER = "dev-laptop";
const OPENED = aheadByHours(-0.5);
const run: RunDetail = {
  id: "44444444-4444-4444-4444-444444444444",
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
  created_by: OWNER,
  agent: "claude-code",
  repo: "acme/widgets",
  task: "do the thing",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/44444444-4444-4444-4444-444444444444",
  runner_target: "docker",
  placement: "local",
  interactive: true,
  machine_kind: "linux_vm",
  runner_name: RUNNER,
};

const card = (w: RunLocalWindow | undefined, over: Partial<RunDetail> = {}, on = true) => (
  <MemoryRouter>
    <ModelAccessProvider status={baseStatus({ features: { scheduled_runs: false, ...(on ? { local_window: true } : {}) } } as Partial<SetupStatus>)} onRefresh={() => {}}>
      <OperatorProvider operator={false} principal={OWNER}>
        <DesktopCard run={{ ...run, local_window: w, ...over }} />
      </OperatorProvider>
    </ModelAccessProvider>
  </MemoryRouter>
);
const desk = () => screen.findByRole("region", { name: DESKTOP.title });
const tile = (region: HTMLElement) => region.parentElement!.parentElement!;
const SERVER_SAYS = "This run is paused. You can open its window once it is running again.";

beforeEach(() => {
  healthMock.mockReset().mockResolvedValue({ status: "ok", ui_sandbox: { enabled: true, enter_post_url: "http://ui.local/__wardyn/enter" } });
});

describe("DesktopCard: the window on your computer", () => {
  it("no window: the title, the note and the command to copy, under Open desktop", async () => {
    render(card({ open: false }));
    const d = await desk();
    const section = within(d).getByTestId("local-window");
    expect(within(section).getByText(WINDOW_LOCAL.TITLE)).toBeInTheDocument();
    expect(within(section).getByText(WINDOW_LOCAL.NOTE)).toBeInTheDocument();
    expect(within(section).getByText(WINDOW_LOCAL.COMMAND(run.id))).toBeInTheDocument();
    expect(within(section).getByRole("button", { name: RUN_SSH.COPY_CLI })).toHaveTextContent(RUN_CLI.COPY);
    const open = within(d).getByRole("button", { name: UI_APPS_LANE.cta("desktop") });
    expect(open.compareDocumentPosition(section) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(await within(tile(d)).findByText(DESKTOP.ready)).toBeInTheDocument();
    expect(within(tile(d)).queryByText(MAC_WINDOW.openChip)).toBeNull();
  });

  it("a window open: the chip, Watch desktop, since when and on which runner, the hand-back and view-only lines, and no command", async () => {
    render(card({ open: true, opened_at: OPENED }));
    const d = await desk();
    const section = within(d).getByTestId("local-window");
    expect(within(section).getByText(WINDOW_LOCAL.OPEN_SINCE(RUNNER, shortTime(OPENED)))).toBeInTheDocument();
    expect(within(section).getByText(MAC_WINDOW.handBack(RUNNER))).toBeInTheDocument();
    expect(within(section).getByText(MAC_WINDOW.viewOnlyWhileOpen)).toBeInTheDocument();
    expect(within(section).queryByText(WINDOW_LOCAL.COMMAND(run.id))).toBeNull();
    expect(within(section).queryByText(WINDOW_LOCAL.NOTE)).toBeNull();
    expect(within(tile(d)).getByText(MAC_WINDOW.openChip)).toBeInTheDocument();
    expect(within(d).getByRole("button", { name: DESKTOP.watch })).toBeInTheDocument();
    expect(within(d).queryByRole("button", { name: UI_APPS_LANE.cta("desktop") })).toBeNull();
    // The Mac's lines name the Mac: neither shows on a Linux VM.
    expect(within(d).queryByText(MAC_WINDOW.watchLine)).toBeNull();
    expect(within(d).queryByText(DESKTOP.observeOnlyMacOS)).toBeNull();
  });

  it("a window open on a run whose runner has no name: this run's runner", async () => {
    render(card({ open: true, opened_at: OPENED }, { runner_name: undefined }));
    const section = within(await desk()).getByTestId("local-window");
    expect(within(section).getByText(WINDOW_LOCAL.OPEN_SINCE(WINDOW_LOCAL.RUNNER_FALLBACK, shortTime(OPENED)))).toBeInTheDocument();
    expect(within(section).getByText(MAC_WINDOW.handBack(WINDOW_LOCAL.RUNNER_FALLBACK))).toBeInTheDocument();
  });

  it("refused: the server's sentence, raw, in place of the note and the command", async () => {
    render(card({ open: false, message: SERVER_SAYS }));
    const d = await desk();
    const section = within(d).getByTestId("local-window");
    expect(within(section).getByText(WINDOW_LOCAL.TITLE)).toBeInTheDocument();
    expect(within(section).getByText(SERVER_SAYS)).toBeInTheDocument();
    expect(within(section).queryByText(WINDOW_LOCAL.NOTE)).toBeNull();
    expect(within(section).queryByText(WINDOW_LOCAL.COMMAND(run.id))).toBeNull();
    expect(within(d).getByRole("button", { name: UI_APPS_LANE.cta("desktop") })).toBeInTheDocument();
  });

  it("no section with the switch off, on a background run, or with no local_window in the run read", async () => {
    for (const [w, over, on] of [
      [{ open: false }, {}, false],
      [{ open: false }, { experience: "background" }, true],
      [undefined, {}, true],
    ] as const) {
      const { unmount } = render(card(w, over, on));
      await desk();
      expect(screen.queryByTestId("local-window")).toBeNull();
      expect(screen.queryByText(WINDOW_LOCAL.TITLE)).toBeNull();
      unmount();
    }
  });
});
