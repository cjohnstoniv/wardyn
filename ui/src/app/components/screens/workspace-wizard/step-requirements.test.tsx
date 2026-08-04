/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, within, cleanup } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const setSecretMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: { setSecret: (...a: unknown[]) => setSecretMock(...a) },
}));

import { StepRequirements } from "./step-requirements";
import { deriveInitialRequirements, newSourceRow, type PowerSource, type SourceRow, type WorkspaceRequirementsMap } from "./wizard-types";
import { C, RD2 } from "../../../lib/workspace-copy";
import type { WorkspaceProfile } from "../../../lib/types";

// Radix Tabs activates a trigger on mousedown (not click) — fireEvent.click
// alone never fires that, so switching tabs needs real userEvent.
async function openTab(name: string) {
  const user = userEvent.setup();
  await user.click(screen.getByRole("tab", { name }));
}

function Harness({
  profile,
  sources = [],
  initialRequirements,
  storedSecretNames = [],
  powerSource = { kind: "default" },
}: {
  profile: WorkspaceProfile | null | undefined;
  sources?: SourceRow[];
  initialRequirements?: WorkspaceRequirementsMap;
  storedSecretNames?: string[];
  powerSource?: PowerSource;
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
      powerSource={powerSource}
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

  it("states the Required/Optional axis on the Secrets tab, where the lanes it explains live", async () => {
    render(
      <Harness profile={{ required_secrets: [{ name: "DATABASE_URL", kind: "postgres" }] }} />,
    );
    await openTab("Secrets");
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

  it("pins the leak banner above everything (not tab-scoped) — and Reach, not Record, is the default tab", () => {
    render(<Harness profile={profile} />);
    const banner = screen.getByTestId("leak-banner");
    // src/config/dev.ts is not a test-conventional path, so it is the RED tier.
    const hot = within(banner).getByTestId("leak-hot");
    expect(within(hot).getByText("src/config/dev.ts:42 — aws-access-key")).toBeInTheDocument();
    expect(within(hot).getByText(C.LOCATION_ONLY)).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Reach", selected: true })).toBeInTheDocument();
  });

  // The two real-use false alarms this tier exists for: a fixture in a test
  // file collapses to the muted tier; the red tier keeps only real paths.
  it("splits test-path findings into the muted collapsed tier — counted, expandable, never red", () => {
    render(
      <Harness
        profile={{
          ...profile,
          leak_findings: [
            { path: "src/config/dev.ts", kind: "aws-access-key", line: 42 },
            { path: "internal/broker/sshkey_test.go", kind: "private-key-block", line: 35 },
            { path: "internal/api/testdata/llm_transport_golden.json", kind: "aws-access-key", line: 65 },
          ],
        }}
      />,
    );
    const hot = screen.getByTestId("leak-hot");
    expect(within(hot).getByText(/src\/config\/dev\.ts:42/)).toBeInTheDocument();
    expect(within(hot).queryByText(/sshkey_test\.go/)).not.toBeInTheDocument();

    const fixtures = screen.getByTestId("leak-fixtures");
    expect(within(fixtures).getByText(/2 key-shaped strings in test files — usually fixtures/)).toBeInTheDocument();
    expect(within(fixtures).getByText("internal/broker/sshkey_test.go:35 — private-key-block")).toBeInTheDocument();
  });

  it("shows no red tier at all when every finding is a fixture", () => {
    render(
      <Harness
        profile={{
          ...profile,
          leak_findings: [{ path: "internal/api/bedrock_test.go", kind: "aws-access-key", line: 80 }],
        }}
      />,
    );
    expect(screen.queryByTestId("leak-hot")).not.toBeInTheDocument();
    expect(screen.getByTestId("leak-fixtures")).toBeInTheDocument();
  });

  it("a not-stored secret shows the pill + Add, which opens the locked AddSecretDialog for its exact name", async () => {
    render(<Harness profile={profile} />);
    await openTab("Secrets");
    const group = screen.getByTestId("group-secrets");
    expect(within(group).getByText("not stored yet")).toBeInTheDocument();

    fireEvent.click(within(group).getByRole("button", { name: "Add" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByLabelText(/name/i)).toHaveValue("DATABASE_URL");
  });

  it("a stored secret shows the stored pill instead, with no Add button", async () => {
    render(<Harness profile={profile} storedSecretNames={["DATABASE_URL"]} />);
    await openTab("Secrets");
    const group = screen.getByTestId("group-secrets");
    expect(within(group).getByText("stored")).toBeInTheDocument();
    expect(within(group).queryByRole("button", { name: "Add" })).not.toBeInTheDocument();
  });

  it("the lane toggle flips a secret between required and optional, clearing the unmet-required line once optional", async () => {
    render(<Harness profile={profile} />);
    await openTab("Secrets");
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

  it("shows the unmet-required line for a required secret that isn't stored", async () => {
    render(<Harness profile={profile} />);
    await openTab("Secrets");
    expect(screen.getByText(/1 required secret isn't stored yet\./)).toBeInTheDocument();
    expect(screen.getByText(C.UNMET_OK, { exact: false })).toBeInTheDocument();
  });

  it("reach: an auto-allowed host defaults to required, and the holding block's Approve needs the untrusted-content confirm", async () => {
    render(<Harness profile={profile} />);
    // Reach is the default tab — no click needed to see what it touches.
    const egressGroup = screen.getByTestId("group-reach");
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

  it("services render names only, with no lane control", async () => {
    render(<Harness profile={profile} />);
    await openTab("Files & services");
    const group = screen.getByTestId("group-services");
    expect(within(group).getByText("postgres:15")).toBeInTheDocument();
    expect(within(group).queryByRole("radio")).toBeNull();
    expect(within(group).getByText(C.SERVICES)).toBeInTheDocument();
  });

  it("shows the blind-spot line and a raw-profile disclosure, in the Files & services tab", async () => {
    render(<Harness profile={profile} />);
    await openTab("Files & services");
    expect(screen.getByText(C.BLIND_SPOT)).toBeInTheDocument();
    expect(screen.getByText("Raw profile")).toBeInTheDocument();
  });
});

describe("StepRequirements — host directory write rows, one per local_dir source", () => {
  const sources: SourceRow[] = [
    { ...newSourceRow("local_dir"), path: "/home/me/payments" },
    { ...newSourceRow("local_dir"), path: "/home/me/fixtures" },
  ];

  it("renders one write row per local_dir source, defaulting to optional", async () => {
    render(<Harness profile={{}} sources={sources} />);
    await openTab("Files & services");
    expect(screen.getByText("/home/me/payments")).toBeInTheDocument();
    expect(screen.getByText("/home/me/fixtures")).toBeInTheDocument();
    const optionalRadios = screen.getAllByRole("radio", { name: "Optional" });
    expect(optionalRadios.length).toBeGreaterThanOrEqual(2);
    expect(screen.getAllByText(/runs mount it read-only/)).toHaveLength(2);
  });

  it("flips the consequence line to the required warning when its lane is switched", async () => {
    render(<Harness profile={{}} sources={[sources[0]]} />);
    await openTab("Files & services");
    const laneGroup = screen.getByRole("radiogroup", { name: "write access to /home/me/payments" });
    fireEvent.click(within(laneGroup).getByRole("radio", { name: "Required" }));
    expect(screen.getByText(/every run can change these files on your machine/)).toBeInTheDocument();
  });
});

describe("StepRequirements — Record, LAST: prove & discover", () => {
  const profile: WorkspaceProfile = { egress_domains: ["registry.npmjs.org"] };

  it("is the last tab, not the first — Reach opens by default", async () => {
    render(<Harness profile={profile} />);
    const tabs = screen.getAllByRole("tab").map((t) => t.textContent);
    expect(tabs).toEqual(["Reach", "Secrets", "Files & services", "Record"]);
    expect(screen.getByRole("tab", { name: "Reach", selected: true })).toBeInTheDocument();
  });

  it("opens with what the recording will carry, derived from the contract-so-far", async () => {
    render(<Harness profile={profile} />);
    await openTab("Record");
    const carry = screen.getByTestId("record-carry");
    expect(within(carry).getByText(RD2.CARRY)).toBeInTheDocument();
    expect(within(carry).getByText(/server default/)).toBeInTheDocument();
    expect(within(carry).getByText(/1 host allowed — anything else is held at the door/)).toBeInTheDocument();
    expect(within(carry).getByText(RD2.CARRY_FROM)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Record a session" })).toBeEnabled();
  });

  // The owner's exact complaint: a live-looking button whose dependency was
  // never stated. With nothing resolving, the agent button is ABSENT — not
  // disabled — the fact says why, and terminal recording is promoted.
  it("with nothing resolving: no agent button at all, the stated fact, and a working path to Reach", async () => {
    render(<Harness profile={profile} powerSource={{ kind: "none" }} />);
    await openTab("Record");
    expect(screen.getByText(RD2.RECORD_NEEDS)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Record a session" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Record a terminal session" })).toBeEnabled();
    expect(screen.getByText(RD2.TERMINAL_ONLY)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /open the reach tab/i }));
    expect(screen.getByRole("tab", { name: "Reach", selected: true })).toBeInTheDocument();
  });

  it("names the pinned integration in the carry card", async () => {
    render(<Harness profile={profile} powerSource={{ kind: "pinned", integrationId: "i1", name: "Team API key" }} />);
    await openTab("Record");
    expect(within(screen.getByTestId("record-carry")).getByText(/Team API key — pinned to this workspace/)).toBeInTheDocument();
  });
});

describe("StepRequirements — the Reach power-source card", () => {
  const profile: WorkspaceProfile = { egress_domains: ["registry.npmjs.org"] };

  it("states the resolution first, display-only — the pin control lives on the workspace page", () => {
    render(<Harness profile={profile} />);
    const card = screen.getByTestId("power-source-card");
    expect(within(card).getByText("Agent runs here use")).toBeInTheDocument();
    expect(within(card).getByText("server default")).toBeInTheDocument();
    expect(within(card).getByText("resolves")).toBeInTheDocument();
    expect(within(card).getByText(RD2.POWER_RESOLVES_BODY)).toBeInTheDocument();
    expect(within(card).queryByRole("button")).not.toBeInTheDocument();
  });

  it("goes amber with the honest none-line when nothing resolves", () => {
    render(<Harness profile={profile} powerSource={{ kind: "none" }} />);
    const card = screen.getByTestId("power-source-card");
    expect(within(card).getByText(/nothing yet — this image's agent won't have model access/)).toBeInTheDocument();
    expect(within(card).getByText(RD2.POWER_NONE_BODY)).toBeInTheDocument();
  });

  it("offers the record-first escape hatch only when reach is empty", () => {
    // Secrets declared, nothing to reach: the step renders, and Reach's empty
    // state carries the one quiet discovery link.
    render(<Harness profile={{ required_secrets: [{ name: "DATABASE_URL", kind: "postgres" }] }} />);
    fireEvent.click(screen.getByText(RD2.ESCAPE));
    expect(screen.getByRole("tab", { name: "Record", selected: true })).toBeInTheDocument();

    // And with hosts on Reach, no escape link — it is for the lost only.
    cleanup();
    render(<Harness profile={{ egress_domains: ["registry.npmjs.org"] }} />);
    expect(screen.queryByText(RD2.ESCAPE)).not.toBeInTheDocument();
  });
});

describe("StepRequirements — Reach/Secrets rows seeded from resolved model access", () => {
  const profile: WorkspaceProfile = {
    required_secrets: [{ name: "DATABASE_URL", kind: "postgres" }],
    egress_domains: ["registry.npmjs.org"],
  };

  it("Reach: adds the 'from model access' row (tooltip RD2.EGRESS_TIP) when the server default resolves", () => {
    render(<Harness profile={profile} powerSource={{ kind: "default" }} />);
    const chip = screen.getByText("from model access");
    expect(chip).toHaveAttribute("title", RD2.EGRESS_TIP);
    expect(screen.getByText("api.anthropic.com")).toBeInTheDocument();
  });

  it("Reach: omits the model-access row when nothing resolves", () => {
    render(<Harness profile={profile} powerSource={{ kind: "none" }} />);
    expect(screen.queryByText("from model access")).not.toBeInTheDocument();
  });

  it("Secrets: adds the managed row (tooltip RD2.SECRET_TIP), named for a pinned integration", async () => {
    render(<Harness profile={profile} powerSource={{ kind: "pinned", integrationId: "i1", name: "Team API key" }} />);
    await openTab("Secrets");
    const chip = screen.getByText("integration");
    expect(chip).toHaveAttribute("title", RD2.SECRET_TIP);
    expect(screen.getByText("Team API key")).toBeInTheDocument();
  });

  it("Secrets: omits the managed row when nothing resolves", async () => {
    render(<Harness profile={profile} powerSource={{ kind: "none" }} />);
    await openTab("Secrets");
    expect(screen.queryByText("integration")).not.toBeInTheDocument();
  });
});
