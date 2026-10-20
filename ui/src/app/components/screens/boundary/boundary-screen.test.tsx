/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Policies -> Wardyn boundary (board T2) and its dry run (board T4's report): the stored mode is what the page
// shows, Hard limits stays off while the server gates it, every save runs the dry run before anything is
// written, a stale dry-run reply is dropped, the PUT carries the draft with the ETag it read, the 409 fallback
// reads in the server's words, and a held write is the submitted note. The page is security-admin only.
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
import { BASELINE, CHANGES, DELIVERY, RUN_LIMITS } from "../../../lib/governance-copy";
import type { GovernanceChange } from "../../../lib/types";
import type { BoundaryReport } from "../../../lib/api/boundary";
import { SECURITY_ONLY_REASON } from "../../wardyn/copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { BoundaryScreen } from "./boundary-screen";

const noLimits = { mode: "no_limits", spec: {}, limits: {}, etag: '"b1"' };
// A boundary already in hard_limits (set through the store): the only way the editor is reachable while gated.
const hardLimits = { mode: "hard_limits", spec: {}, limits: {}, etag: '"b1"' };
const emptyReport: BoundaryReport = { deployment: [], profiles: [], stored_policies: [], assignments: [] };
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

const hardRadio = () => screen.getByRole("radio", { name: new RegExp(BOUNDARY.MODE_HARD_BODY.slice(0, 20)) });

async function draftHosts(hosts: string) {
  setField(await screen.findByLabelText(BOUNDARY.BLOCKED_LABEL), hosts);
}

function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
}

