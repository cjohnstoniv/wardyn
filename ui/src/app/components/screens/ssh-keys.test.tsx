/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const toastError = vi.fn();
const toastSuccess = vi.fn();
vi.mock("sonner", () => ({
  toast: { error: (...a: unknown[]) => toastError(...a), success: (...a: unknown[]) => toastSuccess(...a) },
}));

const listKeysMock = vi.fn();
const addKeyMock = vi.fn();
const deleteKeyMock = vi.fn();
vi.mock("../../lib/api/ssh-keys", () => ({
  sshKeys: {
    listKeys: () => listKeysMock(),
    addKey: (...a: unknown[]) => addKeyMock(...a),
    deleteKey: (...a: unknown[]) => deleteKeyMock(...a),
  },
}));

let mockCaps: MeCapabilities = { grants: [], enforcement: {}, session_groups: [], groups_snapshot_stale: false };
vi.mock("../../lib/api/permissions", () => ({
  permissions: { getMyCapabilities: () => Promise.resolve(mockCaps) },
}));

import { SSHKeysScreen } from "./ssh-keys";
import { OperatorProvider } from "../wardyn/operator-context";
import { DENIED } from "../../lib/permissions-copy";
import type { MeCapabilities } from "../../lib/types";

function renderScreen() {
  return render(
    <MemoryRouter>
      <SSHKeysScreen />
    </MemoryRouter>,
  );
}

function renderAsUser() {
  return render(
    <MemoryRouter>
      <OperatorProvider operator={false} securityOperator={false}>
        <SSHKeysScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  toastError.mockClear();
  toastSuccess.mockClear();
  listKeysMock.mockReset();
  addKeyMock.mockReset();
  deleteKeyMock.mockReset();
});

describe("SSHKeysScreen — empty state", () => {
  it("shows the empty-state copy and an Add key affordance", async () => {
    listKeysMock.mockResolvedValue([]);
    renderScreen();
    await screen.findByText("No keys yet.");
    expect(screen.getByText("Add your public key to connect over SSH.")).toBeInTheDocument();
  });
});

describe("SSHKeysScreen — list + remove", () => {
  beforeEach(() => {
    listKeysMock.mockResolvedValue([
      { fingerprint: "SHA256:abc/def", principal: "alice@example.com", name: "laptop", public_key: "", created_at: "2026-01-01T00:00:00Z" },
    ]);
  });

  it("lists the key by name and fingerprint", async () => {
    renderScreen();
    await screen.findByText("laptop");
    expect(screen.getByText("SHA256:abc/def")).toBeInTheDocument();
  });

  it("removes a key after confirm, and toasts success", async () => {
    deleteKeyMock.mockResolvedValue(undefined);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await screen.findByText("laptop");

    // Only the row's icon trigger matches before the confirm dialog opens.
    await user.click(screen.getByRole("button", { name: /remove key/i }));
    // Once open, the dialog's own AlertDialogAction ("Remove key") is the
    // LAST match — the row trigger is still in the DOM behind the overlay.
    const buttons = await screen.findAllByRole("button", { name: /remove key/i });
    await user.click(buttons[buttons.length - 1]);

    await waitFor(() => expect(deleteKeyMock).toHaveBeenCalledWith("SHA256:abc/def"));
    await waitFor(() => expect(toastSuccess).toHaveBeenCalledTimes(1));
    // ui-shellAuth-5: curly quotes, matching the shared DeleteConfirmDialog
    // pattern used by workspaces/policies/secrets — not straight ASCII quotes.
    expect(toastSuccess).toHaveBeenCalledWith("Key “laptop” removed");
  });

  // ui-shellAuth-5: same curly-quote convention on the confirm dialog's own
  // title, which precedes the toast in the flow above.
  it("titles the confirm dialog with curly quotes around the key name", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await screen.findByText("laptop");
    await user.click(screen.getByRole("button", { name: /remove key/i }));
    expect(await screen.findByText("Remove key “laptop”?")).toBeInTheDocument();
  });

  it("surfaces a toast.error when deleteKey() rejects", async () => {
    deleteKeyMock.mockRejectedValue(new Error("HTTP 404: ssh key not found"));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await screen.findByText("laptop");

    await user.click(screen.getByRole("button", { name: /remove key/i }));
    const buttons = await screen.findAllByRole("button", { name: /remove key/i });
    await user.click(buttons[buttons.length - 1]);

    await waitFor(() => expect(toastError).toHaveBeenCalledTimes(1));
  });
});

