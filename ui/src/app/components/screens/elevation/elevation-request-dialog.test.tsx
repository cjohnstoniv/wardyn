/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The elevation request dialog. Expected strings are the approved copy's,
// spelled out rather than read back from the copy module.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ElevationOffer, GovernanceRequest } from "../../../lib/types/governance-requests";

const listMock = vi.fn();
const createMock = vi.fn();
vi.mock("../../../lib/api/governance-requests", () => ({
  requests: { list: (...a: unknown[]) => listMock(...a), create: (...a: unknown[]) => createMock(...a) },
}));

import { HttpError } from "../../../lib/api/core";
import { absoluteTime } from "../../../lib/format";
import { aheadByHours } from "../../../lib/test-clock";
import { ELEV } from "../../wardyn/copy/elevation";
import { ElevationRequestDialog } from "./elevation-request-dialog";

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

const payments: ElevationOffer = { unit_id: "u1", unit_name: "Payments", max_elevation: "4h" };
const billing: ElevationOffer = { unit_id: "u2", unit_name: "Billing", max_elevation: "8h" };

function sentRequest(over: Partial<GovernanceRequest> = {}): GovernanceRequest {
  return {
    id: "c1",
    state: "pending",
    kind: "elevation",
    unit_id: "u1",
    unit_name: "Payments",
    duration: "4h",
    mine: true,
    decidable: false,
    deciders: [{ kind: "unit_admin", unit_name: "Payments" }, { kind: "security" }],
    expires_at: aheadByHours(24),
    ...over,
  } as GovernanceRequest;
}

type Props = Partial<React.ComponentProps<typeof ElevationRequestDialog>>;

function setup(props: Props = {}) {
  const onOpenChange = vi.fn();
  const onSent = vi.fn();
  const utils = render(<ElevationRequestDialog open onOpenChange={onOpenChange} onSent={onSent} offers={[payments]} {...props} />);
  return { onOpenChange, onSent, ...utils };
}

const send = () => screen.getByRole("button", { name: "Request elevation" });
const reason = () => screen.getByLabelText("Why do you need it?");
const ticket = () => screen.getByLabelText(/^Ticket/);

async function fill(text = "Production incident INC-2210") {
  await userEvent.type(reason(), text);
}

beforeEach(() => {
  listMock.mockReset();
  createMock.mockReset();
});

