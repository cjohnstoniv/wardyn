/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { toast } from "sonner";

import { AddWorkspaceDialog } from "./add-workspace-dialog";
import { OperatorProvider } from "../wardyn/operator-context";
import { workspaces as workspacesApi } from "../../lib/api/workspaces";
import { WORKSPACE_DETAIL_DRAFT } from "../../lib/workspace-copy";

vi.mock("../../lib/api/setup", () => ({ setup: { getSetupStatus: vi.fn().mockResolvedValue({ runner: { driver: "docker" } }) } }));
vi.mock("../../lib/api/workspaces", () => ({ workspaces: { createWorkspace: vi.fn() } }));
vi.mock("sonner", () => ({ toast: { warning: vi.fn(), error: vi.fn() } }));

// 0.7 §B — the local_dir root hint follows the workspace-ownership namespace,
// which /me keys on !isOperator (me.go), not on role === "user". A security
// admin's workspaces are owner-stamped like a member's, so memberSourcesAllowed
// clamps them at authoring time and ValidateMemberMountSource at bind time —
// asking `role === "user"` here instead would never render the real
// member_local_dir_root /me is already sending them, so a security admin
// would type a path with no boundary shown and get refused later.
function renderDialog(operator: boolean, root: string | null) {
  return render(
    <OperatorProvider operator={operator} memberLocalDirRoot={root}>
      <AddWorkspaceDialog existingNames={[]} onClose={() => {}} onCreated={() => {}} />
    </OperatorProvider>,
  );
}

// OptionCard is an aria-pressed <button>, and its label grows the
// "· unavailable" suffix in the no-root case — match the prefix.
async function pickLocalDir() {
  await userEvent.click(screen.getByRole("button", { name: /^local directory/i }));
}

