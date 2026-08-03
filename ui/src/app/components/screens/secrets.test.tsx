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
import { LANE_META } from "../../lib/scm-provider";
import { OperatorProvider } from "../wardyn/operator-context";

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

    await user.click(screen.getByRole("button", { name: "npm-token" }));
    expect(nameInput.value).toBe("npm-token");

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
    expect(screen.queryByRole("button", { name: "npm-token" })).toBeNull();
  });

  it("no longer suggests the legacy github-pat/gitlab-pat names (superseded by git-pat-<slug>)", () => {
    render(<AddSecretDialog open onOpenChange={() => {}} />);
    expect(screen.queryByRole("button", { name: "github-pat" })).toBeNull();
    expect(screen.queryByRole("button", { name: "gitlab-pat" })).toBeNull();
  });
});

// L2: AddSecretDialog's locked, host-aware mode (design "Prompt D") — the SCM
// ladder/Credentials quick-add open this with lockName+host+lane instead of a
// blank editable dialog. The blank-name/rotate paths above are pinned to their
// EXISTING behavior and must stay green untouched by this mode.
describe("AddSecretDialog — locked, host-aware mode (L2)", () => {
  beforeEach(() => {
    setSecretMock.mockReset();
    setSecretMock.mockResolvedValue(undefined);
  });

  it("locks the Name field: readOnly, aria-readonly, and immune to attempted edits", () => {
    render(
      <AddSecretDialog
        open
        onOpenChange={() => {}}
        lockName
        host="dev.azure.com"
        lane="pat"
        initialName="git-pat-dev-azure-com"
      />,
    );
    const nameInput = screen.getByLabelText(/name/i) as HTMLInputElement;
    expect(nameInput).toHaveAttribute("readonly");
    expect(nameInput).toHaveAttribute("aria-readonly", "true");
    expect(nameInput.className).toMatch(/font-mono/);

    fireEvent.change(nameInput, { target: { value: "not-the-real-name" } });
    expect(nameInput.value).toBe("git-pat-dev-azure-com");
  });

  it("renders the host fact block exactly once, with the given lane chip — never as an input", () => {
    render(
      <AddSecretDialog
        open
        onOpenChange={() => {}}
        lockName
        host="dev.azure.com"
        lane="pat"
        initialName="git-pat-dev-azure-com"
      />,
    );
    // The host is a fact (Mono text), not a labelled/editable field.
    expect(screen.getByText("dev.azure.com")).toBeInTheDocument();
    expect(screen.queryByLabelText(/host/i)).toBeNull();
    expect(screen.getByText(LANE_META.pat.label)).toBeInTheDocument();
  });

  it("defaults the lane chip via laneOfName(name) when `lane` is omitted", () => {
    render(
      <AddSecretDialog
        open
        onOpenChange={() => {}}
        lockName
        host="github.com"
        initialName="ssh-key-github-com"
      />,
    );
    expect(screen.getByText(LANE_META.ssh.label)).toBeInTheDocument();
  });

  it("Name helper names the real host", () => {
    render(
      <AddSecretDialog
        open
        onOpenChange={() => {}}
        lockName
        host="ghes.corp.internal"
        lane="pat"
        initialName="git-pat-ghes-corp-internal"
      />,
    );
    // The name is a CONVENTION, not a binding: nothing server-side maps a host
    // to a secret name, so the hint must point at the grant that does bind it
    // (and must not repeat the host — the fact block above already shows it).
    expect(screen.getByText(/locked — the conventional name for this host/i)).toBeInTheDocument();
    expect(screen.getByText(/git_pat grant that names it/i)).toBeInTheDocument();
  });

  it("falls back to a locked-but-generic helper when no host is given", () => {
    render(
      <AddSecretDialog open onOpenChange={() => {}} lockName initialName="some-locked-name" />,
    );
    expect(screen.getByText(/locked\. a run reaches it through a git_pat grant/i)).toBeInTheDocument();
    // No host to claim, so no fact block — this is the "locked, no host" case,
    // distinct from the untouched generic blank-name dialog (that one isn't
    // locked at all; this one is locked but has nothing to attribute).
    expect(screen.queryByText(LANE_META.pat.label)).toBeNull();
  });

  it("keeps the overwrite warning visible together with the host block — the moment of decision", async () => {
    render(
      <AddSecretDialog
        open
        onOpenChange={() => {}}
        lockName
        host="ghes.corp.internal"
        lane="pat"
        initialName="git-pat-ghes-corp-internal"
        existingNames={["git-pat-ghes-corp-internal"]}
      />,
    );
    fireEvent.change(screen.getByLabelText(/value/i), { target: { value: "sk-new" } });

    // Both visible at once: the operator must see WHICH host before confirming.
    expect(screen.getByText("ghes.corp.internal")).toBeInTheDocument();
    expect(screen.getByText(/already exists/i)).toBeInTheDocument();

    const save = screen.getByRole("button", { name: /overwrites/i });
    fireEvent.click(save);
    const overwrite = await screen.findByRole("button", { name: /^overwrite secret$/i });
    // Host block still present at the actual confirm click, not just before it.
    expect(screen.getByText("ghes.corp.internal")).toBeInTheDocument();
    fireEvent.click(overwrite);
    await waitFor(() =>
      expect(setSecretMock).toHaveBeenCalledWith("git-pat-ghes-corp-internal", "sk-new"),
    );
  });

  it("drops the separate write-only Value hint in locked mode (the description already said it)", () => {
    render(
      <AddSecretDialog
        open
        onOpenChange={() => {}}
        lockName
        host="dev.azure.com"
        lane="pat"
        initialName="git-pat-dev-azure-com"
      />,
    );
    expect(screen.getByText(/write-only.*never read back/i)).toBeInTheDocument();
    expect(screen.queryByText(/cleared on save/i)).toBeNull();
  });
});

