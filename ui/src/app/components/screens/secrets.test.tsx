/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

// MEDIUM fixes pinned here:
//  - a failed deleteSecret() must surface a toast.error (was silent).
//  - AddSecretDialog must warn before overwriting an existing secret name.

const toastError = vi.fn();
const toastSuccess = vi.fn();
vi.mock("sonner", () => ({
  toast: { error: (...a: unknown[]) => toastError(...a), success: (...a: unknown[]) => toastSuccess(...a) },
}));

const listSecretsMock = vi.fn();
const deleteSecretMock = vi.fn();
const setSecretMock = vi.fn();
vi.mock("../../lib/api/secrets", () => ({
  secrets: {
    listSecrets: () => listSecretsMock(),
    deleteSecret: (...a: unknown[]) => deleteSecretMock(...a),
    setSecret: (...a: unknown[]) => setSecretMock(...a),
  },
}));
// The screen also lists composer backends (advisory); stub it to an empty list.
vi.mock("../../lib/api/compose", () => ({
  composer: { listComposerBackends: () => Promise.resolve([]) },
}));

import { SecretsScreen, AddSecretDialog } from "./secrets";

describe("SecretsScreen — delete error handling", () => {
  beforeEach(() => {
    toastError.mockClear();
    toastSuccess.mockClear();
    listSecretsMock.mockReset();
    deleteSecretMock.mockReset();
    listSecretsMock.mockResolvedValue(["anthropic-api-key"]);
  });

  // Open the row's dropdown menu and click "Delete", then confirm in the alert
  // dialog. Radix DropdownMenu needs real pointer events, so we drive it with
  // userEvent (pointer checks disabled for jsdom).
  async function deleteFirstSecret() {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const menuBtn = await screen.findByRole("button", { name: /secret actions/i });
    await user.click(menuBtn);
    const deleteItem = await screen.findByRole("menuitem", { name: /delete/i });
    await user.click(deleteItem);
    const confirm = await screen.findByRole("button", { name: /delete secret/i });
    await user.click(confirm);
  }

  it("surfaces a toast.error when deleteSecret() rejects (no longer silent)", async () => {
    deleteSecretMock.mockRejectedValue(new Error("HTTP 403: forbidden"));
    render(<SecretsScreen />);
    await screen.findByText("anthropic-api-key");

    await deleteFirstSecret();

    await waitFor(() => expect(toastError).toHaveBeenCalledTimes(1));
    expect(toastSuccess).not.toHaveBeenCalled();
  });

  it("toasts success when deletion succeeds", async () => {
    deleteSecretMock.mockResolvedValue(undefined);
    render(<SecretsScreen />);
    await screen.findByText("anthropic-api-key");

    await deleteFirstSecret();

    await waitFor(() => expect(toastSuccess).toHaveBeenCalledTimes(1));
    expect(toastError).not.toHaveBeenCalled();
  });
});

describe("AddSecretDialog — overwrite warning", () => {
  beforeEach(() => {
    setSecretMock.mockReset();
    setSecretMock.mockResolvedValue(undefined);
  });

  it("warns and requires a second confirm when the name already exists", async () => {
    render(
      <AddSecretDialog
        open
        onOpenChange={() => {}}
        existingNames={["anthropic-api-key"]}
      />,
    );

    fireEvent.change(screen.getByLabelText(/name/i), { target: { value: "anthropic-api-key" } });
    fireEvent.change(screen.getByLabelText(/value/i), { target: { value: "sk-new" } });

    // The overwrite warning must be visible.
    expect(screen.getByText(/already exists/i)).toBeInTheDocument();

    // First click does NOT save — it asks for confirmation (button flips).
    const save = screen.getByRole("button", { name: /overwrites/i });
    fireEvent.click(save);
    expect(setSecretMock).not.toHaveBeenCalled();

    // Second click (now "Overwrite secret") commits.
    const overwrite = await screen.findByRole("button", { name: /^overwrite secret$/i });
    fireEvent.click(overwrite);
    await waitFor(() => expect(setSecretMock).toHaveBeenCalledWith("anthropic-api-key", "sk-new"));
  });

  it("saves immediately (no warning) for a new, non-colliding name", async () => {
    render(
      <AddSecretDialog
        open
        onOpenChange={() => {}}
        existingNames={["anthropic-api-key"]}
      />,
    );

    fireEvent.change(screen.getByLabelText(/name/i), { target: { value: "openai-api-key" } });
    fireEvent.change(screen.getByLabelText(/value/i), { target: { value: "sk-other" } });
    expect(screen.queryByText(/already exists/i)).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /save secret/i }));
    await waitFor(() => expect(setSecretMock).toHaveBeenCalledWith("openai-api-key", "sk-other"));
  });
});

describe("SecretsScreen — Standing chip (SCM ladder rungs 2/3)", () => {
  beforeEach(() => {
    listSecretsMock.mockReset();
  });

  it("shows the Standing chip only on ssh-key-*/git-pat-* rows", async () => {
    listSecretsMock.mockResolvedValue(["ssh-key-github-com", "other-secret"]);
    render(<SecretsScreen />);
    await screen.findByText("ssh-key-github-com");

    const standingRow = screen.getByText("ssh-key-github-com").closest("tr")!;
    expect(within(standingRow).getByText("Standing")).toBeInTheDocument();

    const plainRow = screen.getByText("other-secret").closest("tr")!;
    expect(within(plainRow).queryByText("Standing")).not.toBeInTheDocument();
  });
});

describe("AddSecretDialog — provider chips (F5)", () => {
  it("offers provider chips on a blank-name open and prefills the Name field on click", async () => {
    render(<AddSecretDialog open onOpenChange={() => {}} />);
    const user = userEvent.setup();

    const nameInput = screen.getByLabelText(/name/i) as HTMLInputElement;
    expect(nameInput.value).toBe("");

    await user.click(screen.getByRole("button", { name: "github-pat" }));
    expect(nameInput.value).toBe("github-pat");

    // Never touches the Value field — chips prefill the name only.
    expect((screen.getByLabelText(/value/i) as HTMLTextAreaElement).value).toBe("");
  });

  it("clears the Name field back to blank via the Custom… chip", async () => {
    render(<AddSecretDialog open onOpenChange={() => {}} />);
    const user = userEvent.setup();
    const nameInput = screen.getByLabelText(/name/i) as HTMLInputElement;

    await user.click(screen.getByRole("button", { name: "anthropic-api-key" }));
    expect(nameInput.value).toBe("anthropic-api-key");

    await user.click(screen.getByRole("button", { name: /custom/i }));
    expect(nameInput.value).toBe("");
  });

  it("does NOT show provider chips when opened with a prefilled name (rotate flow)", () => {
    render(
      <AddSecretDialog open onOpenChange={() => {}} initialName="anthropic-api-key" />,
    );
    expect(screen.queryByRole("button", { name: "github-pat" })).toBeNull();
  });
});