describe("AddWorkspaceDialog — the local_dir root constraint follows !operator, not role", () => {
  it("shows a security admin (operator:false) the root hint /me sent them", async () => {
    renderDialog(false, "/home/agent-projects");
    await pickLocalDir();
    expect(screen.getByText(/must be under \/home\/agent-projects/i)).toBeInTheDocument();
  });

  it("shows a security admin with NO configured root the unavailable explainer, not a path field", async () => {
    renderDialog(false, null);
    await pickLocalDir();
    expect(screen.getByText(/local directories aren't set up for your account/i)).toBeInTheDocument();
    expect(screen.queryByLabelText(/path on this host/i)).not.toBeInTheDocument();
  });

  it("leaves a super admin (operator:true) the unconstrained hint — byte-identical to pre-0.7", async () => {
    renderDialog(true, null);
    await pickLocalDir();
    expect(screen.getByText("Mounted from this machine into the sandbox.")).toBeInTheDocument();
  });
});

// ponytail: the writable-checkbox and submit-payload arms of the same
// `memberClamped` const are not pinned separately — they read the one
// derivation the three tests above already exercise, and the checkbox lives
// inside a collapsed Disclosure, so a query for it passes vacuously either way.

// F1 (V2 walk 1): a repo admitted only by the legacy `scm_hosts` list must
// not onboard in silence — the audit row is written at this door, but
// without surfacing it here the ADMIT.LEGACY_HOST sentence would reach only
// whoever launches the first run. The 201 carries it, and this dialog closes
// on success with no persistent advisory slot, so the warning toast is
// where it can still be read.
describe("AddWorkspaceDialog — the 201's advisory warnings", () => {
  it("says the server's warning sentence verbatim after a successful create", async () => {
    const sentence =
      "host gitlab.com is admitted through the legacy scm_hosts list; enable a provider for it before 0.8";
    vi.mocked(workspacesApi.createWorkspace).mockResolvedValue({
      id: "ws-1",
      name: "legacy-repo",
      warnings: [sentence],
    } as Awaited<ReturnType<typeof workspacesApi.createWorkspace>>);
    render(
      <OperatorProvider operator memberLocalDirRoot={null}>
        <AddWorkspaceDialog existingNames={[]} onClose={() => {}} onCreated={() => {}} />
      </OperatorProvider>,
    );
    await userEvent.type(screen.getByLabelText("Repository URL"), "https://gitlab.com/team/legacy-repo");
    await userEvent.click(screen.getByRole("button", { name: "Add workspace" }));
    expect(vi.mocked(toast.warning)).toHaveBeenCalledWith(sentence);
  });

  it("says nothing when the 201 carries no warnings", async () => {
    vi.mocked(toast.warning).mockClear();
    vi.mocked(workspacesApi.createWorkspace).mockResolvedValue({
      id: "ws-2",
      name: "app",
    } as Awaited<ReturnType<typeof workspacesApi.createWorkspace>>);
    render(
      <OperatorProvider operator memberLocalDirRoot={null}>
        <AddWorkspaceDialog existingNames={[]} onClose={() => {}} onCreated={() => {}} />
      </OperatorProvider>,
    );
    await userEvent.type(screen.getByLabelText("Repository URL"), "acme/app");
    await userEvent.click(screen.getByRole("button", { name: "Add workspace" }));
    expect(vi.mocked(toast.warning)).not.toHaveBeenCalled();
  });
});

// F5-F2: this copy must not claim "you can change everything later" — a
// workspace is create-only in this console (no Edit path; updateWorkspace
// has zero production callers, workspaces.test.tsx's "kebab is Open ·
// Delete… only" describe pins the kebab menu to exactly Open/Delete). Delete
// the sentence, keep the rest of the description.
describe("AddWorkspaceDialog — no false promise of a later edit", () => {
  // ticket: F5-F2
  it("never claims everything can be changed later", () => {
    renderDialog(true, null);
    expect(screen.queryByText(/you can change everything later/i)).not.toBeInTheDocument();
    expect(
      screen.getByText("A workspace is a repo or directory a run can attach."),
    ).toBeInTheDocument();
  });
});

// F5-F5: the Branch field's value would be silently dropped for
// local_dir/ephemeral (only the repo submit arm ever reads it) — offer it
// only where it does something, and let Name take the full row when it's
// gone.
describe("AddWorkspaceDialog — Branch only where it's wired", () => {
  // ticket: F5-F5
  it("shows Branch for a repo source", () => {
    renderDialog(true, null);
    expect(screen.getByLabelText(/branch/i)).toBeInTheDocument();
  });

  it("hides Branch for Empty — Name takes the full row", async () => {
    renderDialog(true, null);
    await userEvent.click(screen.getByRole("button", { name: /^empty/i }));
    expect(screen.queryByLabelText(/branch/i)).not.toBeInTheDocument();
    expect(screen.getByLabelText(/^name$/i)).toBeInTheDocument();
  });

  it("hides Branch for Local directory", async () => {
    renderDialog(true, "/home/agent-projects");
    await pickLocalDir();
    expect(screen.queryByLabelText(/branch/i)).not.toBeInTheDocument();
  });
});

// F5-F6: picking "devcontainer.json" would store {kind:"recommended"}, which
// workspaceImage() collapses back into the absent case with no profile yet
// (the dialog runs no scan) — an honest chip would then contradict the pick
// the operator just made. Collapse the two dishonest choices into one "Auto"
// option that stores nothing at all; Pinned stays the one explicit choice.
describe("AddWorkspaceDialog — one honest Auto image choice", () => {
  // ticket: F5-F6
  async function openImagePicker() {
    await userEvent.click(screen.getByRole("button", { name: /advanced/i }));
  }

  it("offers exactly Auto and Pinned image ref — no separate devcontainer/standard choices", async () => {
    renderDialog(true, null);
    await openImagePicker();
    const group = screen.getByRole("radiogroup", { name: /container image/i });
    expect(within(group).getAllByRole("button")).toHaveLength(2);
    expect(
      within(group).getByRole("button", {
        name: new RegExp(`^${WORKSPACE_DETAIL_DRAFT.ADD_WORKSPACE_IMAGE_AUTO_TITLE}`, "i"),
      }),
    ).toBeInTheDocument();
    expect(within(group).getByRole("button", { name: /pinned image ref/i })).toBeInTheDocument();
    expect(within(group).queryByRole("button", { name: /^devcontainer\.json$/i })).not.toBeInTheDocument();
    expect(within(group).queryByRole("button", { name: /^standard sandbox image$/i })).not.toBeInTheDocument();
  });

  it("Auto stores nothing — no base_image on the create call", async () => {
    vi.mocked(workspacesApi.createWorkspace).mockResolvedValue({
      id: "ws-3",
      name: "app",
    } as Awaited<ReturnType<typeof workspacesApi.createWorkspace>>);
    renderDialog(true, null);
    await userEvent.type(screen.getByLabelText("Repository URL"), "acme/app");
    await userEvent.click(screen.getByRole("button", { name: "Add workspace" }));
    const call = vi.mocked(workspacesApi.createWorkspace).mock.calls[0][0];
    expect(call).not.toHaveProperty("base_image");
  });
});
