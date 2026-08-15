/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A workspace naming an integration instead of restating its hosts and secret
// names. The claim under test: one name, and the hosts and the credential ride
// along — plus the honest state when a named integration no longer exists.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { IntegrationRequirements, namedIntegrationIds } from "./integration-requirements";
import type { SetupStatus, WireIntegration } from "../../../lib/types/setup";
import type { WorkspaceRequirementsMap } from "./wizard-types";

const FEED: WireIntegration = {
  id: "corp-artifactory",
  name: "Corp Artifactory",
  kind: "artifactory",
  egress: ["artifactory.corp.internal"],
  secrets: [
    { role: "token", secret_name: "artifactory-token", delivery: { mode: "proxy_header", header: "Authorization" } },
  ],
};

function renderSection(
  integrations: WireIntegration[],
  requirements: WorkspaceRequirementsMap = {},
  namedOnly = false,
) {
  const setLane = vi.fn();
  const clear = vi.fn();
  render(
    <IntegrationRequirements
      status={{ integrations } as unknown as SetupStatus}
      requirements={requirements}
      setLane={setLane}
      clear={clear}
      namedOnly={namedOnly}
    />,
  );
  return { setLane, clear };
}

const required = { level: "required" as const, provenance: "operator_set" as const };

describe("namedIntegrationIds", () => {
  it("reads only the integration keys, leaving the other requirement types alone", () => {
    expect(
      namedIntegrationIds({
        "integration:corp-artifactory": required,
        "egress:api.stripe.com": required,
        "secret:acme-key": required,
      }),
    ).toEqual(["corp-artifactory"]);
  });
});

describe("IntegrationRequirements", () => {
  it("says nothing at all when there is nothing to name", () => {
    const { container } = render(
      <IntegrationRequirements status={null} requirements={{}} setLane={vi.fn()} clear={vi.fn()} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("offers a configured integration and adds it as required", async () => {
    const { setLane } = renderSection([FEED]);
    expect(screen.getByText("Corp Artifactory")).toBeInTheDocument();
    expect(screen.getByText("artifactory.corp.internal")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /use in this workspace/i }));
    expect(setLane).toHaveBeenCalledWith("integration:corp-artifactory", "required");
  });

  it("shows a named integration as carrying its hosts and credential", () => {
    renderSection([FEED], { "integration:corp-artifactory": required });
    expect(screen.getByText(/hosts and credential ride along/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /use in this workspace/i })).not.toBeInTheDocument();
  });

  // ui-wsWizard-3: the generic-integrations section used bare, hand-styled
  // <button> elements for add/remove where step-integrations.tsx's NamedRow
  // (the AI/SCM rows above it, same step) used the shared Button component
  // with "Use in this workspace" / "Not used" — two button systems and two
  // label pairs for the identical action, stacked in one card list.
  it("the add/remove action is the shared Button component with the NamedRow label pair", () => {
    renderSection([FEED]);
    expect(screen.getByRole("button", { name: "Use in this workspace" })).toHaveAttribute("data-slot", "button");
  });

  // Removing a row is ABSENCE, not a third lane.
  it("removes a row rather than inventing an off state", async () => {
    const { clear } = renderSection([FEED], { "integration:corp-artifactory": required });
    await userEvent.click(screen.getByRole("button", { name: /not used/i }));
    expect(clear).toHaveBeenCalledWith("integration:corp-artifactory");
  });

  // A workspace may name an integration before it exists, or outlive one.
  // Either way the contract states an intent and opens nothing meanwhile.
  it("keeps showing a named integration that is not configured, and says what that means", () => {
    renderSection([], { "integration:not-configured-yet": required });
    expect(screen.getByText("not-configured-yet")).toBeInTheDocument();
    expect(screen.getByText(/opens nothing until it exists/i)).toBeInTheDocument();
  });

  // The live lie this pins: an adopted AI row (picked on step ③) is filtered
  // out of the generic picker by DESIGN, but it is stored — the existence
  // check must consult every stored integration, never the picker subset.
  it("a named AI/SCM row that IS stored renders configured, never 'not configured'", async () => {
    const sub: WireIntegration = {
      id: "anthropic_subscription:managed",
      name: "Claude subscription (managed)",
      kind: "anthropic_subscription",
    };
    const { setLane, clear } = renderSection([sub], {
      "integration:anthropic_subscription:managed": required,
    });
    expect(screen.getByText("Claude subscription (managed)")).toBeInTheDocument();
    expect(screen.queryByText(/not configured/i)).not.toBeInTheDocument();
    // Full lane parity with any other named row: re-lane and remove both work.
    await userEvent.click(screen.getByRole("radio", { name: "Optional" }));
    expect(setLane).toHaveBeenCalledWith("integration:anthropic_subscription:managed", "optional");
    await userEvent.click(screen.getByRole("button", { name: /remove/i }));
    expect(clear).toHaveBeenCalledWith("integration:anthropic_subscription:managed");
  });

  // The wizard's Reach: step ③ owns picking, so no picker here — but the rows
  // the contract names still render, resolved against the STORED set (this
  // mode with no status was exactly the 'not configured' lie).
  it("namedOnly hides the picker but keeps named rows honest", () => {
    renderSection([FEED], { "integration:corp-artifactory": required }, true);
    // No picker: an un-named stored feed offers nothing to add.
    expect(screen.queryByRole("button", { name: /use in this workspace/i })).not.toBeInTheDocument();
    // The named row resolves against the stored set: real name, no warning.
    expect(screen.getByText("Corp Artifactory")).toBeInTheDocument();
    expect(screen.queryByText(/not configured/i)).not.toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "Required", checked: true })).toBeInTheDocument();
  });
});