describe("SSHKeysScreen — add key", () => {
  beforeEach(() => {
    listKeysMock.mockResolvedValue([]);
  });

  it("adds a key and reloads the list", async () => {
    addKeyMock.mockResolvedValue({
      fingerprint: "SHA256:new",
      principal: "alice@example.com",
      name: "new key",
      public_key: "ssh-ed25519 AAAA",
      created_at: "2026-01-01T00:00:00Z",
    });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await screen.findByText("No keys yet.");

    await user.click(screen.getAllByRole("button", { name: /add key/i })[0]);
    await user.type(screen.getByLabelText(/name/i), "new key");
    await user.type(screen.getByLabelText(/public key/i), "ssh-ed25519 AAAA");

    listKeysMock.mockResolvedValue([
      { fingerprint: "SHA256:new", principal: "alice@example.com", name: "new key", public_key: "", created_at: "2026-01-01T00:00:00Z" },
    ]);
    await user.click(screen.getByRole("button", { name: /^add key$/i }));

    await waitFor(() => expect(addKeyMock).toHaveBeenCalledWith("new key", "ssh-ed25519 AAAA"));
    await waitFor(() => expect(toastSuccess).toHaveBeenCalled());
  });

  it("shows the server's validation error inline (e.g. private-key paste)", async () => {
    addKeyMock.mockRejectedValue(new Error("this looks like a PRIVATE key — paste your PUBLIC key instead"));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await screen.findByText("No keys yet.");

    await user.click(screen.getAllByRole("button", { name: /add key/i })[0]);
    await user.type(screen.getByLabelText(/public key/i), "-----BEGIN OPENSSH PRIVATE KEY-----");
    await user.click(screen.getByRole("button", { name: /^add key$/i }));

    await screen.findByText(/paste your public key instead/i);
    expect(addKeyMock).toHaveBeenCalled();
  });

  it("never asks for a name in the paste field's own text — the helper line says public-key-only", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await screen.findByText("No keys yet.");
    await user.click(screen.getAllByRole("button", { name: /add key/i })[0]);
    expect(screen.getByText("Public key only — never paste a private key.")).toBeInTheDocument();
    expect(screen.getByPlaceholderText(/ssh-ed25519 AAAA/)).toBeInTheDocument();
  });

  // ui-shellAuth-3: "Public key" is the only field save() actually requires
  // (and the submit button gates on) — it must carry the same `required`
  // marker convention as every other required field in the app (step-shell's
  // Field), not read identically to the genuinely-optional "Name" field.
  it("marks the Public key field required — the app's required-field convention", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await screen.findByText("No keys yet.");
    await user.click(screen.getAllByRole("button", { name: /add key/i })[0]);
    expect(screen.getByLabelText(/public key/i)).toBeRequired();
    expect(screen.getByLabelText(/^name$/i)).not.toBeRequired();
  });
});

// migration 0043's `role`: the stamp is taken at registration and never
// re-taken, so a demoted admin's key keeps the SSH override until it is
// deleted and added again (docs/SSH.md §Bounds). The list is the only place a
// human can see which of their keys carries it.
describe("SSHKeysScreen — admin-override badge", () => {
  it("marks only the admin-stamped key", async () => {
    listKeysMock.mockResolvedValue([
      {
        fingerprint: "SHA256:aaa",
        principal: "alice@example.com",
        name: "laptop",
        public_key: "",
        role: "user",
        created_at: "2026-01-01T00:00:00Z",
      },
      {
        fingerprint: "SHA256:bbb",
        principal: "alice@example.com",
        name: "workstation",
        public_key: "",
        role: "admin",
        created_at: "2026-01-02T00:00:00Z",
      },
    ]);
    renderScreen();
    const badge = await screen.findByText("Admin override");
    expect(badge).toBeInTheDocument();
    expect(screen.getAllByText("Admin override")).toHaveLength(1);
    // …on the admin key's row, not the member key's.
    expect(badge.closest("tr")).toHaveTextContent("workstation");
  });
});

// UT-12: the `feature` kind. The server's check is the wall; the pane only
// says so before the click, with the server's own sentence.
describe("SSHKeysScreen — ssh_key feature not available", () => {
  beforeEach(() => {
    listKeysMock.mockResolvedValue([]);
    mockCaps = { grants: [], enforcement: {}, session_groups: [], groups_snapshot_stale: false };
  });

  it("disables Add key with the sentence when the feature is enforced and not granted", async () => {
    mockCaps = { ...mockCaps, enforcement: { feature: true } };
    renderAsUser();
    await screen.findByText(DENIED.SSH_KEY_FEATURE);
    for (const b of screen.getAllByRole("button", { name: /add key/i })) expect(b).toBeDisabled();
  });

  it("disables Add key when a deny row names ssh_key, even unenforced", async () => {
    mockCaps = {
      ...mockCaps,
      grants: [{ id: "g1", subject_type: "user_type", subject: "portfolio-manager", capability: "feature", value: "ssh_key", effect: "deny", created_at: "" }],
    };
    renderAsUser();
    await screen.findByText(DENIED.SSH_KEY_FEATURE);
    for (const b of screen.getAllByRole("button", { name: /add key/i })) expect(b).toBeDisabled();
  });

  it("leaves Add key alone when only api_token is denied", async () => {
    mockCaps = {
      ...mockCaps,
      grants: [{ id: "g2", subject_type: "all", subject: "", capability: "feature", value: "api_token", effect: "deny", created_at: "" }],
    };
    renderAsUser();
    await screen.findByText("No keys yet.");
    await waitFor(() => {
      for (const b of screen.getAllByRole("button", { name: /add key/i })) expect(b).toBeEnabled();
    });
    expect(screen.queryByText(DENIED.SSH_KEY_FEATURE)).not.toBeInTheDocument();
  });

  it("never fetches or disables for a super admin", async () => {
    mockCaps = { ...mockCaps, enforcement: { feature: true } };
    renderScreen();
    await screen.findByText("No keys yet.");
    for (const b of screen.getAllByRole("button", { name: /add key/i })) expect(b).toBeEnabled();
    expect(screen.queryByText(DENIED.SSH_KEY_FEATURE)).not.toBeInTheDocument();
  });
});
