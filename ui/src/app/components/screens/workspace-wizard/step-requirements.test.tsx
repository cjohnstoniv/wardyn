/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, within } from "@testing-library/react";

const setSecretMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: { setSecret: (...a: unknown[]) => setSecretMock(...a) },
}));

import { StepRequirements } from "./step-requirements";
import { deriveInitialRequirements, newSourceRow, type SourceRow, type WorkspaceRequirementsMap } from "./wizard-types";
import { C } from "../../../lib/workspace-copy";
import type { WorkspaceProfile } from "../../../lib/types";

function Harness({
  profile,
  sources = [],
  initialRequirements,
  storedSecretNames = [],
}: {
  profile: WorkspaceProfile | null | undefined;
  sources?: SourceRow[];
  initialRequirements?: WorkspaceRequirementsMap;
  storedSecretNames?: string[];
}) {
  const localDirPaths = sources.filter((s) => s.type === "local_dir").map((s) => s.path);
  const [requirements, setRequirements] = React.useState<WorkspaceRequirementsMap>(
    initialRequirements ?? deriveInitialRequirements(profile, localDirPaths),
  );
  const [stored, setStored] = React.useState<string[]>(storedSecretNames);
  return (
    <StepRequirements
      profile={profile}
      sources={sources}
      requirements={requirements}
      onChange={setRequirements}
      storedSecretNames={stored}
      onSecretStored={(n) => setStored((prev) => [...prev, n])}
    />
  );
}

beforeEach(() => {
  setSecretMock.mockReset();
  setSecretMock.mockResolvedValue(undefined);
});

describe("StepRequirements — empty / no-profile states", () => {
  it("shows C.NO_CONTRACT when the scan hasn't produced a profile yet", () => {
    render(<Harness profile={null} />);
    expect(screen.getByText(C.NO_CONTRACT)).toBeInTheDocument();
  });

  it("shows C.EMPTY_SCAN when the scan found nothing this workspace needs", () => {
    render(<Harness profile={{}} />);
    expect(screen.getByText(C.EMPTY_SCAN)).toBeInTheDocument();
  });

  it("always states the Required/Optional axis at the top, even in an empty state", () => {
    render(<Harness profile={null} />);
    expect(screen.getByText(C.REQ_DEF)).toBeInTheDocument();
    expect(screen.getByText(C.OPT_DEF)).toBeInTheDocument();
    expect(screen.getByText(C.SEEDED)).toBeInTheDocument();
  });
});