describe("ElevationRequestDialog: the form", () => {
  it("opens with the approved wording", () => {
    setup();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(screen.getByText("New elevation request")).toBeInTheDocument();
    expect(
      screen.getByText(
        "Ask for time-limited access to a profile. The people who decide for it see your request, and the time starts when one of them approves.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByText("Only units that offer elevation are listed. A profile is a governance unit.")).toBeInTheDocument();
    expect(screen.getByText("Shown to the people who decide, and kept in the audit trail.")).toBeInTheDocument();
    expect(screen.getByText("A reference the deciders can look up.")).toBeInTheDocument();
    expect(reason()).toHaveAttribute("placeholder", "Say what you need to do and why. The people who decide read this.");
    expect(reason()).toHaveAttribute("maxlength", "500");
    expect(ticket()).toHaveAttribute("placeholder", "For example SUP-1234");
    expect(ticket()).toHaveAttribute("maxlength", "128");
    expect(screen.getByLabelText("Ticket (optional)")).toBe(ticket());
    expect(screen.getByRole("combobox", { name: "Unit" })).toHaveTextContent("Payments");
    expect(screen.getByText("0 of 500")).toBeInTheDocument();
  });

  it("disables presets over the maximum with the reason, and starts on the longest that fits", () => {
    setup();
    expect(screen.getByRole("radio", { name: "8 h" })).toBeDisabled();
    expect(screen.getByRole("radio", { name: "8 h" })).toHaveAccessibleDescription("Payments allows at most 4 hours.");
    expect(screen.getByRole("radio", { name: "8 h" }).closest("label")).toHaveAttribute("title", "Payments allows at most 4 hours.");
    expect(screen.getByRole("radio", { name: "1 h" })).toBeEnabled();
    expect(screen.getByRole("radio", { name: "4 h" })).toBeChecked();
    expect(screen.getByText("Up to 4 hours in Payments. The time starts when someone approves.")).toBeInTheDocument();
  });

  it("starts on Custom when no preset fits the maximum", () => {
    setup({ offers: [{ ...payments, max_elevation: "45m" }] });
    expect(screen.getByRole("radio", { name: "Custom" })).toBeChecked();
    expect(screen.getByRole("radio", { name: "1 h" })).toBeDisabled();
    expect(screen.getByRole("spinbutton", { name: "Length" })).toBeInTheDocument();
    expect(screen.getByText("Up to 45 minutes in Payments. The time starts when someone approves.")).toBeInTheDocument();
  });

  it("keeps Request elevation off until there is a reason", async () => {
    setup();
    expect(send()).toBeDisabled();
    await userEvent.type(reason(), "   ");
    expect(send()).toBeDisabled();
    await fill();
    expect(send()).toBeEnabled();
  });

  it("counts the reason against 500", async () => {
    setup();
    await userEvent.type(reason(), "twelve chars");
    expect(screen.getByText("12 of 500")).toBeInTheDocument();
  });

  it("makes the ticket required when the unit asks for one", async () => {
    setup({ offers: [{ ...payments, ticket_required: true }] });
    expect(screen.queryByText("Ticket (optional)")).toBeNull();
    expect(ticket()).toBeRequired();
    expect(screen.getByLabelText("Ticket")).toBe(ticket());
    await fill();
    expect(send()).toBeDisabled();
    await userEvent.type(ticket(), "INC-2210");
    expect(send()).toBeEnabled();
  });

  it("preselects the unit it is given, and sends that one", async () => {
    const { onSent } = setup({ offers: [payments, billing], unitId: "u2" });
    expect(screen.getByRole("combobox", { name: "Unit" })).toHaveTextContent("Billing");
    expect(screen.getByRole("radio", { name: "8 h" })).toBeChecked();
    createMock.mockResolvedValueOnce(sentRequest({ unit_id: "u2", unit_name: "Billing" }));
    await fill();
    await userEvent.click(send());
    expect(createMock).toHaveBeenCalledWith({ kind: "elevation", unit_id: "u2", duration: "8h", reason: "Production incident INC-2210" });
    expect(onSent).toHaveBeenCalledTimes(1);
  });

  it("picks another unit and moves the maximum with it", async () => {
    setup({ offers: [payments, billing] });
    await userEvent.click(screen.getByRole("combobox", { name: "Unit" }));
    await userEvent.click(await screen.findByRole("option", { name: "Billing" }));
    expect(screen.getByRole("radio", { name: "8 h" })).toBeEnabled();
    expect(screen.getByText("Up to 8 hours in Billing. The time starts when someone approves.")).toBeInTheDocument();
  });

  it("sends a custom length as a canonical duration", async () => {
    setup();
    await userEvent.click(screen.getByRole("radio", { name: "Custom" }));
    await userEvent.type(screen.getByRole("spinbutton", { name: "Length" }), "90");
    createMock.mockResolvedValueOnce(sentRequest());
    await fill();
    await userEvent.type(ticket(), " INC-2210 ");
    await userEvent.click(send());
    expect(createMock).toHaveBeenCalledWith({
      kind: "elevation",
      unit_id: "u1",
      duration: "1h30m",
      reason: "Production incident INC-2210",
      ticket: "INC-2210",
    });
  });

  it("sends hours when the unit is hours", async () => {
    setup();
    await userEvent.click(screen.getByRole("radio", { name: "Custom" }));
    await userEvent.type(screen.getByRole("spinbutton", { name: "Length" }), "2");
    await userEvent.click(screen.getByRole("combobox", { name: "Length unit" }));
    await userEvent.click(await screen.findByRole("option", { name: "hours" }));
    createMock.mockResolvedValueOnce(sentRequest());
    await fill();
    await userEvent.click(send());
    expect(createMock).toHaveBeenCalledWith(expect.objectContaining({ duration: "2h" }));
  });
});

describe("ElevationRequestDialog: sending", () => {
  it("turns both buttons off and reads Sending… while the request is out", async () => {
    setup();
    const d = deferred<GovernanceRequest>();
    createMock.mockReturnValueOnce(d.promise);
    await fill();
    await userEvent.click(send());
    expect(screen.getByRole("button", { name: "Sending…" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeDisabled();
    await act(async () => d.resolve(sentRequest()));
    expect(screen.queryByRole("button", { name: "Sending…" })).toBeNull();
  });

  it("shows who decides and when it expires, calls onSent once, and Done closes", async () => {
    const { onSent, onOpenChange } = setup();
    const r = sentRequest();
    createMock.mockResolvedValueOnce(r);
    await fill();
    await userEvent.click(send());
    expect(await screen.findByText("Request sent")).toBeInTheDocument();
    expect(
      screen.getByText(
        `It is waiting for admins of Payments or a security admin. You'll see the answer under Approvals. If nobody decides it by ${absoluteTime(r.expires_at)}, it expires.`,
      ),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText("Why do you need it?")).toBeNull();
    expect(screen.queryByRole("button", { name: "Request elevation" })).toBeNull();
    expect(onSent).toHaveBeenCalledTimes(1);
    expect(onSent).toHaveBeenCalledWith(r);
    expect(onOpenChange).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Done" }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(onSent).toHaveBeenCalledTimes(1);
  });
});

describe("ElevationRequestDialog: refusals", () => {
  async function refuse(err: unknown) {
    const { onOpenChange, onSent } = setup();
    createMock.mockRejectedValueOnce(err);
    await fill();
    await userEvent.type(ticket(), "INC-2210");
    await userEvent.click(send());
    return { onOpenChange, onSent };
  }

  function expectFieldsKept() {
    expect(screen.getByText("New elevation request")).toBeInTheDocument();
    expect(reason()).toHaveValue("Production incident INC-2210");
    expect(ticket()).toHaveValue("INC-2210");
    expect(screen.getByRole("radio", { name: "4 h" })).toBeChecked();
    expect(send()).toBeEnabled();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeEnabled();
  }

  it.each(["elevation_too_long", "elevation_duration_invalid"])("%s shows the server's sentence under How long", async (reasonCode) => {
    const sentence = reasonCode === "elevation_too_long" ? "Payments allows at most 4h." : "duration must be at least 15m, for example 4h";
    const { onSent } = await refuse(new HttpError(400, sentence, reasonCode));
    expect(await screen.findByText(sentence)).toHaveClass("text-danger");
    expect(screen.queryByRole("alert")).toBeNull();
    expectFieldsKept();
    expect(onSent).not.toHaveBeenCalled();
  });

  it("marks the custom field invalid on a length refusal", async () => {
    setup();
    await userEvent.click(screen.getByRole("radio", { name: "Custom" }));
    await userEvent.type(screen.getByRole("spinbutton", { name: "Length" }), "9");
    createMock.mockRejectedValueOnce(new HttpError(400, "Payments allows at most 4h.", "elevation_too_long"));
    await fill();
    await userEvent.click(send());
    await screen.findByText("Payments allows at most 4h.");
    expect(screen.getByRole("spinbutton", { name: "Length" })).toBeInvalid();
    expect(screen.getByRole("spinbutton", { name: "Length" })).toHaveValue(9);
  });

  it("governance_change_pending: the already-waiting note, not the server's sentence", async () => {
    await refuse(new HttpError(409, "a change is already waiting: c-77", "governance_change_pending"));
    const note = await screen.findByRole("alert");
    expect(within(note).getByText("Couldn't send the request")).toBeInTheDocument();
    expect(within(note).getByText("You already have a request waiting for Payments. Withdraw it first, or wait for an answer.")).toBeInTheDocument();
    expect(screen.queryByText(/c-77/)).toBeNull();
    expectFieldsKept();
  });

  it("elevation_not_offered: the unit does not offer elevation", async () => {
    await refuse(new HttpError(403, "not offered", "elevation_not_offered"));
    const note = await screen.findByRole("alert");
    expect(within(note).getByText("Couldn't send the request")).toBeInTheDocument();
    expect(within(note).getByText("Payments doesn't offer elevation.")).toBeInTheDocument();
    expectFieldsKept();
  });

  it.each(["elevation_cooldown", "elevation_already_member", "request_reason_too_long", "deciders_unreadable"])(
    "%s: the server's sentence in mono under the red heading",
    async (reasonCode) => {
      await refuse(new HttpError(409, `the sentence for ${reasonCode}`, reasonCode));
      const note = await screen.findByRole("alert");
      expect(within(note).getByText("Couldn't send the request")).toBeInTheDocument();
      expect(within(note).getByText(`the sentence for ${reasonCode}`)).toHaveClass("font-mono");
      expectFieldsKept();
    },
  );

  it("a failure that is not an HttpError shows its message", async () => {
    await refuse(new Error("network down"));
    const note = await screen.findByRole("alert");
    expect(within(note).getByText("Couldn't send the request")).toBeInTheDocument();
    expect(within(note).getByText("network down")).toBeInTheDocument();
    expectFieldsKept();
  });

  it("clears the refusal when the person sends again", async () => {
    await refuse(new HttpError(409, "cooldown sentence", "elevation_cooldown"));
    await screen.findByRole("alert");
    const d = deferred<GovernanceRequest>();
    createMock.mockReturnValueOnce(d.promise);
    await userEvent.click(send());
    expect(screen.queryByRole("alert")).toBeNull();
    await act(async () => d.resolve(sentRequest()));
    expect(await screen.findByText("Request sent")).toBeInTheDocument();
  });
});

describe("ElevationRequestDialog: loading its own units", () => {
  it("shows Loading units… with the request off, then the form", async () => {
    const d = deferred<{ requests: never[]; offered: ElevationOffer[]; image_offered: boolean } | null>();
    listMock.mockReturnValueOnce(d.promise);
    setup({ offers: undefined });
    expect(listMock).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("combobox", { name: "Unit" })).toHaveTextContent("Loading units…");
    await userEvent.type(reason(), "needs it");
    expect(send()).toBeDisabled();
    await act(async () => d.resolve({ requests: [], offered: [payments], image_offered: false }));
    expect(screen.getByRole("combobox", { name: "Unit" })).toHaveTextContent("Payments");
    expect(send()).toBeEnabled();
  });

  it("preselects unitId among the loaded offers", async () => {
    listMock.mockResolvedValueOnce({ requests: [], offered: [payments, billing], image_offered: false });
    setup({ offers: undefined, unitId: "u2" });
    expect(await screen.findByText("Billing")).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Unit" })).toHaveTextContent("Billing");
  });

  it.each([
    ["a timeout", new HttpError(0, "Could not reach the control plane."), "Wardyn isn't answering. Try again."],
    ["a 503 with no sentence", Object.assign(new HttpError(503, "Service Unavailable"), { said: "" }), "Wardyn couldn't load your units. Try again."],
    ["a 403 with no sentence", Object.assign(new HttpError(403, "Forbidden"), { said: "" }), "Wardyn couldn't load your units."],
  ])("a units read that fails with %s says so", async (_name, error, text) => {
    listMock.mockRejectedValueOnce(error);
    setup({ offers: undefined });
    const note = await screen.findByRole("alert");
    expect(within(note).getByText(text)).toBeInTheDocument();
  });

  it("shows the Retry note on failure, and Retry asks again", async () => {
    listMock.mockRejectedValueOnce(new Error("down"));
    setup({ offers: undefined });
    const note = await screen.findByRole("alert");
    expect(within(note).getByText("Couldn't load your units")).toBeInTheDocument();
    expect(within(note).getByText("Wardyn isn't answering. Try again.")).toBeInTheDocument();
    expect(send()).toBeDisabled();
    const d = deferred<{ requests: never[]; offered: ElevationOffer[]; image_offered: boolean }>();
    listMock.mockReturnValueOnce(d.promise);
    await userEvent.click(within(note).getByRole("button", { name: "Retry" }));
    expect(listMock).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("alert")).toBeNull();
    await act(async () => d.resolve({ requests: [], offered: [payments], image_offered: false }));
    expect(screen.getByRole("combobox", { name: "Unit" })).toHaveTextContent("Payments");
  });

  it.each([
    ["an older server", null],
    ["no offered units", { requests: [], offered: [], image_offered: false }],
  ])("says nothing is offered for %s", async (_name, list) => {
    listMock.mockResolvedValueOnce(list);
    setup({ offers: undefined });
    const note = await screen.findByRole("alert");
    expect(within(note).getByText("Couldn't load your units")).toBeInTheDocument();
    expect(within(note).getByText("No unit offers you elevation.")).toBeInTheDocument();
    expect(send()).toBeDisabled();
  });

  it("does not ask the server when it is given offers", () => {
    setup();
    expect(listMock).not.toHaveBeenCalled();
  });

  it("does not list while closed", () => {
    setup({ open: false, offers: undefined });
    expect(listMock).not.toHaveBeenCalled();
  });
});

describe("ElevationRequestDialog: closing", () => {
  it("Cancel asks the parent to close", async () => {
    const { onOpenChange } = setup();
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("returns to its first state when closed and opened again", async () => {
    const { rerender, onOpenChange, onSent } = setup();
    await userEvent.click(screen.getByRole("radio", { name: "1 h" }));
    await fill();
    await userEvent.type(ticket(), "INC-2210");
    const again = (open: boolean) => rerender(<ElevationRequestDialog open={open} onOpenChange={onOpenChange} onSent={onSent} offers={[payments]} />);
    again(false);
    expect(screen.queryByRole("dialog")).toBeNull();
    again(true);
    expect(reason()).toHaveValue("");
    expect(ticket()).toHaveValue("");
    expect(screen.getByRole("radio", { name: "4 h" })).toBeChecked();
    expect(send()).toBeDisabled();
  });

  it("opens fresh after a sent request too", async () => {
    const { rerender, onOpenChange, onSent } = setup();
    createMock.mockResolvedValueOnce(sentRequest());
    await fill();
    await userEvent.click(send());
    await screen.findByText("Request sent");
    const again = (open: boolean) => rerender(<ElevationRequestDialog open={open} onOpenChange={onOpenChange} onSent={onSent} offers={[payments]} />);
    again(false);
    again(true);
    expect(screen.queryByText("Request sent")).toBeNull();
    expect(reason()).toHaveValue("");
  });
});

describe("ELEV copy", () => {
  it("holds the approved list strings, with the card's new name", () => {
    expect(ELEV.LIST_TITLE).toBe("Your requests");
    expect(ELEV.LIST_LEAD).toBe("Time-limited access to a profile. A profile is a governance unit.");
    expect(ELEV.LIST_EMPTY).toBe("You have no requests.");
    expect([ELEV.STATE_WAITING, ELEV.STATE_APPROVED, ELEV.STATE_DENIED, ELEV.STATE_EXPIRED, ELEV.STATE_ENDED]).toEqual([
      "Waiting for approval",
      "Approved",
      "Denied",
      "Expired",
      "Ended",
    ]);
  });

  it("builds the list lines", () => {
    expect(ELEV.LINE_WAITING("3m ago", "admins of Payments or a security admin")).toBe("Requested 3m ago. Admins of Payments or a security admin decide it.");
    expect(ELEV.LINE_WAITING("3m ago", "a security admin")).toBe("Requested 3m ago. A security admin decides it.");
    expect(ELEV.LINE_WAITING("3m ago", "the support-leads group")).toBe("Requested 3m ago. The support-leads group decides it.");
    expect(ELEV.LINE_WAITING("3m ago", "admins of Payments")).toBe("Requested 3m ago. Admins of Payments decide it.");
    expect(ELEV.LINE_APPROVED("Mon 14:20", "3 h", "Payments")).toBe("Ends Mon 14:20 (in 3 h). Pick Payments when you start a run.");
    expect(ELEV.LINE_DENIED("Dana", "not now", "1h ago")).toBe("Dana: “not now” · 1h ago");
    expect(ELEV.LINE_EXPIRED("1h ago")).toBe("Nobody decided it in time · 1h ago");
    expect(ELEV.LINE_ENDED("1h ago")).toBe("Ended 1h ago");
  });

  it("builds the decider card lines", () => {
    expect(ELEV.F_TITLE("Ana", "Payments", "4 hours")).toBe("Elevate Ana to Payments for 4 hours");
    expect(ELEV.F_WHAT("Ana", "Payments", "4 hours")).toBe("Ana becomes a member of Payments for 4 hours, starting when you approve.");
    expect(ELEV.F_BLAST("Payments")).toBe(
      "They can start runs under Payments' limits until then, and each run they start ends when the elevation ends. Ending the elevation early doesn't stop a run that has already started.",
    );
    expect(ELEV.F_BLAST("Platform")).toContain("under Platform's limits until then");
    expect(ELEV.F_BLAST("NEWS")).toContain("under NEWS' limits until then");
  });
});
