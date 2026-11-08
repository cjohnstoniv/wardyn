/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The held-save note: without a change it is the note it always was; with one it offers Add a note, and a
// save turns the action into Edit note.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));
const noteMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return { ...actual, governance: { ...actual.governance, noteChange: (...a: unknown[]) => noteMock(...a) } };
});

import { toast } from "sonner";
import { CHANGES } from "../../../lib/governance-copy";
import { aheadByHours } from "../../../lib/test-clock";
import type { GovernanceChange } from "../../../lib/types";
import { CHANGE_NOTE } from "../../wardyn/copy/change-note";
import { SubmittedNote } from "./submitted-note";

const CHANGE: GovernanceChange = {
  id: "c1",
  target_kind: "governance_unit",
  op: "update",
  target_key: "team-a",
  state: "pending",
  proposed_by: "ana@example.com",
  proposed_at: aheadByHours(-1),
  expires_at: aheadByHours(23),
  diff: { changed: [] },
};

beforeEach(() => noteMock.mockReset());

describe("SubmittedNote", () => {
  it("without a change renders as it always did, with no note action", () => {
    render(
      <MemoryRouter>
        <SubmittedNote />
      </MemoryRouter>,
    );
    expect(screen.getByText(CHANGES.SUBMITTED_TITLE)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: CHANGES.SUBMITTED_LINK })).toBeInTheDocument();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("with a change, Add a note opens the form and a save turns it into Edit note", async () => {
    const saved = { ...CHANGE, note: { reason: "Raise the cap", ticket: "SUP-1" } };
    noteMock.mockResolvedValue(saved);
    render(
      <MemoryRouter>
        <SubmittedNote change={CHANGE} />
      </MemoryRouter>,
    );
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.ADD }));
    await userEvent.type(screen.getByLabelText(CHANGE_NOTE.WHY_LABEL), "Raise the cap");
    await userEvent.type(screen.getByLabelText(CHANGE_NOTE.TICKET_LABEL), "SUP-1");
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.SAVE }));
    expect(noteMock).toHaveBeenCalledWith("c1", { reason: "Raise the cap", ticket: "SUP-1" });
    expect(toast.success).toHaveBeenCalledWith(CHANGE_NOTE.SAVED);
    const edit = await screen.findByRole("button", { name: CHANGE_NOTE.EDIT });
    expect(screen.queryByLabelText(CHANGE_NOTE.WHY_LABEL)).toBeNull();
    await userEvent.click(edit);
    expect((screen.getByLabelText(CHANGE_NOTE.WHY_LABEL) as HTMLTextAreaElement).value).toBe("Raise the cap");
    expect((screen.getByLabelText(CHANGE_NOTE.TICKET_LABEL) as HTMLInputElement).value).toBe("SUP-1");
  });

  it("Cancel closes the form and keeps Add a note", async () => {
    render(
      <MemoryRouter>
        <SubmittedNote change={CHANGE} />
      </MemoryRouter>,
    );
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.ADD }));
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.CANCEL }));
    expect(screen.getByRole("button", { name: CHANGE_NOTE.ADD })).toBeInTheDocument();
  });
});
