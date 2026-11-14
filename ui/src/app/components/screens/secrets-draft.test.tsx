/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Add secret dialog's draft: a save that finishes after the dialog was closed and opened again leaves the new
// draft alone and still says what became of the earlier secret; nothing closes while a save is in flight; Enter in
// the single-line Name field saves while the Value textarea keeps its newlines; closing typed text asks first.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const toastError = vi.fn();
vi.mock("sonner", () => ({ toast: { error: (...a: unknown[]) => toastError(...a), success: vi.fn() } }));
const setSecretMock = vi.fn();
vi.mock("../../lib/api/secrets", () => ({ secrets: { setSecret: (...a: unknown[]) => setSecretMock(...a) } }));

import { HttpError } from "../../lib/api/core";
import { AddSecretDialog } from "./secrets";

const nameField = () => screen.getByPlaceholderText("npm-token") as HTMLInputElement;
const valueField = () => screen.getByPlaceholderText("sk-…") as HTMLTextAreaElement;
const saveButton = () => screen.getByRole("button", { name: /save secret/i });
const cancelButton = () => screen.getByRole("button", { name: "Cancel" });
const PROMPT = "Clear this draft?";

beforeEach(() => {
  setSecretMock.mockReset().mockResolvedValue(undefined);
  toastError.mockReset();
});
afterEach(cleanup);

async function typeDraft(name: string, value: string) {
  await userEvent.type(nameField(), name);
  await userEvent.type(valueField(), value);
}

describe("AddSecretDialog: a save that finishes after the dialog was reopened", () => {
  const held = () => {
    const h = { done: () => {}, fail: (_e: unknown) => {} };
    setSecretMock.mockImplementationOnce(() => new Promise<void>((res, rej) => ((h.done = () => res()), (h.fail = rej))));
    return h;
  };

  it("a late success is reported as stored, never as this dialog's save, and leaves the new draft as typed", async () => {
    const a = held();
    const saved = vi.fn();
    const storedLate = vi.fn();
    const changed = vi.fn();
    const dialog = (open: boolean) => <AddSecretDialog open={open} ownRows onOpenChange={changed} onSaved={saved} onStoredLate={storedLate} />;
    const { rerender } = render(dialog(true));
    await typeDraft("secret-a", "aaa");
    await userEvent.click(saveButton());
    expect(setSecretMock).toHaveBeenCalledWith("secret-a", "aaa", { scope: undefined, createOnly: true });
    rerender(dialog(false));
    rerender(dialog(true));
    await typeDraft("secret-b", "bbb");
    await act(async () => a.done());
    expect(storedLate).toHaveBeenCalledWith("secret-a");
    // onSaved is what callers close and apply on: a late save must not reach it.
    expect(saved).not.toHaveBeenCalled();
    expect(changed).not.toHaveBeenCalledWith(false);
    expect(nameField().value).toBe("secret-b");
    expect(valueField().value).toBe("bbb");
    expect(saveButton()).toBeEnabled();
  });

  // Only a refusal proves nothing was saved: a server error can follow the write, and no answer says nothing.
  it.each([
    ["a refusal", new HttpError(400, "secret too short"), "secret-a was not saved", "secret too short"],
    ["a name already stored", new HttpError(409, "a secret with this name already exists", "secret_exists"), "secret-a was not saved", "a secret with this name already exists"],
    ["a server error", new HttpError(500, "store secret failed"), "secret-a may not have been saved", "store secret failed"],
    ["no answer", new HttpError(0, "Could not reach the control plane."), "secret-a may not have been saved", "Could not reach the control plane."],
  ])("a late failure (%s) is said by name, not shown against the new draft", async (_kind, failure, title, description) => {
    const a = held();
    const { rerender } = render(<AddSecretDialog open ownRows onOpenChange={() => {}} />);
    await typeDraft("secret-a", "aaa");
    await userEvent.click(saveButton());
    rerender(<AddSecretDialog open={false} ownRows onOpenChange={() => {}} />);
    rerender(<AddSecretDialog open ownRows onOpenChange={() => {}} />);
    await typeDraft("secret-b", "bbb");
    await act(async () => a.fail(failure));
    expect(toastError).toHaveBeenCalledWith(title, { description });
    expect(screen.queryByRole("alert")).toBeNull();
    expect(valueField().value).toBe("bbb");
  });

  // Leaving the page unmounts the dialog mid-save: its answer is a late one, said by name or refreshed, never onSaved.
  it.each([
    ["a server error", new HttpError(500, "store secret failed"), "secret-a may not have been saved", "store secret failed"],
    ["a refusal", new HttpError(400, "secret too short"), "secret-a was not saved", "secret too short"],
  ])("a failure (%s) after the dialog unmounted is said by name", async (_kind, failure, title, description) => {
    const a = held();
    const { unmount } = render(<AddSecretDialog open ownRows onOpenChange={() => {}} />);
    await typeDraft("secret-a", "aaa");
    await userEvent.click(saveButton());
    unmount();
    await act(async () => a.fail(failure));
    expect(toastError).toHaveBeenCalledWith(title, { description });
  });

  it("a success after the dialog unmounted refreshes, never reaching onSaved", async () => {
    const a = held();
    const saved = vi.fn();
    const storedLate = vi.fn();
    const { unmount } = render(<AddSecretDialog open ownRows onOpenChange={() => {}} onSaved={saved} onStoredLate={storedLate} />);
    await typeDraft("secret-a", "aaa");
    await userEvent.click(saveButton());
    unmount();
    await act(async () => a.done());
    expect(storedLate).toHaveBeenCalledWith("secret-a");
    expect(saved).not.toHaveBeenCalled();
  });

  it("while a save is in flight, Cancel is disabled and Escape does not close", async () => {
    held();
    const changed = vi.fn();
    render(<AddSecretDialog open ownRows onOpenChange={changed} />);
    await typeDraft("secret-a", "aaa");
    await userEvent.click(saveButton());
    expect(cancelButton()).toBeDisabled();
    await userEvent.keyboard("{Escape}");
    expect(changed).not.toHaveBeenCalledWith(false);
    expect(screen.queryByText(PROMPT)).toBeNull();
  });

  it("while a save is in flight, the name and value hold what it sent, so nothing typed then is lost", async () => {
    const a = held();
    const changed = vi.fn();
    render(<AddSecretDialog open ownRows onOpenChange={changed} />);
    await typeDraft("secret-a", "aaa");
    await userEvent.click(saveButton());
    await userEvent.type(valueField(), "zzz");
    await userEvent.type(nameField(), "-b");
    await userEvent.click(screen.getByRole("button", { name: "npm-token" }));
    expect(nameField().readOnly).toBe(true);
    expect(valueField().readOnly).toBe(true);
    // readOnly stops keystrokes only: a dispatched change event still reaches a controlled field's onChange.
    fireEvent.change(valueField(), { target: { value: "dispatched" } });
    fireEvent.change(nameField(), { target: { value: "dispatched" } });
    expect(valueField().value).toBe("aaa");
    expect(nameField().value).toBe("secret-a");
    await act(async () => a.done());
    expect(changed).toHaveBeenCalledWith(false);
  });

  it("a discard prompt already open goes away when a save starts, so it cannot close the dialog mid-save", async () => {
    held();
    const changed = vi.fn();
    render(<AddSecretDialog open ownRows onOpenChange={changed} />);
    await typeDraft("secret-a", "aaa");
    await userEvent.click(cancelButton());
    expect(screen.getByText(PROMPT)).toBeInTheDocument();
    await userEvent.click(saveButton());
    expect(setSecretMock).toHaveBeenCalledTimes(1);
    expect(screen.queryByText(PROMPT)).toBeNull();
    expect(screen.queryByRole("button", { name: "Discard changes" })).toBeNull();
    expect(changed).not.toHaveBeenCalledWith(false);
  });
});

