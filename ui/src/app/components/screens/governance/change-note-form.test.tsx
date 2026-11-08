/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The note form shared by the drawer and the held-save note: its fields, its Save/Remove rule, its busy state
// and what a refusal does. Strings are read by constant from CHANGE_NOTE.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));
const noteMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return { ...actual, governance: { ...actual.governance, noteChange: (...a: unknown[]) => noteMock(...a) } };
});

import { HttpError } from "../../../lib/api/core";
import { aheadByHours } from "../../../lib/test-clock";
import type { GovernanceChange } from "../../../lib/types";
import { CHANGE_NOTE } from "../../wardyn/copy/change-note";
import { ChangeNoteForm } from "./change-note-form";

function change(over: Partial<GovernanceChange> = {}): GovernanceChange {
  return {
    id: "c1",
    target_kind: "governance_unit",
    op: "update",
    target_key: "team-a",
    state: "pending",
    proposed_by: "ana@example.com",
    proposed_at: aheadByHours(-2),
    expires_at: aheadByHours(20),
    diff: { changed: [] },
    ...over,
  };
}

function renderForm(c: GovernanceChange = change()) {
  const onSaved = vi.fn();
  const onCancel = vi.fn();
  const onGone = vi.fn();
  render(<ChangeNoteForm change={c} onSaved={onSaved} onCancel={onCancel} onGone={onGone} />);
  return { onSaved, onCancel, onGone };
}

const why = () => screen.getByLabelText(CHANGE_NOTE.WHY_LABEL) as HTMLTextAreaElement;
const ticket = () => screen.getByLabelText(CHANGE_NOTE.TICKET_LABEL) as HTMLInputElement;

beforeEach(() => noteMock.mockReset());

describe("ChangeNoteForm", () => {
  it("opens empty with Save off when the change has no note", () => {
    renderForm();
    expect(why().value).toBe("");
    expect(ticket().value).toBe("");
    expect(screen.getByRole("button", { name: CHANGE_NOTE.SAVE })).toBeDisabled();
    expect(screen.getByText(CHANGE_NOTE.WHY_HINT)).toBeInTheDocument();
    expect(screen.getByText(CHANGE_NOTE.TICKET_HINT)).toBeInTheDocument();
  });

  it("is prefilled from the stored note, with Save on", () => {
    renderForm(change({ note: { reason: "Raise the cap", ticket: "SUP-1" } }));
    expect(why().value).toBe("Raise the cap");
    expect(ticket().value).toBe("SUP-1");
    expect(screen.getByRole("button", { name: CHANGE_NOTE.SAVE })).toBeEnabled();
  });

  it("reads Remove note once a stored note is emptied, and sends two empty strings", async () => {
    const { onSaved } = renderForm(change({ note: { reason: "Raise the cap", ticket: "SUP-1" } }));
    noteMock.mockResolvedValue(change());
    await userEvent.clear(why());
    await userEvent.clear(ticket());
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.REMOVE }));
    expect(noteMock).toHaveBeenCalledWith("c1", { reason: "", ticket: "" });
    expect(onSaved).toHaveBeenCalledWith(change());
  });

  it("caps the fields at 500 and 128 characters", () => {
    renderForm();
    expect(why().maxLength).toBe(500);
    expect(ticket().maxLength).toBe(128);
  });

  it("a blank-only entry keeps Save off when there is no note", async () => {
    renderForm();
    await userEvent.type(why(), "   ");
    expect(screen.getByRole("button", { name: CHANGE_NOTE.SAVE })).toBeDisabled();
  });

  it("saves the trimmed fields and hands the returned change to onSaved", async () => {
    const saved = change({ note: { reason: "Raise the cap", ticket: "SUP-1" } });
    noteMock.mockResolvedValue(saved);
    const { onSaved } = renderForm();
    await userEvent.type(why(), "  Raise the cap ");
    await userEvent.type(ticket(), " SUP-1");
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.SAVE }));
    expect(noteMock).toHaveBeenCalledWith("c1", { reason: "Raise the cap", ticket: "SUP-1" });
    expect(onSaved).toHaveBeenCalledWith(saved);
  });

  it("is busy while the save is in flight: both buttons off, Save with a spinner", async () => {
    let finish!: (c: GovernanceChange) => void;
    noteMock.mockReturnValue(new Promise<GovernanceChange>((r) => (finish = r)));
    const { onSaved } = renderForm();
    await userEvent.type(why(), "x");
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.SAVE }));
    expect(screen.getByRole("button", { name: CHANGE_NOTE.SAVE })).toBeDisabled();
    expect(screen.getByRole("button", { name: CHANGE_NOTE.CANCEL })).toBeDisabled();
    expect(screen.getByRole("button", { name: CHANGE_NOTE.SAVE }).querySelector("svg")).not.toBeNull();
    finish(change());
    await vi.waitFor(() => expect(onSaved).toHaveBeenCalled());
  });

  it("Cancel calls onCancel", async () => {
    const { onCancel } = renderForm();
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.CANCEL }));
    expect(onCancel).toHaveBeenCalled();
  });

  it("a 400 shows the heading and the server's sentence, and keeps the form", async () => {
    noteMock.mockRejectedValue(new HttpError(400, "ticket must be one line"));
    const { onSaved, onGone } = renderForm();
    await userEvent.type(why(), "x");
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.SAVE }));
    expect(await screen.findByText(CHANGE_NOTE.REFUSED_TITLE)).toBeInTheDocument();
    expect(screen.getByText("ticket must be one line")).toBeInTheDocument();
    expect(onSaved).not.toHaveBeenCalled();
    expect(onGone).not.toHaveBeenCalled();
    expect(why().value).toBe("x");
  });

  it("a 409 shows the refusal and calls onGone(409)", async () => {
    noteMock.mockRejectedValue(new HttpError(409, "this change is no longer pending"));
    const { onGone } = renderForm();
    await userEvent.type(why(), "x");
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.SAVE }));
    expect(await screen.findByText("this change is no longer pending")).toBeInTheDocument();
    expect(onGone).toHaveBeenCalledWith(409);
  });

  it("a 404 calls onGone(404)", async () => {
    noteMock.mockRejectedValue(new HttpError(404, "not found"));
    const { onGone } = renderForm();
    await userEvent.type(why(), "x");
    await userEvent.click(screen.getByRole("button", { name: CHANGE_NOTE.SAVE }));
    expect(await screen.findByText(CHANGE_NOTE.REFUSED_TITLE)).toBeInTheDocument();
    expect(onGone).toHaveBeenCalledWith(404);
  });
});
