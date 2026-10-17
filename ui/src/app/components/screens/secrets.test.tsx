/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { WorkspaceProvidersSnapshot } from "../../lib/api/providers";

// Fixes pinned here:
//  - a failed deleteSecret() must surface a toast.error.
//  - AddSecretDialog must warn before overwriting an existing secret name.

const toastError = vi.fn();
const toastSuccess = vi.fn();
vi.mock("sonner", () => ({
  toast: { error: (...a: unknown[]) => toastError(...a), success: (...a: unknown[]) => toastSuccess(...a) },
}));

const listSecretsMock = vi.fn();
const deleteSecretMock = vi.fn();
const setSecretMock = vi.fn();
const listSecretsMineMock = vi.fn();
vi.mock("../../lib/api/secrets", () => ({
  secrets: {
    listSecrets: () => listSecretsMock(),
    listSecretsMine: () => listSecretsMineMock(),
    deleteSecret: (...a: unknown[]) => deleteSecretMock(...a),
    setSecret: (...a: unknown[]) => setSecretMock(...a),
  },
}));

// #381 F3/F8: SecretsScreen reads the real WARDYN_GIT_PAT_BROKER switch off
// GET /workspace-providers when the caller is an operator. Defaults to
// resolving nothing (the fetch's own .catch keeps the ON default) unless a
// test overrides it.
const getWorkspaceProvidersMock = vi.fn<() => Promise<WorkspaceProvidersSnapshot>>(() =>
  Promise.reject(new Error("not mocked")),
);
vi.mock("../../lib/api/providers", () => ({
  providers: { getWorkspaceProviders: () => getWorkspaceProvidersMock() },
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
    fireEvent.change(screen.getByLabelText(/^value$/i), { target: { value: "sk-new" } });

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
    fireEvent.change(screen.getByLabelText(/^value$/i), { target: { value: "sk-other" } });
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

describe("AddSecretDialog — provider chips", () => {
  // ticket: F5
  it("offers provider chips on a blank-name open and prefills the Name field on click", async () => {
    render(<AddSecretDialog open onOpenChange={() => {}} />);
    const user = userEvent.setup();

    const nameInput = screen.getByLabelText(/name/i) as HTMLInputElement;
    expect(nameInput.value).toBe("");

    await user.click(screen.getByRole("button", { name: "npm-token" }));
    expect(nameInput.value).toBe("npm-token");

    // Never touches the Value field — chips prefill the name only.
    expect((screen.getByLabelText(/^value$/i) as HTMLTextAreaElement).value).toBe("");
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
describe("AddSecretDialog — locked, host-aware mode", () => {
  // ticket: L2
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
    // A LITERAL string, not LANE_META.pat.label — asserting through the same
    // constant the component reads is self-referential and would not have
    // caught #381 (the label read "PAT · in-sandbox" here long after the
    // broker went on by default; see the OFF-position test below for the
    // other half of this pin).
    expect(screen.getByText("PAT · brokered")).toBeInTheDocument();
  });

  // #381 F8: the ON-default test above proves nothing about the off
  // position — this is the real coverage the review found missing.
  it("renders the pre-0.7 in-sandbox label when patBrokerEnabled is false", () => {
    render(
      <AddSecretDialog
        open
        onOpenChange={() => {}}
        lockName
        host="dev.azure.com"
        lane="pat"
        initialName="git-pat-dev-azure-com"
        patBrokerEnabled={false}
      />,
    );
    expect(screen.getByText("PAT · in-sandbox")).toBeInTheDocument();
    expect(screen.queryByText("PAT · brokered")).not.toBeInTheDocument();
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
    fireEvent.change(screen.getByLabelText(/^value$/i), { target: { value: "sk-new" } });

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

// #381 F3/F8: the page-header sentence reads the real switch, both positions.
describe("SecretsScreen — the PAT broker sentence in the page header", () => {
  beforeEach(() => {
    listSecretsMock.mockReset().mockResolvedValue([]);
    getWorkspaceProvidersMock.mockReset();
  });

  it("says the brokered sentence when the switch resolves on (the 0.7.10 default)", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({ providers: { git_pat_broker_enabled: true }, etag: null });
    render(<SecretsScreen />);
    await waitFor(() =>
      expect(
        screen.getByText(/A stored git access token is attached to the request by the proxy/),
      ).toBeInTheDocument(),
    );
    expect(screen.queryByText(/Exception:/)).not.toBeInTheDocument();
    expect(screen.queryByText(/is handed to git inside the sandbox/)).not.toBeInTheDocument();
  });

  it("says the pre-0.7 exception sentence when the switch resolves off", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({ providers: { git_pat_broker_enabled: false }, etag: null });
    render(<SecretsScreen />);
    await waitFor(() =>
      expect(screen.getByText(/Exception: A git access token is handed to git inside the sandbox/)).toBeInTheDocument(),
    );
  });
});

// A member writes and deletes their OWN secrets (the API stores under the
// caller and cannot reach another person's row), so the console offers exactly
// that: their rows, with Add, Rotate and Delete, and the admin's paired names
// beside them without actions. The shared dialog still refuses a non-operator
// anywhere that does not say it writes the caller's own row.
describe("SecretsScreen / AddSecretDialog — a member's own secrets", () => {
  beforeEach(() => {
    listSecretsMock.mockReset().mockResolvedValue(["anthropic-api-key"]);
    listSecretsMineMock.mockReset().mockResolvedValue({ names: ["team-key", "my-key"], mine: ["my-key"] });
    setSecretMock.mockReset().mockResolvedValue(undefined);
    deleteSecretMock.mockReset().mockResolvedValue(undefined);
  });

  it("operator (today's default, no provider needed): Add secret is enabled with no reason shown", async () => {
    render(<SecretsScreen />);
    await screen.findByText("anthropic-api-key");
    const addBtn = screen.getByRole("button", { name: /add secret/i });
    expect(addBtn).not.toBeDisabled();
    expect(screen.queryByText(/requires the admin role/i)).not.toBeInTheDocument();
    expect(listSecretsMineMock).not.toHaveBeenCalled();
  });

  it("member: reads their own rows, Add secret is enabled and no admin-only reason shows", async () => {
    render(
      <OperatorProvider operator={false}>
        <SecretsScreen />
      </OperatorProvider>,
    );
    await screen.findByText("my-key");
    expect(listSecretsMineMock).toHaveBeenCalled();
    expect(listSecretsMock).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: /add secret/i })).toBeEnabled();
    expect(screen.queryByText(/requires the admin role/i)).not.toBeInTheDocument();
  });

  it("member: Rotate and Delete act on their own row only; the admin's name has no actions", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <OperatorProvider operator={false}>
        <SecretsScreen />
      </OperatorProvider>,
    );
    await screen.findByText("my-key");
    // Two names are listed; only the member's own has a menu.
    expect(screen.getByText("team-key")).toBeInTheDocument();
    expect(screen.getByText("Provided by your admin")).toBeInTheDocument();
    const menus = screen.getAllByRole("button", { name: /secret actions/i });
    expect(menus).toHaveLength(1);

    await user.click(menus[0]);
    const rotate = await screen.findByRole("menuitem", { name: /rotate/i });
    const del = screen.getByRole("menuitem", { name: /delete/i });
    expect(rotate).not.toHaveAttribute("data-disabled");
    expect(del).not.toHaveAttribute("data-disabled");

    await user.click(del);
    await user.click(await screen.findByRole("button", { name: /delete secret/i }));
    await waitFor(() => expect(deleteSecretMock).toHaveBeenCalledWith("my-key"));
  });

  it("member: Add secret stores the value under the name they typed", async () => {
    render(
      <OperatorProvider operator={false}>
        <SecretsScreen />
      </OperatorProvider>,
    );
    await screen.findByText("my-key");
    fireEvent.click(screen.getByRole("button", { name: /add secret/i }));
    fireEvent.change(await screen.findByLabelText(/^name/i), { target: { value: "acme-key" } });
    fireEvent.change(screen.getByLabelText(/^value$/i), { target: { value: "sk-new-value" } });
    fireEvent.click(screen.getByRole("button", { name: /save secret/i }));
    await waitFor(() => expect(setSecretMock).toHaveBeenCalledWith("acme-key", "sk-new-value"));
  });

  it("member: the empty page says what a member can do, not that an admin must", async () => {
    listSecretsMineMock.mockResolvedValue({ names: [], mine: [] });
    render(
      <OperatorProvider operator={false}>
        <SecretsScreen />
      </OperatorProvider>,
    );
    await screen.findByText(/no secrets yet/i);
    expect(screen.getByText(/stored under your name/i)).toBeInTheDocument();
    expect(screen.queryByText(/requires the admin role/i)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /add your first secret/i })).toBeEnabled();
  });

  it("another embedding of the dialog (no ownRows) still refuses a non-operator", () => {
    render(
      <OperatorProvider operator={false}>
        <AddSecretDialog open onOpenChange={() => {}} />
      </OperatorProvider>,
    );
    fireEvent.change(screen.getByLabelText(/name/i), { target: { value: "openai-api-key" } });
    fireEvent.change(screen.getByLabelText(/^value$/i), { target: { value: "sk-new" } });
    expect(screen.getByText(/requires the admin role/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /save secret/i })).toBeDisabled();
  });

  it("operator: AddSecretDialog saves normally (unchanged from today)", async () => {
    render(<AddSecretDialog open onOpenChange={() => {}} />);
    fireEvent.change(screen.getByLabelText(/name/i), { target: { value: "openai-api-key" } });
    fireEvent.change(screen.getByLabelText(/^value$/i), { target: { value: "sk-new" } });
    fireEvent.click(screen.getByRole("button", { name: /save secret/i }));
    await waitFor(() => expect(setSecretMock).toHaveBeenCalledWith("openai-api-key", "sk-new"));
  });
});