describe("AddSecretDialog: Enter, Cancel and Escape", () => {
  it("Enter in the Name field saves", async () => {
    render(<AddSecretDialog open ownRows onOpenChange={() => {}} />);
    await typeDraft("secret-a", "aaa");
    await userEvent.type(nameField(), "{Enter}");
    expect(setSecretMock).toHaveBeenCalledTimes(1);
    expect(setSecretMock).toHaveBeenCalledWith("secret-a", "aaa", { scope: undefined, createOnly: true });
  });

  it("a held Enter does not walk through the overwrite confirmation", async () => {
    render(<AddSecretDialog open ownRows existingNames={["secret-a"]} onOpenChange={() => {}} />);
    await typeDraft("secret-a", "aaa");
    nameField().focus();
    await userEvent.keyboard("{Enter>3/}");
    expect(setSecretMock).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Overwrite secret" })).toBeInTheDocument();
  });

  it("Enter in the Value field is a newline, not a save", async () => {
    render(<AddSecretDialog open ownRows onOpenChange={() => {}} />);
    await typeDraft("secret-a", "line one{Enter}line two");
    expect(valueField().value).toBe("line one\nline two");
    expect(setSecretMock).not.toHaveBeenCalled();
  });

  it("Cancel with typed text asks first: Keep editing keeps it, Discard changes closes", async () => {
    const changed = vi.fn();
    render(<AddSecretDialog open ownRows onOpenChange={changed} />);
    await userEvent.type(valueField(), "x");
    await userEvent.click(cancelButton());
    expect(screen.getByText(PROMPT)).toBeInTheDocument();
    expect(changed).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Keep editing" }));
    expect(screen.queryByText(PROMPT)).toBeNull();
    expect(valueField().value).toBe("x");
    await userEvent.click(cancelButton());
    await userEvent.click(screen.getByRole("button", { name: "Discard changes" }));
    expect(changed).toHaveBeenCalledWith(false);
  });

  it("a discard prompt goes away when the typed text is removed again, and stays away", async () => {
    render(<AddSecretDialog open ownRows onOpenChange={() => {}} />);
    await userEvent.type(valueField(), "x");
    await userEvent.keyboard("{Escape}");
    expect(screen.getByText(PROMPT)).toBeInTheDocument();
    await userEvent.clear(valueField());
    expect(screen.queryByText(PROMPT)).toBeNull();
    await userEvent.type(valueField(), "y");
    expect(screen.queryByText(PROMPT)).toBeNull();
  });

  it("Cancel on an untouched dialog closes without asking", async () => {
    const changed = vi.fn();
    render(<AddSecretDialog open ownRows onOpenChange={changed} />);
    await userEvent.click(cancelButton());
    expect(changed).toHaveBeenCalledWith(false);
    expect(screen.queryByText(PROMPT)).toBeNull();
  });

  it("Escape with typed text asks first", async () => {
    const changed = vi.fn();
    render(<AddSecretDialog open ownRows onOpenChange={changed} />);
    await userEvent.type(nameField(), "secret-a");
    await userEvent.keyboard("{Escape}");
    expect(screen.getByText(PROMPT)).toBeInTheDocument();
    expect(changed).not.toHaveBeenCalledWith(false);
  });
});
