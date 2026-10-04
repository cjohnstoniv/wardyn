/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// deny-f4: the Request access remedy on the New Run rail, under a launch
// refusal (from the error's own `policy`) and beside the governance profile line
// (from GET /me's governance_contact).
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

vi.mock("../../../lib/api/health", () => ({
  health: { health: () => Promise.resolve({ components: {} }) },
}));

import { RunRail } from "./new-run-rail";
import { AUTONOMY_RAIL } from "../../../lib/governance-copy";
import type { PolicyRef } from "../../../lib/api/health";

const CONTACT: PolicyRef = {
  source: "profile",
  name: "Contractors",
  owner: "Platform Security",
  request_url: "https://example.com/access",
};

function rail(props: { governanceContact?: PolicyRef | null; launchError?: string; launchPolicy?: PolicyRef }) {
  return render(
    <MemoryRouter>
      <RunRail
        cc="CC1"
        governanceProfile="Contractors"
        governanceContact={props.governanceContact}
        showModelWarning={false}
        startup="It starts."
        showHoldNote={false}
        toolRules={null}
        unattended={false}
        launch={{
          onLaunch: () => {},
          disabled: false,
          spinning: false,
          inFlight: false,
          problem: null,
          error: props.launchError ?? null,
          errorSeq: 1,
          policy: props.launchPolicy,
          credentialRefused: false,
        }}
        preflight={{
          error: null,
          errorSeq: 0,
          result: {
            setup_items: [],
            enforced_confinement_class: "CC1",
            autonomy: { level: "L1", posture: { egress: "sealed", secrets: "powerful", confinement: "CC1" }, bound_by: ["secrets_powerful"] },
          },
        }}
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

describe("New run rail — Request access remedy", () => {
  it("shows the remedy under a launch refusal that names a policy", () => {
    rail({ launchError: "Too many runs at once.", launchPolicy: CONTACT });
    expect(screen.getByRole("alert")).toHaveTextContent("Too many runs at once.");
    expect(screen.getByTestId("policy-remedy")).toHaveTextContent("Owned by Platform Security · Request access");
  });

  it("shows nothing extra under a refusal with no policy", () => {
    rail({ launchError: "Too many runs at once." });
    expect(screen.queryByTestId("policy-remedy")).toBeNull();
  });

  it("shows the remedy beside the governance profile line from Me.governance_contact", () => {
    rail({ governanceContact: CONTACT });
    expect(screen.getByText(AUTONOMY_RAIL.PROFILE_LINE("Contractors"))).toBeInTheDocument();
    expect(screen.getByTestId("policy-remedy")).toHaveTextContent("Owned by Platform Security");
  });

  it("shows nothing there when governance_contact is null or absent", () => {
    rail({ governanceContact: null });
    expect(screen.getByText(AUTONOMY_RAIL.PROFILE_LINE("Contractors"))).toBeInTheDocument();
    expect(screen.queryByTestId("policy-remedy")).toBeNull();
  });
});
