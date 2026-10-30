/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Policies -> Wardyn boundary (board T2) and its dry run (board T4's report): the stored mode is what the page
// shows, Hard limits can be chosen from no_limits, every save runs the dry run before anything is
// written, a stale dry-run reply is dropped, the PUT carries the draft with the ETag it read, a refused write
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
import { BOUNDARY, BOUNDARY_DESKTOP, BOUNDARY_DRY_RUN } from "../../../lib/boundary-copy";
import { BASELINE, CHANGES, DELIVERY, RUN_LIMITS } from "../../../lib/governance-copy";
import type { GovernanceChange } from "../../../lib/types";
import type { BoundaryReport } from "../../../lib/api/boundary";
import { SECURITY_ONLY_REASON } from "../../wardyn/copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { BoundaryScreen } from "./boundary-screen";

const noLimits = { mode: "no_limits", spec: {}, limits: {}, etag: '"b1"' };
// A boundary already in hard_limits (set through the store).
const hardLimits = { mode: "hard_limits", spec: {}, limits: {}, etag: '"b1"' };
const emptyReport: BoundaryReport = { target: "boundary", deployment: [], units: [], stored_policies: [], memberships: [] };
const REFUSED = "invalid Wardyn boundary: denied_domains: not a hostname";

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

  it("with a stored no_limits boundary the Hard limits choice is enabled, and choosing it then Review shows the dry run", async () => {
    const user = userEvent.setup();
    renderScreen();
    await screen.findByText(BOUNDARY.ORG_PANEL);
    expect(hardRadio()).toBeEnabled();
    await user.click(hardRadio());
    expect(await screen.findByTestId("boundary-hard-limits")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    expect(await screen.findByTestId("boundary-dry-run")).toBeInTheDocument();
    expect(dryRunMock).toHaveBeenCalledTimes(1);
    expect(putMock).not.toHaveBeenCalled();
  });

  it("sits in the shared form column (#3058), not a page of its own width", async () => {
    renderScreen();
    const title = await screen.findByRole("heading", { level: 1, name: BOUNDARY.TITLE });
    expect(title.closest("[data-page-width]")).toHaveAttribute("data-page-width", "form");
  });

  it("with no second person required, the report says After saving and the button says Save changes; the read's flag switches both and the footer (#3389)", async () => {
    const user = userEvent.setup();
    const { unmount } = renderScreen();
    await screen.findByText(BOUNDARY.ORG_PANEL);
    await user.click(hardRadio());
    expect(screen.getByText(BOUNDARY.FOOTER)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    const report = await screen.findByTestId("boundary-dry-run");
    expect(within(report).getByRole("columnheader", { name: BOUNDARY_DRY_RUN.AFTER_SAVE })).toBeInTheDocument();
    expect(within(report).getByRole("button", { name: BOUNDARY_DRY_RUN.SUBMIT_SAVE })).toBeInTheDocument();
    expect(within(report).queryByText(BOUNDARY_DRY_RUN.SUBMIT)).toBeNull();
    unmount();

    getMock.mockResolvedValue({ ...noLimits, save_held_for_approval: true });
    renderScreen();
    await screen.findByText(BOUNDARY.ORG_PANEL);
    await user.click(hardRadio());
    expect(screen.getByText(BOUNDARY.FOOTER_SECOND_HUMAN)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    const held = await screen.findByTestId("boundary-dry-run");
    expect(within(held).getByRole("columnheader", { name: BOUNDARY_DRY_RUN.AFTER })).toBeInTheDocument();
    expect(within(held).getByRole("button", { name: BOUNDARY_DRY_RUN.SUBMIT })).toBeInTheDocument();
    expect(within(held).queryByText(BOUNDARY_DRY_RUN.SUBMIT_SAVE)).toBeNull();
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
      units: [{ id: "p1", name: "Platform team", changes: [{ knob: "denied_domains", before: null, after: ["paste.example.org"] }] }],
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

  it("the Desktop card's draft goes to the dry run and the report's change table (#2967)", async () => {
    getMock.mockResolvedValue(hardLimits);
    const user = userEvent.setup();
    renderScreen();
    const card = await screen.findByTestId("boundary-desktop");
    await user.click(within(within(card).getByRole("group", { name: "Copy out of the desktop" })).getByRole("radio", { name: BOUNDARY_DESKTOP.BLOCKED }));
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    const report = await screen.findByTestId("boundary-dry-run");
    expect(dryRunMock).toHaveBeenCalledWith({
      spec: { desktop: { paste_in: "allowed", copy_out: "blocked", upload: "allowed", download: "allowed" } },
      limits: {},
    });
    const row = within(report).getByText(BOUNDARY_DESKTOP.TITLE).closest("tr")!;
    expect(row).toHaveTextContent("Paste in allowed · Copy out allowed · Upload allowed · Download allowed");
    expect(row).toHaveTextContent("Paste in allowed · Copy out blocked · Upload allowed · Download allowed");
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
    late.resolve({ ...emptyReport, units: [{ id: "p1", name: "Platform team", changes: [] }] });
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
      units: [{ id: "p1", name: "Platform team", changes: c }],
      memberships: [
        { id: "a1", subject_type: "user", subject: "jonas@example.org", unit_id: "p1", unit_name: "Platform team", changes: c },
        { id: "a2", subject_type: "group", subject: "platform-leads", unit_id: "p1", unit_name: "Platform team", changes: c },
        { id: "a3", subject_type: "all", subject: "", unit_id: "p1", unit_name: "Platform team", changes: c },
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
    expect(within(report).getByText("IdP group platform-leads · Platform team")).toBeInTheDocument();
    expect(within(report).getByText("Everyone · Platform team")).toBeInTheDocument();
    expect(within(report).queryByText(/^ · /)).toBeNull();
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

  it("a desktop lock drafts into the dry run and the PUT, keeping the stored locks it does not touch", async () => {
    getMock.mockResolvedValue({ ...hardLimits, locked_knobs: ["allowed_domains"] });
    putMock.mockResolvedValue({ ...hardLimits, locked_knobs: ["allowed_domains", "desktop.copy_out"] });
    const user = userEvent.setup();
    renderScreen();
    const copyOut = await screen.findByRole("group", { name: "Copy out of the desktop" });
    await user.click(within(copyOut).getByRole("checkbox", { name: BOUNDARY_DESKTOP.LOCK_ROW }));
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    const report = await screen.findByTestId("boundary-dry-run");
    expect(within(report).getByRole("row", { name: new RegExp(`^${BOUNDARY_DESKTOP.LOCKS}`) })).toHaveTextContent(
      `${BOUNDARY_DESKTOP.LOCKS}${BOUNDARY_DESKTOP.NO_LOCKS}Copy out`,
    );
    await user.click(await within(report).findByRole("button", { name: BOUNDARY_DRY_RUN.SUBMIT_SAVE }));
    expect(putMock).toHaveBeenCalledWith(
      { mode: "hard_limits", spec: {}, limits: {}, locked_knobs: ["allowed_domains", "desktop.copy_out"] },
      '"b1"',
    );
  });

  it("Submit sends the draft with the ETag it read, and a refused write reads in the server's words", async () => {
    getMock.mockResolvedValue(hardLimits);
    putMock.mockRejectedValue(new HttpError(400, REFUSED, "boundary_invalid"));
    const user = userEvent.setup();
    renderScreen();
    await draftHosts("paste.example.org");
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    const report = await screen.findByTestId("boundary-dry-run");
    await user.click(await within(report).findByRole("button", { name: BOUNDARY_DRY_RUN.SUBMIT_SAVE }));
    expect(putMock).toHaveBeenCalledWith({ mode: "hard_limits", spec: { denied_domains: ["paste.example.org"] }, limits: {} }, '"b1"');
    expect(await within(report).findByText(BASELINE.REFUSED_TITLE)).toBeInTheDocument();
    expect(within(report).getByText(REFUSED)).toBeInTheDocument();
  });

  it("a held write is the submitted note and Submit goes off until the draft changes", async () => {
    getMock.mockResolvedValue({ ...hardLimits, save_held_for_approval: true });
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
    expect(within(report).getByRole("button", { name: BOUNDARY_DRY_RUN.SUBMIT_SAVE })).toBeDisabled();
    await user.click(within(report).getByRole("button", { name: BOUNDARY_DRY_RUN.RETRY }));
    await waitFor(() => expect(dryRunMock).toHaveBeenCalledTimes(2));
    expect(await within(report).findByText(BOUNDARY_DRY_RUN.NO_CHANGE)).toBeInTheDocument();
  });

  // The dry run names the person (asPerson); a session another tab switched to someone else is refused 412
  // (expected_principal_mismatch). Today that is the generic failed report: it does not show the server's
  // "reload and retry" sentence, offers no reload, and sends nothing.
  it("a 412 from the dry run after a session switch is the failed report: no Submit, no write", async () => {
    getMock.mockResolvedValue(hardLimits);
    const sentence = "This page was opened as a different person than the one now signed in — reload and retry";
    dryRunMock.mockRejectedValueOnce(new HttpError(412, sentence, "expected_principal_mismatch"));
    const user = userEvent.setup();
    renderScreen();
    await draftHosts("paste.example.org");
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    const report = await screen.findByTestId("boundary-dry-run");
    const alert = await within(report).findByRole("alert");
    expect(within(alert).getByText(BOUNDARY_DRY_RUN.FAILED_TITLE)).toBeInTheDocument();
    expect(within(alert).getByText(BOUNDARY_DRY_RUN.FAILED_BODY)).toBeInTheDocument();
    expect(within(alert).getByRole("button", { name: BOUNDARY_DRY_RUN.RETRY })).toBeInTheDocument();
    expect(within(report).queryByText(sentence)).toBeNull();
    expect(within(report).queryByRole("button", { name: /reload/i })).toBeNull();
    expect(within(report).getByRole("button", { name: BOUNDARY_DRY_RUN.SUBMIT_SAVE })).toBeDisabled();
    expect(putMock).not.toHaveBeenCalled();
  });

  it("a direct save says Saved and adopts the stored document", async () => {
    getMock.mockResolvedValue(hardLimits);
    putMock.mockImplementation(() => Promise.resolve({ mode: "no_limits", spec: { denied_domains: ["x.example"] }, limits: {}, etag: '"b2"' }));
    const user = userEvent.setup();
    renderScreen();
    await draftHosts("x.example");
    await user.click(screen.getByRole("button", { name: BOUNDARY.REVIEW }));
    await user.click(await within(await screen.findByTestId("boundary-dry-run")).findByRole("button", { name: BOUNDARY_DRY_RUN.SUBMIT_SAVE }));
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
