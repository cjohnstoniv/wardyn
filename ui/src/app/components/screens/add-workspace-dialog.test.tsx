/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { AddWorkspaceDialog } from "./add-workspace-dialog";
import { OperatorProvider } from "../wardyn/operator-context";

vi.mock("../../lib/api/setup", () => ({ setup: { getSetupStatus: vi.fn().mockResolvedValue({ runner: { driver: "docker" } }) } }));
vi.mock("../../lib/api/workspaces", () => ({ workspaces: { create: vi.fn() } }));

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
