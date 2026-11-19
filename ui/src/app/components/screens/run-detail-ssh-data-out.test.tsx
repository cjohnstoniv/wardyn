/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The SSH card on a run whose data_out rule keeps content in: the web app's refusal prints the server's
// sentence under the app's row, and the Files panel is mounted for a run with no desktop only under review.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { RunDetail } from "../../lib/types";

const healthMock = vi.fn();
vi.mock("./run-detail/desktop-files-panel", () => ({
  DesktopFilesPanel: ({ noDesktop }: { noDesktop?: boolean }) => (noDesktop ? <div data-testid="files-no-desktop" /> : null),
}));
vi.mock("../../lib/api/health", () => ({ health: { health: (...a: unknown[]) => healthMock(...a) } }));
vi.mock("../../lib/api/ssh-keys", () => ({ sshKeys: { listKeys: () => Promise.resolve([]) } }));
const attachTicketMock = vi.fn();
const getPolicyMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: { attachTicket: (...a: unknown[]) => attachTicketMock(...a), getPolicy: (...a: unknown[]) => getPolicyMock(...a) },
}));

import { ConnectSSHCard } from "./run-detail-ssh";
import { HttpError } from "../../lib/api/core";
import { OperatorProvider } from "../wardyn/operator-context";
import { UI_APPS_LANE } from "../wardyn/copy";
import { aheadByHours } from "../../lib/test-clock";

const OWNER = "alice@example.com";
const baseRun: RunDetail = {
  id: "11111111-1111-1111-1111-111111111111",
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
  created_by: OWNER,
  agent: "claude-code",
  repo: "acme/widgets",
  task: "do the thing",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/11111111-1111-1111-1111-111111111111",
  runner_target: "docker",
  placement: "",
  interactive: false,
};
const recorded = (data_out?: string) => ({ run_id: "r", state: "recorded", source: { kind: "default" }, spec: data_out ? { data_out } : {}, redacted: false, changes: [] });

function renderCard(run: Partial<RunDetail> = {}) {
  return render(
    <MemoryRouter>
      <OperatorProvider operator principal={OWNER}>
        <ConnectSSHCard run={{ ...baseRun, ...run }} />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

let openSpy: ReturnType<typeof vi.spyOn>;
beforeEach(() => {
  healthMock.mockReset();
  healthMock.mockResolvedValue({ status: "ok", ui_sandbox: { enabled: true, enter_url_template: "http://ui.local/__wardyn/enter?run={run}&app={app}&ticket={ticket}" } });
  attachTicketMock.mockReset();
  getPolicyMock.mockReset();
  getPolicyMock.mockResolvedValue(recorded());
  openSpy = vi.spyOn(window, "open").mockReturnValue({ opener: {}, close: vi.fn(), location: { replace: vi.fn() } } as unknown as Window);
});
afterEach(() => openSpy.mockRestore());

describe("ConnectSSHCard under a data_out rule that keeps content in", () => {
  // The server's own sentence for a held rule is the body under the app's row, as sent.
  it("prints a 403 data_out_blocked refusal under the app's row: the title and the server's sentence", async () => {
    const sentence = "This run's data_out rule keeps content in, so its web apps are off; its desktop still opens.";
    attachTicketMock.mockRejectedValue(new HttpError(403, sentence, "data_out_blocked"));
    renderCard({ ui_apps: [{ name: "vscode", port: 8080 }] });
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    screen.getByRole("button", { name: UI_APPS_LANE.cta("vscode") }).click();
    await screen.findByText(UI_APPS_LANE.errorTitle("vscode"));
    expect(screen.getByText(sentence)).toBeInTheDocument();
  });

  it("mounts the Files panel for a run with no UI app whose rule is review", async () => {
    getPolicyMock.mockResolvedValue(recorded("review"));
    renderCard();
    expect(await screen.findByTestId("files-no-desktop")).toBeInTheDocument();
  });

  it("mounts nothing while the rule is unread, allowed or blocked", async () => {
    for (const read of [() => new Promise(() => {}), () => Promise.resolve(recorded()), () => Promise.resolve(recorded("blocked"))]) {
      getPolicyMock.mockReset();
      getPolicyMock.mockImplementation(read);
      const { unmount } = renderCard();
      await waitFor(() => expect(getPolicyMock).toHaveBeenCalled());
      expect(screen.queryByTestId("files-no-desktop")).toBeNull();
      unmount();
    }
  });

  it("mounts nothing for a run that has a UI app, and does not read its rule", async () => {
    getPolicyMock.mockResolvedValue(recorded("review"));
    renderCard({ ui_apps: [{ name: "vscode", port: 8080 }] });
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(screen.queryByTestId("files-no-desktop")).toBeNull();
    expect(getPolicyMock).not.toHaveBeenCalled();
  });
});
