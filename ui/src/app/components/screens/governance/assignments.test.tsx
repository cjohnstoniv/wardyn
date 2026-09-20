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

function renderBlock() {
  return render(<AssignmentsBlock snapshot={SNAPSHOT} disabled={false} collapsed={false} onChanged={() => {}} />);
}

beforeEach(() => {
  previewGovernanceMock.mockReset();
  directorySearchMock.mockReset().mockResolvedValue(null);
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
});
