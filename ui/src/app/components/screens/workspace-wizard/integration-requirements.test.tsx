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
  category: "package_feed",
  type: "artifactory",
  hosts: ["artifactory.corp.internal"],
  header: "Authorization",
  credentials: { token: "artifactory-token" },
};

function renderSection(integrations: WireIntegration[], requirements: WorkspaceRequirementsMap = {}) {
  const setLane = vi.fn();
  const clear = vi.fn();
  render(
    <IntegrationRequirements
      status={{ integrations } as unknown as SetupStatus}
      requirements={requirements}
      setLane={setLane}
      clear={clear}
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
    await userEvent.click(screen.getByRole("button", { name: /add to this workspace/i }));
    expect(setLane).toHaveBeenCalledWith("integration:corp-artifactory", "required");
  });

  it("shows a named integration as carrying its hosts and credential", () => {
    renderSection([FEED], { "integration:corp-artifactory": required });
    expect(screen.getByText(/hosts and credential ride along/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /add to this workspace/i })).not.toBeInTheDocument();
  });

  // Removing a row is ABSENCE, not a third lane.
  it("removes a row rather than inventing an off state", async () => {
    const { clear } = renderSection([FEED], { "integration:corp-artifactory": required });
    await userEvent.click(screen.getByRole("button", { name: /remove/i }));
    expect(clear).toHaveBeenCalledWith("integration:corp-artifactory");
  });

  // A workspace may name an integration before it exists, or outlive one.
  // Either way the contract states an intent and opens nothing meanwhile.
  it("keeps showing a named integration that is not configured, and says what that means", () => {
    renderSection([], { "integration:not-configured-yet": required });
    expect(screen.getByText("not-configured-yet")).toBeInTheDocument();
    expect(screen.getByText(/opens nothing until it exists/i)).toBeInTheDocument();
  });
});
