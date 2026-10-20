/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Policies -> Wardyn boundary (board T2) and its dry run (board T4's report): the stored mode is what the page
// shows, every save runs the dry run before anything is written, the PUT carries the draft with the ETag it
// read, the gated hard_limits refusal reads in the server's words, and a held write is the submitted note.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const getMock = vi.fn();
const putMock = vi.fn();
const dryRunMock = vi.fn();
vi.mock("../../../lib/api/boundary", () => ({
  boundary: { get: () => getMock(), put: (...a: unknown[]) => putMock(...a), dryRun: (...a: unknown[]) => dryRunMock(...a) },
}));

import { setField } from "../../../../test/set-field";
import { HttpError, PendingChangeError } from "../../../lib/api/core";
import { BOUNDARY, BOUNDARY_DRY_RUN } from "../../../lib/boundary-copy";
import { BASELINE, CHANGES } from "../../../lib/governance-copy";
import type { GovernanceChange } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";
import { BoundaryScreen } from "./boundary-screen";

const stored = { mode: "no_limits", spec: {}, limits: {}, etag: '"b1"' };
const emptyReport = { deployment: [], profiles: [], stored_policies: [], assignments: [] };
const GATED = "hard_limits cannot be set yet: the Wardyn boundary does not bound every brokered credential path a run is given, so it stays in no_limits until it does";

function renderScreen(securityOperator = true) {
  return render(
    <MemoryRouter>
      <OperatorProvider operator securityOperator={securityOperator}>
        <BoundaryScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

async function draftHardLimits(user: ReturnType<typeof userEvent.setup>, hosts: string) {
  await user.click(await screen.findByRole("radio", { name: new RegExp(BOUNDARY.MODE_HARD_BODY.slice(0, 20)) }));
  setField(screen.getByLabelText(BOUNDARY.BLOCKED_LABEL), hosts);
}

beforeEach(() => {
  getMock.mockReset().mockResolvedValue(stored);
  putMock.mockReset().mockImplementation((body: unknown) => Promise.resolve({ ...(body as object), etag: '"b2"' }));
  dryRunMock.mockReset().mockResolvedValue(emptyReport);
});

describe("BoundaryScreen", () => {
  it("a stored no_limits boundary shows the off state, not the hard-limits list, and nothing to review", async () => {
    renderScreen();
    expect(await screen.findByText(BOUNDARY.ORG_PANEL)).toBeInTheDocument();
    expect(screen.queryByTestId("boundary-hard-limits")).toBeNull();
    expect(screen.getByTestId("boundary-mode-chip")).toHaveTextContent(new RegExp(`^${BOUNDARY.MODE_ORG}$`));
    expect(screen.getByRole("button", { name: BOUNDARY.REVIEW })).toBeDisabled();
    expect(screen.getByText(BOUNDARY.BINDS_EVERY_BODY)).toBeInTheDocument();
    expect(screen.getByText(BOUNDARY.BREAK_GLASS_BODY)).toBeInTheDocument();
    expect(screen.getByRole("group", { name: BOUNDARY.RINGS_LABEL })).toHaveTextContent(BOUNDARY.NOT_VISIBLE);
  });

  it("picking Hard limits shows the list editor and the floor's barrier, and Discard puts the stored mode back", async () => {
    const user = userEvent.setup();
    renderScreen();
    await draftHardLimits(user, "");
    const hardPanel = screen.getByTestId("boundary-hard-limits");
    expect(screen.getByTestId("boundary-mode-chip")).toHaveTextContent(BOUNDARY.CHIP_HARD);
    expect(within(hardPanel).getByText(BOUNDARY.HARD_HEADING)).toBeInTheDocument();
    expect(within(hardPanel).getByText(new RegExp(BOUNDARY.BLOCKED_EMPTY))).toBeInTheDocument();
    await user.selectOptions(screen.getByLabelText(BOUNDARY.FLOOR_LABEL), "CC2");
    expect(within(hardPanel).getByText(BOUNDARY.FLOOR_TAIL("Wall"))).toBeInTheDocument();
    expect(screen.queryByTestId("boundary-org")).toBeNull();
    await user.click(screen.getByRole("button", { name: BOUNDARY.DISCARD }));
    expect(screen.getByTestId("boundary-org")).toBeInTheDocument();
  });

  it("Review changes runs the dry run on the draft and writes nothing until Submit", async () => {
    dryRunMock.mockResolvedValue({
      ...emptyReport,
      profiles: [{ id: "p1", name: "Platform team", changes: [{ knob: "denied_domains", before: null, after: ["paste.example.org"] }] }],
    });
    const user = userEvent.setup();
    renderScreen();
    await draftHardLimits(user, "paste.example.org\n  10.20.0.0/16 \n");
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    const report = await screen.findByTestId("boundary-dry-run");
    expect(dryRunMock).toHaveBeenCalledWith({ spec: { denied_domains: ["paste.example.org", "10.20.0.0/16"] }, limits: {} });
    expect(putMock).not.toHaveBeenCalled();
    expect(within(report).getByText(BOUNDARY_DRY_RUN.TITLE)).toBeInTheDocument();
    expect(await within(report).findByText("Platform team")).toBeInTheDocument();
    expect(within(report).getByText(`${BOUNDARY.BLOCKED_LABEL}: ${CHANGES.DIFF_UNSET} → paste.example.org`)).toBeInTheDocument();
    expect(within(report).getByText(BOUNDARY_DRY_RUN.COMPARED)).toBeInTheDocument();
    expect(within(screen.getByTestId("boundary-dry-run-tiles")).getByText(BOUNDARY_DRY_RUN.TILE_TEAMS).previousSibling).toHaveTextContent("1");
    await user.click(within(report).getByRole("button", { name: BOUNDARY_DRY_RUN.BACK }));
    expect(screen.getByLabelText(BOUNDARY.BLOCKED_LABEL)).toHaveValue("paste.example.org\n  10.20.0.0/16 \n");
  });

  it("an empty report says No change", async () => {
    const user = userEvent.setup();
    renderScreen();
    await draftHardLimits(user, "paste.example.org");
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    expect(await within(await screen.findByTestId("boundary-dry-run")).findByText(BOUNDARY_DRY_RUN.NO_CHANGE)).toBeInTheDocument();
  });

  it("Submit sends the draft with the ETag it read, and the gated hard_limits 409 reads in the server's words", async () => {
    putMock.mockRejectedValue(new HttpError(409, GATED, "boundary_hard_limits_gated"));
    const user = userEvent.setup();
    renderScreen();
    await draftHardLimits(user, "paste.example.org");
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    const report = await screen.findByTestId("boundary-dry-run");
    await user.click(await within(report).findByRole("button", { name: BOUNDARY_DRY_RUN.SUBMIT }));
    expect(putMock).toHaveBeenCalledWith({ mode: "hard_limits", spec: { denied_domains: ["paste.example.org"] }, limits: {} }, '"b1"');
    expect(await within(report).findByText(BASELINE.REFUSED_TITLE)).toBeInTheDocument();
    expect(within(report).getByText(GATED)).toBeInTheDocument();
    expect(within(report).getByText(BOUNDARY.NEEDS_SECOND_ADMIN)).toBeInTheDocument();
  });

  it("a held write is the submitted note and Submit goes off until the draft changes", async () => {
    putMock.mockRejectedValue(new PendingChangeError({ id: "c1" } as GovernanceChange));
    const user = userEvent.setup();
    renderScreen();
    await draftHardLimits(user, "paste.example.org");
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    const report = await screen.findByTestId("boundary-dry-run");
    await user.click(await within(report).findByRole("button", { name: BOUNDARY_DRY_RUN.SUBMIT }));
    expect(await within(report).findByText(CHANGES.SUBMITTED_TITLE)).toBeInTheDocument();
    expect(screen.queryByText(BOUNDARY.SAVED)).toBeNull();
    expect(within(report).getByRole("button", { name: BOUNDARY_DRY_RUN.SUBMIT })).toBeDisabled();
  });

  it("a failed dry run can't be submitted, and Try again runs it again", async () => {
    dryRunMock.mockRejectedValueOnce(new HttpError(500, "boom"));
    const user = userEvent.setup();
    renderScreen();
    await draftHardLimits(user, "paste.example.org");
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    const report = await screen.findByTestId("boundary-dry-run");
    expect(await within(report).findByText(BOUNDARY_DRY_RUN.FAILED_TITLE)).toBeInTheDocument();
    expect(within(report).getByRole("button", { name: BOUNDARY_DRY_RUN.SUBMIT })).toBeDisabled();
    await user.click(within(report).getByRole("button", { name: BOUNDARY_DRY_RUN.RETRY }));
    await waitFor(() => expect(dryRunMock).toHaveBeenCalledTimes(2));
    expect(await within(report).findByText(BOUNDARY_DRY_RUN.NO_CHANGE)).toBeInTheDocument();
  });

  it("a direct save adopts the stored document and its ETag", async () => {
    putMock.mockImplementation(() => Promise.resolve({ mode: "no_limits", spec: { denied_domains: ["x.example"] }, limits: {}, etag: '"b2"' }));
    const user = userEvent.setup();
    renderScreen();
    await draftHardLimits(user, "x.example");
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    await user.click(await within(await screen.findByTestId("boundary-dry-run")).findByRole("button", { name: BOUNDARY_DRY_RUN.SUBMIT }));
    expect(await screen.findByText(BOUNDARY.SAVED)).toBeInTheDocument();
    expect(screen.getByTestId("boundary-org")).toBeInTheDocument();
  });

  it("a member who isn't a security admin can't change the mode", async () => {
    renderScreen(false);
    await screen.findByText(BOUNDARY.ORG_PANEL);
    for (const radio of screen.getAllByRole("radio")) expect(radio).toBeDisabled();
  });

  it("the load failure offers a retry", async () => {
    getMock.mockRejectedValueOnce(new Error("down"));
    const user = userEvent.setup();
    renderScreen();
    expect(await screen.findByText(BOUNDARY.FETCH_FAILED_TITLE)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText(BOUNDARY.ORG_PANEL)).toBeInTheDocument();
  });

  it("a pending boundary change is named Wardyn boundary on the Changes tab", () => {
    expect(CHANGES.KIND.governance_boundary).toBe(BOUNDARY.TITLE);
  });
});
