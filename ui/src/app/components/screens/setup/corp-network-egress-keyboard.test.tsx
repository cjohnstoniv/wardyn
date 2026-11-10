/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A collapsed redirect row is a role=button that expands on Enter/Space. Its Test and Remove buttons sit
// inside it: a key pressed on them must run them, not expand the row.
import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { OperatorProvider } from "../../wardyn/operator-context";
import { CorpNetworkStep } from "./corp-network-step";
import { baseStatus } from "../../../lib/test-fixtures";
import { health } from "../../../lib/api/health";

vi.mock("../../../lib/api/health", () => ({ health: { testProxy: vi.fn(), testRedirect: vi.fn() } }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const REDIRECTS = [{ from: "a.example.com", to: "mirror-a.corp.internal" }];

function renderEgress(save = vi.fn().mockResolvedValue(undefined)) {
  render(
    <MemoryRouter>
      <OperatorProvider operator>
        <CorpNetworkStep
          status={baseStatus()}
          siteConfig={{ egress_redirects: REDIRECTS }}
          reloadSiteConfig={vi.fn().mockResolvedValue(undefined)}
          saveSiteConfig={save}
          gate={{ probeRunning: false, customDraft: "", redirectProbes: {} }}
          onGateChange={vi.fn()}
          tab="egress"
        />
      </OperatorProvider>
    </MemoryRouter>,
  );
  return save;
}

describe("Egress redirect row keyboard", () => {
  it("Enter on the Test button runs the probe and leaves the row collapsed", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    vi.mocked(health.testRedirect).mockResolvedValue({ state: "reached", detail: "ok" } as never);
    renderEgress();
    screen.getByRole("button", { name: "Test" }).focus();
    await user.keyboard("{Enter}");
    await waitFor(() => expect(health.testRedirect).toHaveBeenCalledWith("a.example.com", "mirror-a.corp.internal"));
    expect(document.getElementById("eg-edit-token")).toBeNull();
  });

  it("Space on the Remove button removes the redirect instead of expanding the row", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const save = renderEgress();
    screen.getByRole("button", { name: "Remove a.example.com redirect" }).focus();
    await user.keyboard(" ");
    await waitFor(() => expect(save).toHaveBeenCalled());
    expect(document.getElementById("eg-edit-token")).toBeNull();
  });

  it("Enter on the row itself still expands it", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderEgress();
    screen.getByText("a.example.com").closest<HTMLElement>('[role="button"]')!.focus();
    await user.keyboard("{Enter}");
    expect(await screen.findByLabelText(/token secret name/i, { selector: "#eg-edit-token" })).toBeInTheDocument();
  });
});
