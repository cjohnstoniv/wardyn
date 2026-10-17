/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// AssignmentsBlock's own resolved-profile preview (governance-screen.test.tsx
// covers the rest of the block through the full GovernanceScreen — this file
// is scoped to the one behaviour below, kept separate to avoid a
// file-ownership collision with whatever owns governance-screen.tsx).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

const previewGovernanceMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return {
    ...actual,
    governance: {
      ...actual.governance,
      previewGovernance: (...a: unknown[]) => previewGovernanceMock(...a),
    },
  };
});

const directorySearchMock = vi.fn();
vi.mock("../../../lib/api/directory", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/directory")>("../../../lib/api/directory");
  return { ...actual, directory: { search: (...a: unknown[]) => directorySearchMock(...a) } };
});

const listUserTypesMock = vi.fn();
vi.mock("../../../lib/api/user-types", () => ({
  userTypes: { listUserTypes: () => listUserTypesMock() },
}));

import { GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { PREVIEW } from "../../../lib/people-access-copy";
import { AssignmentsBlock } from "./assignments";
import type { GovernanceSnapshot } from "../../../lib/api/governance";

// PREVIEW_RUN_CTA disables when snapshot.profiles is empty (assignments.tsx)
// — the preview asks a real profile to exist, so this fixture carries one.
const SNAPSHOT: GovernanceSnapshot = {
  profiles: [
    {
      id: "p1",
      name: "Platform",
      ceiling: { allowed_domains: [], first_use_approval: "deny_with_review", min_confinement_class: "CC1" },
      limits: {},
      created_at: "",
      updated_at: "",
    },
  ],
  assignments: [],
};

const STANDARD = { id: "standard", name: "Standard user", description: "", priority: 0, built_in: true, created_at: "", updated_at: "" };
const CONTRACTOR = { id: "contractor", name: "Contractor", description: "", priority: 5, built_in: false, created_at: "", updated_at: "" };

function renderBlock() {
  return render(<AssignmentsBlock snapshot={SNAPSHOT} disabled={false} collapsed={false} onChanged={() => {}} />);
}

beforeEach(() => {
  previewGovernanceMock.mockReset();
  directorySearchMock.mockReset().mockResolvedValue(null);
  listUserTypesMock.mockReset().mockResolvedValue([STANDARD]);
});

describe("AssignmentsBlock — the resolved preview", () => {
  // `result` must clear when `claims` changes — otherwise the previous
  // group's resolved profile sits under new, unrun input as if it were the
  // new group's answer.
  it("typing new claims clears the previous group's stale preview result", async () => {
    previewGovernanceMock.mockResolvedValue({ profile_id: "p1", profile_name: "Platform", matched_tier: "group" });
    renderBlock();

    await userEvent.type(screen.getByLabelText(PREVIEW.FIELD_CLAIMS), "wardyn.platform");
    await userEvent.click(screen.getByRole("button", { name: GOV.PREVIEW_RUN_CTA }));
    expect(await screen.findByText(GOV.PREVIEW_RESULT("Platform", GOV.MATCHED_GROUP))).toBeInTheDocument();

    await userEvent.type(screen.getByLabelText(PREVIEW.FIELD_CLAIMS), "x");
    expect(screen.queryByText(GOV.PREVIEW_RESULT("Platform", GOV.MATCHED_GROUP))).not.toBeInTheDocument();
  });

  // R1-0097: a one-option dropdown is not a choice, so the picker is absent
  // while the built-in Standard user is the only type, and the preview still
  // sends it.
  it("hides the user type picker when only the built-in type exists, and sends standard", async () => {
    previewGovernanceMock.mockResolvedValue({});
    renderBlock();
    await userEvent.type(screen.getByLabelText(PREVIEW.FIELD_CLAIMS), "eng");
    expect(listUserTypesMock).toHaveBeenCalled();
    expect(screen.queryByRole("combobox", { name: GOV.PREVIEW_USER_TYPE_LABEL })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: GOV.PREVIEW_RUN_CTA }));
    expect(previewGovernanceMock).toHaveBeenCalledWith(expect.objectContaining({ user_type: "standard" }));
  });

  it("offers the types, sends the picked id, and clears the last answer when it changes", async () => {
    listUserTypesMock.mockResolvedValue([STANDARD, CONTRACTOR]);
    previewGovernanceMock.mockResolvedValue({ profile_id: "p1", profile_name: "Platform", matched_tier: "user_type" });
    renderBlock();
    const picker = await screen.findByRole("combobox", { name: GOV.PREVIEW_USER_TYPE_LABEL });
    expect(picker).toHaveTextContent("Standard user");
    expect(screen.getByText(GOV.PREVIEW_USER_TYPE_HINT)).toBeInTheDocument();

    await userEvent.type(screen.getByLabelText(PREVIEW.FIELD_CLAIMS), "eng");
    await userEvent.click(picker);
    await userEvent.click(await screen.findByRole("option", { name: "Contractor" }));
    await userEvent.click(screen.getByRole("button", { name: GOV.PREVIEW_RUN_CTA }));
    expect(previewGovernanceMock).toHaveBeenCalledWith(expect.objectContaining({ user_type: "contractor" }));
    expect(await screen.findByText(GOV.PREVIEW_RESULT("Platform", GOV.MATCHED_USER_TYPE))).toBeInTheDocument();

    await userEvent.click(picker);
    await userEvent.click(await screen.findByRole("option", { name: "Standard user" }));
    expect(screen.queryByText(GOV.PREVIEW_RESULT("Platform", GOV.MATCHED_USER_TYPE))).not.toBeInTheDocument();
  });
});