describe("StepRequirements — the full stack's lane controls", () => {
  const profile: WorkspaceProfile = {
    required_secrets: [{ name: "DATABASE_URL", kind: "postgres" }],
    egress_domains: ["registry.npmjs.org"],
    suggested_egress: ["telemetry.segment.io"],
    services_needed: ["postgres:15"],
    leak_findings: [{ path: "src/config/dev.ts", kind: "aws-access-key", line: 42 }],
  };

  it("pins the leak-findings banner above everything, with C.LOCATION_ONLY and no secret value ever shown", () => {
    render(<Harness profile={profile} />);
    const banner = screen.getByTestId("leak-banner");
    expect(within(banner).getByText("src/config/dev.ts:42 — aws-access-key")).toBeInTheDocument();
    expect(within(banner).getByText(C.LOCATION_ONLY)).toBeInTheDocument();
  });

  it("a not-stored secret shows the pill + Add, which opens the locked AddSecretDialog for its exact name", () => {
    render(<Harness profile={profile} />);
    const group = screen.getByTestId("group-secrets");
    expect(within(group).getByText("not stored yet")).toBeInTheDocument();

    fireEvent.click(within(group).getByRole("button", { name: "Add" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByLabelText(/name/i)).toHaveValue("DATABASE_URL");
  });

  it("a stored secret shows the stored pill instead, with no Add button", () => {
    render(<Harness profile={profile} storedSecretNames={["DATABASE_URL"]} />);
    const group = screen.getByTestId("group-secrets");
    expect(within(group).getByText("stored")).toBeInTheDocument();
    expect(within(group).queryByRole("button", { name: "Add" })).not.toBeInTheDocument();
  });

  it("the lane toggle flips a secret between required and optional, clearing the unmet-required line once optional", () => {
    render(<Harness profile={profile} />);
    const laneGroup = screen.getByRole("radiogroup", { name: "DATABASE_URL lane" });
    // Defaults to required (no `optional` flag on this SecretNeed).
    expect(within(laneGroup).getByRole("radio", { name: "Required" })).toBeChecked();
    expect(screen.getByText(C.UNMET_OK, { exact: false })).toBeInTheDocument();

    fireEvent.click(within(laneGroup).getByRole("radio", { name: "Optional" }));
    expect(within(laneGroup).getByRole("radio", { name: "Optional" })).toBeChecked();
    // The unmet-required line only applies while the secret is BOTH required
    // and unstored — it disappears once optional is picked.
    expect(screen.queryByText(C.UNMET_OK, { exact: false })).not.toBeInTheDocument();
  });

  it("shows the unmet-required line for a required secret that isn't stored", () => {
    render(<Harness profile={profile} />);
    expect(screen.getByText(/1 required secret isn't stored yet\./)).toBeInTheDocument();
    expect(screen.getByText(C.UNMET_OK, { exact: false })).toBeInTheDocument();
  });

  it("egress: an auto-allowed host defaults to required, and the holding block's Approve needs the untrusted-content confirm", () => {
    render(<Harness profile={profile} />);
    const egressGroup = screen.getByTestId("group-egress");
    expect(within(egressGroup).getByText("registry.npmjs.org")).toBeInTheDocument();

    const holding = screen.getByTestId("holding-block");
    expect(within(holding).getByText(C.HOLDING)).toBeInTheDocument();
    fireEvent.click(within(holding).getByRole("button", { name: "Approve" }));

    // The confirm dialog gates the promotion — it isn't in the contract yet.
    const confirmDialog = screen.getByRole("alertdialog");
    expect(within(confirmDialog).getByText(/approve egress to telemetry\.segment\.io/i)).toBeInTheDocument();
    expect(screen.queryByRole("radiogroup", { name: "telemetry.segment.io lane" })).not.toBeInTheDocument();

    fireEvent.click(within(confirmDialog).getByRole("button", { name: /approve host/i }));
    // Promoted into the egress group with a lane control, default required.
    const promotedLane = screen.getByRole("radiogroup", { name: "telemetry.segment.io lane" });
    expect(within(promotedLane).getByRole("radio", { name: "Required" })).toBeChecked();
  });

  it("services render names only, with no lane control", () => {
    render(<Harness profile={profile} />);
    const group = screen.getByTestId("group-services");
    expect(within(group).getByText("postgres:15")).toBeInTheDocument();
    expect(within(group).queryByRole("radio")).toBeNull();
    expect(within(group).getByText(C.SERVICES)).toBeInTheDocument();
  });

  it("shows the blind-spot line and a raw-profile disclosure", () => {
    render(<Harness profile={profile} />);
    expect(screen.getByText(C.BLIND_SPOT)).toBeInTheDocument();
    expect(screen.getByText("Raw profile")).toBeInTheDocument();
  });
});

describe("StepRequirements — host directory write rows, one per local_dir source", () => {
  const sources: SourceRow[] = [
    { ...newSourceRow("local_dir"), path: "/home/me/payments" },
    { ...newSourceRow("local_dir"), path: "/home/me/fixtures" },
  ];

  it("renders one write row per local_dir source, defaulting to optional", () => {
    render(<Harness profile={{}} sources={sources} />);
    expect(screen.getByText("/home/me/payments")).toBeInTheDocument();
    expect(screen.getByText("/home/me/fixtures")).toBeInTheDocument();
    const optionalRadios = screen.getAllByRole("radio", { name: "Optional" });
    expect(optionalRadios.length).toBeGreaterThanOrEqual(2);
    expect(screen.getAllByText(/runs mount it read-only/)).toHaveLength(2);
  });

  it("flips the consequence line to the required warning when its lane is switched", () => {
    render(<Harness profile={{}} sources={[sources[0]]} />);
    const laneGroup = screen.getByRole("radiogroup", { name: "write access to /home/me/payments" });
    fireEvent.click(within(laneGroup).getByRole("radio", { name: "Required" }));
    expect(screen.getByText(/every run can change these files on your machine/)).toBeInTheDocument();
  });
});
