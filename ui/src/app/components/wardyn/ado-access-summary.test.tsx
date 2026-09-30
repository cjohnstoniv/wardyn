/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { ADO_ENTRA_EDITOR } from "../../lib/workspace-providers-copy";
import { ADOAccessSummary, adoAccessSummary } from "./ado-access-summary";

// The four saved policies of the approved per-area packet (Member · State 4),
// with the summary line each one reads as.
const TWELVE_READS = [
  "code_read", "work_read", "wiki_read", "build_read", "release_read", "serviceendpoint_read",
  "library_read", "packaging_read", "test_read", "project_read", "identity_read", "analytics_read",
];
const STATE_4 = [
  { caps: [...TWELVE_READS, "code_write", "pr"], line: "Azure DevOps: Read (every area) · Repos" },
  { caps: ["code_read", "work_read", "work_write", "project_read"], line: "Azure DevOps: Read code · Boards · View projects & teams" },
  { caps: ["code_read", "policy_admin"], line: "Azure DevOps: Read code · Edit branch policies High risk" },
  { caps: ["work_read", "work_write", "work_admin"], line: "Azure DevOps: Boards · Delete work items & manage work tracking High risk" },
];

describe("ADOAccessSummary — the saved-policy summary line", () => {
  it("renders the per-area packet's four example policies verbatim", () => {
    for (const { caps, line } of STATE_4) {
      const { container, unmount } = render(<ADOAccessSummary caps={caps} />);
      expect(container.textContent).toBe(line);
      unmount();
    }
    render(<ADOAccessSummary caps={["code_read", "policy_admin"]} />);
    // Both the size token and the danger tone survive: a cn()/tailwind-merge
    // badge dropped text-danger in favour of text-meta.
    expect(screen.getByText(ADO_ENTRA_EDITOR.HIGH_RISK_BADGE)).toHaveClass("text-meta", "text-danger");
  });

  it("folds every read to one phrase, and then leaves the reads out of each area", () => {
    expect(adoAccessSummary(TWELVE_READS)).toEqual({ parts: ["Read (every area)"], highRisk: false });
    // Eleven reads are not every read: each area names its own.
    expect(adoAccessSummary(TWELVE_READS.slice(1)).parts[0]).toBe("View work items");
  });

  it("names an area whose everyday rows are all chosen, else each row, and never folds high risk", () => {
    expect(adoAccessSummary(["code_write", "wiki_read", "wiki_write"])).toEqual({
      parts: ["Push to the run's own branch", "Wiki"],
      highRisk: false,
    });
    expect(adoAccessSummary(["test_read"]).parts).toEqual(["Test Plans"]);
    const risky = adoAccessSummary(["policy_admin", "policy_bypass", "repo_admin", "work_admin", "build_admin",
      "release_admin", "serviceendpoint_admin", "packaging_manage", "project_admin", "security_admin"]);
    expect(risky.parts).toHaveLength(10);
    expect(risky.highRisk).toBe(true);
    // A High-risk row does not count toward folding its area.
    expect(adoAccessSummary(["work_read", "work_write", "work_admin"]).parts).toEqual([
      "Boards",
      "Delete work items & manage work tracking",
    ]);
  });

  it("says nothing for a policy that names no capabilities, or a malformed value", () => {
    for (const caps of [undefined, [], "read", { read: true }]) {
      const { container, unmount } = render(<ADOAccessSummary caps={caps} />);
      expect(container).toBeEmptyDOMElement();
      unmount();
    }
  });
});
