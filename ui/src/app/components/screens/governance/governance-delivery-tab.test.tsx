/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Runner credentials tab of the Governance screen: where it sits, who sees it, and that its editor is read
// only once, on first visit, and kept (hidden) while another tab is open. The editor itself is
// runner-credentials.test.tsx.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

vi.mock("../../../lib/api/runs", () => ({ runs: { gradePolicy: () => Promise.resolve({ overall_risk: "medium", risk_assessment: [] }) } }));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

const getGovernanceMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return { ...actual, governance: { getGovernance: () => getGovernanceMock(), listChanges: () => Promise.resolve([]) } };
});

const deliveryGetMock = vi.fn();
vi.mock("../../../lib/api/credential-delivery", () => ({
  credentialDelivery: { get: () => deliveryGetMock(), put: vi.fn(), runnersEnabled: () => Promise.resolve(true) },
}));

import { BASELINE, CHANGES, DELIVERY, GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { GovernanceScreen } from "./governance-screen";

const GREENFIELD = {
  id: "p1",
  name: "Greenfield contractors",
  ceiling: { allowed_domains: ["api.anthropic.com"], first_use_approval: "deny_with_review", min_confinement_class: "CC2" },
  limits: {},
  created_at: "2026-10-12T00:00:00Z",
  updated_at: "2026-10-12T00:00:00Z",
};

// The screen's ONE `default` button; switches are not buttons.
const tealButtons = () => screen.getAllByRole("button").filter((b) => b.className.split(/\s+/).includes("bg-primary"));

beforeEach(() => {
  getGovernanceMock.mockReset().mockResolvedValue({ profiles: [GREENFIELD], assignments: [] });
  deliveryGetMock.mockReset().mockResolvedValue({ policy: { classes: {} }, updated_at: "", updated_by: "" });
});

describe("GovernanceScreen — the Runner credentials tab", () => {
  const renderAt = (url: string, securityOperator = true) => {
    render(
      <MemoryRouter initialEntries={[url]}>
        <OperatorProvider operator={securityOperator} securityOperator={securityOperator}>
          <GovernanceScreen />
        </OperatorProvider>
      </MemoryRouter>,
    );
  };

  it("sits between Profiles and Changes for a security operator, beside Baseline hosts", async () => {
    renderAt("/");
    await screen.findByText(GREENFIELD.name);
    expect(screen.getAllByRole("tab").map((t) => t.textContent)).toEqual([GOV.PROFILES_TITLE, DELIVERY.TAB, BASELINE.TAB, CHANGES.TAB]);
  });

  it("is absent without the security tier, and a typed URL falls back to Profiles", async () => {
    renderAt("/?tab=runner-credentials", false);
    await screen.findByText(GREENFIELD.name);
    expect(screen.queryByRole("tab", { name: DELIVERY.TAB })).toBeNull();
    expect(screen.getByRole("tab", { name: GOV.PROFILES_TITLE })).toHaveAttribute("aria-selected", "true");
    expect(deliveryGetMock).not.toHaveBeenCalled();
  });

  it("opens the editor, reads the policy only then, and Save is the screen's one default", async () => {
    const user = userEvent.setup();
    renderAt("/");
    await screen.findByText(GREENFIELD.name);
    expect(deliveryGetMock).not.toHaveBeenCalled();
    await user.click(screen.getByRole("tab", { name: DELIVERY.TAB }));
    expect(await screen.findByText(DELIVERY.LEAD)).toBeInTheDocument();
    expect(deliveryGetMock).toHaveBeenCalledTimes(1);
    expect(tealButtons().map((b) => b.textContent)).toEqual([DELIVERY.SAVE]);
  });

  it("keeps the editor mounted, hidden, when another tab is opened", async () => {
    const user = userEvent.setup();
    renderAt("/?tab=runner-credentials");
    await screen.findByText(DELIVERY.LEAD);
    await user.click(screen.getByRole("tab", { name: CHANGES.TAB }));
    expect(screen.getByText(DELIVERY.LEAD)).not.toBeVisible();
    await user.click(screen.getByRole("tab", { name: DELIVERY.TAB }));
    expect(screen.getByText(DELIVERY.LEAD)).toBeVisible();
    expect(deliveryGetMock).toHaveBeenCalledTimes(1);
  });
});
