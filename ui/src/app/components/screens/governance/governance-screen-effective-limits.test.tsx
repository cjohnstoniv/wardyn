/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GovernanceScreen's Limits column for a profile with a base: it reads the effective limits, not the stored `{}`.
// Split from governance-screen.test.tsx, which is at the file-size cap.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

vi.mock("../../../lib/api/runs", () => ({ runs: { gradePolicy: () => Promise.resolve({ overall_risk: "medium", risk_assessment: [] }) } }));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

const getGovernanceMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return {
    ...actual,
    governance: {
      getGovernance: () => getGovernanceMock(),
      previewGovernance: () => Promise.resolve({}),
      listChanges: () => Promise.resolve([]),
    },
  };
});
vi.mock("../../../lib/api/directory", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/directory")>("../../../lib/api/directory");
  return { ...actual, directory: { search: () => Promise.resolve(null) } };
});
const getDefaultPolicyMock = vi.fn();
vi.mock("../../../lib/api/policies", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/policies")>("../../../lib/api/policies");
  return { ...actual, policies: { ...actual.policies, getDefaultPolicy: () => getDefaultPolicyMock() } };
});
vi.mock("../../../lib/api/user-types", () => ({ userTypes: { listUserTypes: () => Promise.resolve([]) } }));

import type { GovernanceUnit, GovernanceSnapshot } from "../../../lib/api/governance";
import { GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { GovernanceScreen } from "./governance-screen";
import { aheadByHours } from "../../../lib/test-clock";

const SPEC = { allowed_domains: ["api.anthropic.com"], first_use_approval: "deny_with_review", min_confinement_class: "CC2" } as const;
const unit = (over: Partial<GovernanceUnit>): GovernanceUnit => ({
  id: "p1",
  name: "Greenfield contractors",
  ceiling: { ...SPEC, allowed_domains: [...SPEC.allowed_domains] },
  limits: {},
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
  ...over,
});
const PLATFORM = unit({ id: "p2", name: "Platform" });

async function rowFor(profile: GovernanceUnit) {
  const snap: GovernanceSnapshot = { profiles: [profile, PLATFORM], assignments: [] };
  getGovernanceMock.mockResolvedValue(snap);
  render(
    <MemoryRouter>
      <OperatorProvider operator securityOperator>
        <GovernanceScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
  await screen.findByText(profile.name);
  const rows = within(screen.getAllByRole("table")[0]).getAllByRole("row");
  return within(rows.find((r) => within(r).queryByText(profile.name))!);
}

beforeEach(() => {
  getGovernanceMock.mockReset();
  getDefaultPolicyMock.mockReset().mockResolvedValue({ min_confinement_class: "CC2" });
  window.localStorage.clear();
});

describe("GovernanceScreen — Limits of a based-on profile", () => {
  // A profile with a base stores `limits: {}`; what binds is `effective.limits`.
  it("shows a narrowed limit, not None", async () => {
    const row = await rowFor(unit({ parent_id: "p2", effective: { ceiling: unit({}).ceiling, limits: { deny_interactive: true } } }));
    expect(row.getByText(GOV.LIMIT_INTERACTIVE_LABEL)).toBeInTheDocument();
    expect(row.queryByText(GOV.LIMITS_NONE)).toBeNull();
  });

  it("says why the effective limits could not be worked out, not None", async () => {
    const row = await rowFor(unit({ effective: { ceiling: {} as never, limits: {}, error: "The base profile was deleted." } }));
    expect(row.getByText("The base profile was deleted.")).toBeInTheDocument();
    expect(row.queryByText(GOV.LIMITS_NONE)).toBeNull();
  });

  it("a profile with no limits anywhere still reads None", async () => {
    const row = await rowFor(unit({ effective: { ceiling: unit({}).ceiling, limits: {} } }));
    expect(row.getByText(GOV.LIMITS_NONE)).toBeInTheDocument();
  });
});