// Role-aware console: a viewer reads Secrets but can't write. Every entry
// point (screen + the shared AddSecretDialog) must disable, name the reason,
// and never let a viewer actually reach setSecret/deleteSecret — the server
// would 403 it anyway, but the point is the console never lets it get there.
describe("SecretsScreen / AddSecretDialog — role-aware (viewer vs operator)", () => {
  beforeEach(() => {
    listSecretsMock.mockReset().mockResolvedValue(["anthropic-api-key"]);
    setSecretMock.mockReset().mockResolvedValue(undefined);
  });

  it("operator (today's default, no provider needed): Add secret is enabled with no reason shown", async () => {
    render(<SecretsScreen />);
    await screen.findByText("anthropic-api-key");
    const addBtn = screen.getByRole("button", { name: /add secret/i });
    expect(addBtn).not.toBeDisabled();
    expect(screen.queryByText(/requires the operator role/i)).not.toBeInTheDocument();
  });

  it("viewer: Add secret is disabled and names the reason", async () => {
    render(
      <OperatorProvider operator={false}>
        <SecretsScreen />
      </OperatorProvider>,
    );
    await screen.findByText("anthropic-api-key");
    expect(screen.getByRole("button", { name: /add secret/i })).toBeDisabled();
    expect(screen.getByText(/requires the operator role/i)).toBeInTheDocument();
  });

  it("viewer: the row's Rotate and Delete actions are disabled, each naming the reason", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <OperatorProvider operator={false}>
        <SecretsScreen />
      </OperatorProvider>,
    );
    const menuBtn = await screen.findByRole("button", { name: /secret actions/i });
    await user.click(menuBtn);

    const rotate = await screen.findByRole("menuitem", { name: /rotate/i });
    const del = screen.getByRole("menuitem", { name: /delete/i });
    expect(rotate).toHaveAttribute("data-disabled");
    expect(del).toHaveAttribute("data-disabled");
    // Radix marks disabled items aria-disabled but keeps them in the a11y tree
    // (unlike a bare `disabled` attribute) — the reason is real content, not a
    // hover-only title, so it's there either way.
    expect(within(rotate).getByText(/requires the operator role/i)).toBeInTheDocument();
    expect(within(del).getByText(/requires the operator role/i)).toBeInTheDocument();
  });

  it("viewer: AddSecretDialog's own Save stays disabled even with a valid name+value (defense in depth for every other embedding)", () => {
    render(
      <OperatorProvider operator={false}>
        <AddSecretDialog open onOpenChange={() => {}} />
      </OperatorProvider>,
    );
    fireEvent.change(screen.getByLabelText(/name/i), { target: { value: "openai-api-key" } });
    fireEvent.change(screen.getByLabelText(/value/i), { target: { value: "sk-new" } });
    expect(screen.getByText(/requires the operator role/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /save secret/i })).toBeDisabled();
  });

  it("operator: AddSecretDialog saves normally (unchanged from today)", async () => {
    render(<AddSecretDialog open onOpenChange={() => {}} />);
    fireEvent.change(screen.getByLabelText(/name/i), { target: { value: "openai-api-key" } });
    fireEvent.change(screen.getByLabelText(/value/i), { target: { value: "sk-new" } });
    fireEvent.click(screen.getByRole("button", { name: /save secret/i }));
    await waitFor(() => expect(setSecretMock).toHaveBeenCalledWith("openai-api-key", "sk-new"));
  });
});