// go-live findings pinned here (the secrets.tsx side of the same findings
// policies.test.tsx pins for policies.tsx):
//  - ui-secretsPolicies-2: AddSecretDialog's save-error must be announced
//    (role=alert) and wired into the Save button's aria-describedby.
//  - ui-secretsPolicies-3: required Name/Value fields must be marked required.
//  - ui-secretsPolicies-4: the empty-state CTA is general-purpose secret
//    storage copy, not LLM-specific.
describe("SecretsScreen — empty-state CTA copy (ui-secretsPolicies-4)", () => {
  it("reads 'Add your first secret', not LLM-specific copy, matching the header button", async () => {
    listSecretsMock.mockReset().mockResolvedValue([]);
    render(<SecretsScreen />);
    await screen.findByText(/no secrets yet/i);

    expect(screen.getByRole("button", { name: /add your first secret/i })).toBeInTheDocument();
    expect(screen.queryByText(/llm key/i)).not.toBeInTheDocument();
  });
});

describe("AddSecretDialog — required fields + error announcement (ui-secretsPolicies-2/3)", () => {
  beforeEach(() => {
    setSecretMock.mockReset();
  });

  it("marks Name and Value as required", () => {
    render(<AddSecretDialog open onOpenChange={() => {}} />);
    expect(screen.getByLabelText(/^name/i)).toBeRequired();
    expect(screen.getByLabelText(/^value/i)).toBeRequired();
  });

  it("announces a rejected save via role=alert and wires it to the Save button", async () => {
    setSecretMock.mockRejectedValue(new Error("HTTP 400: invalid secret name"));
    render(<AddSecretDialog open onOpenChange={() => {}} />);

    fireEvent.change(screen.getByLabelText(/^name/i), { target: { value: "anthropic-api-key" } });
    fireEvent.change(screen.getByLabelText(/^value/i), { target: { value: "sk-ant-x" } });
    fireEvent.click(screen.getByRole("button", { name: /^save secret$/i }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/invalid secret name/i);
    expect(screen.getByRole("button", { name: /^save secret$/i })).toHaveAttribute(
      "aria-describedby",
      alert.id,
    );
  });
});

describe("AddSecretDialog — reveal state", () => {
  // ticket: G3/P4
  // The dialog component stays mounted across close/open — only Radix's content
  // unmounts — so `reveal` survives unless the open-effect resets it. Without
  // that reset, the next Add/Rotate opens showing the previous plaintext.
  // The mask itself is `-webkit-text-security`, which jsdom does not keep, so
  // the toggle's aria-pressed is what `reveal` is read through here; the
  // Playwright lane sees the rendered masking.
  it("re-masks the Value field when the dialog is closed and re-opened", () => {
    const { rerender } = render(<AddSecretDialog open onOpenChange={() => {}} />);
    const toggle = () => screen.getByRole("button", { name: /(show|hide) value/i });

    expect(toggle()).toHaveAttribute("aria-pressed", "false");
    expect(toggle()).toHaveAccessibleName("Show value");

    fireEvent.click(toggle());
    expect(toggle()).toHaveAttribute("aria-pressed", "true");
    expect(toggle()).toHaveAccessibleName("Hide value");

    rerender(<AddSecretDialog open={false} onOpenChange={() => {}} />); // Cancel
    rerender(<AddSecretDialog open onOpenChange={() => {}} />); // re-open

    expect(toggle()).toHaveAttribute("aria-pressed", "false");
    expect(toggle()).toHaveAccessibleName("Show value");
  });
});