beforeEach(() => {
  getMock.mockReset().mockResolvedValue(noLimits);
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

  it("while the server gates hard limits, the Hard limits choice is off, says why, and the list can't be reached", async () => {
    const user = userEvent.setup();
    renderScreen();
    await screen.findByText(BOUNDARY.ORG_PANEL);
    expect(hardRadio()).toBeDisabled();
    expect(hardRadio()).toHaveAccessibleDescription(BOUNDARY.HARD_GATED);
    await user.click(hardRadio());
    expect(screen.queryByTestId("boundary-hard-limits")).toBeNull();
    expect(screen.queryByText(BOUNDARY.HARD_HEADING)).toBeNull();
    expect(screen.queryByText(BOUNDARY.BLOCKED_HINT)).toBeNull();
    expect(screen.getByRole("button", { name: BOUNDARY.REVIEW })).toBeDisabled();
  });

  it("no second-admin claim before a submit, and no New installs chip", async () => {
    renderScreen();
    await screen.findByText(BOUNDARY.ORG_PANEL);
    expect(screen.queryByText(/second admin|another security admin/)).toBeNull();
    expect(screen.queryByText("New installs")).toBeNull();
    expect(screen.getByText(BOUNDARY.FOOTER)).toBeInTheDocument();
  });

  it("a stored hard-limits boundary shows the list editor and the floor's barrier, and Discard puts the stored draft back", async () => {
    getMock.mockResolvedValue(hardLimits);
    const user = userEvent.setup();
    renderScreen();
    const hardPanel = await screen.findByTestId("boundary-hard-limits");
    expect(screen.getByTestId("boundary-mode-chip")).toHaveTextContent(BOUNDARY.CHIP_HARD);
    expect(hardRadio()).toBeEnabled();
    expect(within(hardPanel).getByText(new RegExp(BOUNDARY.BLOCKED_EMPTY))).toBeInTheDocument();
    await user.selectOptions(screen.getByLabelText(BOUNDARY.FLOOR_LABEL), "CC2");
    expect(within(hardPanel).getByText(BOUNDARY.FLOOR_TAIL("Wall"))).toBeInTheDocument();
    expect(within(hardPanel).queryByText(new RegExp(BOUNDARY.BLOCKED_EMPTY))).toBeNull();
    await user.click(screen.getByRole("radio", { name: new RegExp(BOUNDARY.MODE_ORG_BODY.slice(0, 20)) }));
    expect(screen.getByTestId("boundary-org")).toBeInTheDocument();
    expect(screen.getByTestId("boundary-mode-chip")).toHaveTextContent(BOUNDARY.CHIP_ORG_DRAFT);
    await user.click(screen.getByRole("button", { name: BOUNDARY.DISCARD }));
    expect(screen.getByTestId("boundary-hard-limits")).toBeInTheDocument();
    expect(screen.getByLabelText(BOUNDARY.FLOOR_LABEL)).toHaveValue("CC1");
  });

  it("Review changes runs the dry run on the draft and writes nothing until Submit", async () => {
    getMock.mockResolvedValue(hardLimits);
    dryRunMock.mockResolvedValue({
      ...emptyReport,
      profiles: [{ id: "p1", name: "Platform team", changes: [{ knob: "denied_domains", before: null, after: ["paste.example.org"] }] }],
    });
    const user = userEvent.setup();
    renderScreen();
    await draftHosts("paste.example.org\n  10.20.0.0/16 \n");
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    const report = await screen.findByTestId("boundary-dry-run");
    expect(dryRunMock).toHaveBeenCalledWith({ spec: { denied_domains: ["paste.example.org", "10.20.0.0/16"] }, limits: {} });
    expect(putMock).not.toHaveBeenCalled();
    expect(within(report).getByText(BOUNDARY_DRY_RUN.TITLE)).toBeInTheDocument();
    expect(await within(report).findByText("Platform team")).toBeInTheDocument();
    expect(within(report).getByText(`${BOUNDARY.BLOCKED_LABEL}: ${CHANGES.DIFF_UNSET} → paste.example.org`)).toBeInTheDocument();
    expect(within(report).getByText(BOUNDARY_DRY_RUN.COMPARED)).toBeInTheDocument();
    await user.click(within(report).getByRole("button", { name: BOUNDARY_DRY_RUN.BACK }));
    expect(screen.getByLabelText(BOUNDARY.BLOCKED_LABEL)).toHaveValue("paste.example.org\n  10.20.0.0/16 \n");
  });

  it("a dry-run reply that lands after Back and an edit doesn't bring the report back", async () => {
    getMock.mockResolvedValue(hardLimits);
    const late = deferred<BoundaryReport>();
    dryRunMock.mockReturnValueOnce(late.promise);
    const user = userEvent.setup();
    renderScreen();
    await draftHosts("paste.example.org");
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    const report = await screen.findByTestId("boundary-dry-run");
    await user.click(within(report).getByRole("button", { name: BOUNDARY_DRY_RUN.BACK }));
    await draftHosts("paste.example.org\nother.example.org");
    late.resolve({ ...emptyReport, profiles: [{ id: "p1", name: "Platform team", changes: [] }] });
    await late.promise;
    await new Promise((r) => setTimeout(r, 0));
    expect(screen.queryByTestId("boundary-dry-run")).toBeNull();
    expect(screen.getByLabelText(BOUNDARY.BLOCKED_LABEL)).toHaveValue("paste.example.org\nother.example.org");
  });

  it("the tiles count people only for user assignments, with T4's singular, and other kinds get their own rows", async () => {
    getMock.mockResolvedValue(hardLimits);
    const c = [{ knob: "limits.default_end_sec", before: 3600, after: 1800 }];
    dryRunMock.mockResolvedValue({
      ...emptyReport,
      profiles: [{ id: "p1", name: "Platform team", changes: c }],
      assignments: [
        { id: "a1", subject_type: "user", subject: "jonas@example.org", profile_id: "p1", profile_name: "Platform team", changes: c },
        { id: "a2", subject_type: "group", subject: "platform-leads", profile_id: "p1", profile_name: "Platform team", changes: c },
      ],
    });
    const user = userEvent.setup();
    renderScreen();
    await draftHosts("paste.example.org");
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    const report = await screen.findByTestId("boundary-dry-run");
    const tiles = await within(report).findByTestId("boundary-dry-run-tiles");
    expect(within(tiles).getByText("team below narrows").previousSibling).toHaveTextContent("1");
    expect(within(tiles).getByText("person narrows").previousSibling).toHaveTextContent("1");
    expect(within(tiles).getByText("stored policies narrow").previousSibling).toHaveTextContent("0");
    expect(within(report).getByText(BOUNDARY_DRY_RUN.OTHER_ASSIGNMENTS)).toBeInTheDocument();
    expect(within(report).getByText("platform-leads · Platform team")).toBeInTheDocument();
    expect(within(report).getAllByText(new RegExp(`^${RUN_LIMITS.DEFAULT_END_LABEL}: `)).length).toBeGreaterThan(0);
    expect(BOUNDARY_DRY_RUN.TILE_TEAMS(2)).toBe("teams below narrow");
    expect(BOUNDARY_DRY_RUN.TILE_PEOPLE(2)).toBe("people narrow");
  });

  it("an empty report says No change", async () => {
    getMock.mockResolvedValue(hardLimits);
    const user = userEvent.setup();
    renderScreen();
    await draftHosts("paste.example.org");
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    expect(await within(await screen.findByTestId("boundary-dry-run")).findByText(BOUNDARY_DRY_RUN.NO_CHANGE)).toBeInTheDocument();
  });

  it("Submit sends the draft with the ETag it read, and the gated hard_limits 409 reads in the server's words", async () => {
    getMock.mockResolvedValue(hardLimits);
    putMock.mockRejectedValue(new HttpError(409, GATED, "boundary_hard_limits_gated"));
    const user = userEvent.setup();
    renderScreen();
    await draftHosts("paste.example.org");
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    const report = await screen.findByTestId("boundary-dry-run");
    await user.click(await within(report).findByRole("button", { name: BOUNDARY_DRY_RUN.SUBMIT }));
    expect(putMock).toHaveBeenCalledWith({ mode: "hard_limits", spec: { denied_domains: ["paste.example.org"] }, limits: {} }, '"b1"');
    expect(await within(report).findByText(BASELINE.REFUSED_TITLE)).toBeInTheDocument();
    expect(within(report).getByText(GATED)).toBeInTheDocument();
  });

  it("a held write is the submitted note and Submit goes off until the draft changes", async () => {
    getMock.mockResolvedValue(hardLimits);
    putMock.mockRejectedValue(new PendingChangeError({ id: "c1" } as GovernanceChange));
    const user = userEvent.setup();
    renderScreen();
    await draftHosts("paste.example.org");
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    const report = await screen.findByTestId("boundary-dry-run");
    await user.click(await within(report).findByRole("button", { name: BOUNDARY_DRY_RUN.SUBMIT }));
    expect(await within(report).findByText(CHANGES.SUBMITTED_TITLE)).toBeInTheDocument();
    expect(screen.queryByText(DELIVERY.SAVED)).toBeNull();
    expect(within(report).getByRole("button", { name: BOUNDARY_DRY_RUN.SUBMIT })).toBeDisabled();
  });

  it("a failed dry run can't be submitted, and Try again runs it again", async () => {
    getMock.mockResolvedValue(hardLimits);
    dryRunMock.mockRejectedValueOnce(new HttpError(500, "boom"));
    const user = userEvent.setup();
    renderScreen();
    await draftHosts("paste.example.org");
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    const report = await screen.findByTestId("boundary-dry-run");
    expect(await within(report).findByText(BOUNDARY_DRY_RUN.FAILED_TITLE)).toBeInTheDocument();
    expect(within(report).getByRole("button", { name: BOUNDARY_DRY_RUN.SUBMIT })).toBeDisabled();
    await user.click(within(report).getByRole("button", { name: BOUNDARY_DRY_RUN.RETRY }));
    await waitFor(() => expect(dryRunMock).toHaveBeenCalledTimes(2));
    expect(await within(report).findByText(BOUNDARY_DRY_RUN.NO_CHANGE)).toBeInTheDocument();
  });

  it("a direct save says Saved and adopts the stored document", async () => {
    getMock.mockResolvedValue(hardLimits);
    putMock.mockImplementation(() => Promise.resolve({ mode: "no_limits", spec: { denied_domains: ["x.example"] }, limits: {}, etag: '"b2"' }));
    const user = userEvent.setup();
    renderScreen();
    await draftHosts("x.example");
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    await user.click(await within(await screen.findByTestId("boundary-dry-run")).findByRole("button", { name: BOUNDARY_DRY_RUN.SUBMIT }));
    expect(await screen.findByText(DELIVERY.SAVED)).toBeInTheDocument();
    expect(screen.getByTestId("boundary-org")).toBeInTheDocument();
  });

  it("an admin who isn't a security admin gets the security-only line, no fetch and no Retry", async () => {
    renderScreen(false);
    expect(await screen.findByText(SECURITY_ONLY_REASON)).toBeInTheDocument();
    expect(getMock).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });

  it("a 403 from the server is the security-only line, not a retry", async () => {
    getMock.mockRejectedValueOnce(new HttpError(403, "forbidden"));
    renderScreen();
    expect(await screen.findByText(SECURITY_ONLY_REASON)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
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
