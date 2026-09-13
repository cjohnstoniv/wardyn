/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { toast } from "sonner";

import { AddWorkspaceDialog } from "./add-workspace-dialog";
import { OperatorProvider } from "../wardyn/operator-context";
import { workspaces as workspacesApi } from "../../lib/api/workspaces";

vi.mock("../../lib/api/setup", () => ({ setup: { getSetupStatus: vi.fn().mockResolvedValue({ runner: { driver: "docker" } }) } }));
vi.mock("../../lib/api/workspaces", () => ({ workspaces: { createWorkspace: vi.fn() } }));
vi.mock("sonner", () => ({ toast: { warning: vi.fn(), error: vi.fn() } }));

// 0.7 §B — the local_dir root hint follows the workspace-OWNERSHIP namespace,
// which /me keys on !isOperator (me.go), NOT on role === "member". A SECURITY
// ADMIN's workspaces are owner-stamped like a member's, so memberSourcesAllowed
// clamps them at authoring time and ValidateMemberMountSource at bind time —
// before this fix the dialog asked `role === "member"` and simply never
// rendered the real member_local_dir_root /me was already sending them, so a
// security admin typed a path with no boundary shown and got refused later.
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

// F1 (V2 walk 1): a repo admitted ONLY by the legacy `scm_hosts` list was
// onboarded in complete silence — the audit row was written at this door and the
// ADMIT.LEGACY_HOST sentence went only to whoever launched the first run. The
// 201 now carries it, and this dialog closes on success with no persistent
// advisory slot, so the warning toast is where it can still be read.
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
