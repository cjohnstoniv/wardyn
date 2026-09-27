/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #492: a refused Egress redirection save (a 412 from another tab's write,
// or any failure) must leave the operator's draft where they left it — the
// add form keeps From/To/token, the edit row stays open with its edits. Only
// a save that landed clears or collapses. Kept out of corp-network-step.test.tsx,
// which sits near the 1000-line file-size cap.
import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { SiteConfig } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";
import { CorpNetworkStep } from "./corp-network-step";
import { baseStatus } from "../../../lib/test-fixtures";

vi.mock("../../../lib/api/health", () => ({ health: { testProxy: vi.fn(), testRedirect: vi.fn() } }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const FROM = "https://registry.npmjs.org";

function renderEgress(siteConfig: SiteConfig, saveSiteConfig: (next: SiteConfig) => Promise<void>) {
  render(
    <MemoryRouter>
      <OperatorProvider operator>
        <CorpNetworkStep
          status={baseStatus()}
          siteConfig={siteConfig}
          reloadSiteConfig={vi.fn().mockResolvedValue(undefined)}
          saveSiteConfig={saveSiteConfig}
          gate={{ probeRunning: false, customDraft: "", redirectProbes: {} }}
          onGateChange={vi.fn()}
          tab="egress"
        />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

async function fillAddForm(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("combobox"));
  await user.click(await screen.findByText(FROM));
  await user.type(screen.getByPlaceholderText(/artifactory\.corp\.internal/i), "https://mirror.corp.internal");
  await user.type(screen.getByLabelText(/token secret name/i), "mirror-token");
  await user.click(screen.getByRole("button", { name: /\+ add redirect/i }));
}

describe("Egress redirection — a refused save keeps the draft (#492)", () => {
  it("add: a failed save leaves From, To and the token in the form", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const save = vi.fn().mockRejectedValue(new Error("saved elsewhere"));
    renderEgress({}, save);
    await fillAddForm(user);
    await waitFor(() => expect(save).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByDisplayValue("https://mirror.corp.internal")).toBeInTheDocument());
    expect(screen.getByRole("combobox")).toHaveTextContent(FROM);
    expect(screen.getByDisplayValue("mirror-token")).toBeInTheDocument();
  });

  it("add: a save that lands clears the form", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const save = vi.fn().mockResolvedValue(undefined);
    renderEgress({}, save);
    await fillAddForm(user);
    await waitFor(() => expect(save).toHaveBeenCalled());
    await waitFor(() => expect(screen.queryByDisplayValue("https://mirror.corp.internal")).not.toBeInTheDocument());
    expect(screen.queryByDisplayValue("mirror-token")).not.toBeInTheDocument();
    expect(screen.getByRole("combobox")).not.toHaveTextContent(FROM);
  });

  it("edit: a failed save keeps the row open with the operator's edits", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const save = vi.fn().mockRejectedValue(new Error("saved elsewhere"));
    renderEgress({ egress_redirects: [{ from: "a.example.com", to: "mirror-a.corp.internal" }] }, save);
    await user.click(screen.getByText("a.example.com"));
    const token = await screen.findByLabelText(/token secret name/i, { selector: "#eg-edit-token" });
    await user.type(token, "edited-token");
    await user.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(save).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByDisplayValue("edited-token")).toBeInTheDocument());
    expect(screen.getByText(/editing — full values/i)).toBeInTheDocument();
  });

  it("edit: a save that lands collapses the row", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const save = vi.fn().mockResolvedValue(undefined);
    renderEgress({ egress_redirects: [{ from: "a.example.com", to: "mirror-a.corp.internal" }] }, save);
    await user.click(screen.getByText("a.example.com"));
    await user.click(await screen.findByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(screen.queryByText(/editing — full values/i)).not.toBeInTheDocument());
  });
});
