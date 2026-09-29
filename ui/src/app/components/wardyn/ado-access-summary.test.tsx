/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { ADO_ENTRA_EDITOR } from "../../lib/workspace-providers-copy";
import { ADOAccessSummary, adoAccessSummary } from "./ado-access-summary";

describe("ADOAccessSummary — the saved-policy summary line", () => {
  it("renders the mock's two example policies verbatim", () => {
    const { container, unmount } = render(<ADOAccessSummary caps={["read", "code_write", "pr"]} />);
    expect(container.textContent).toBe("Azure DevOps: Read · Contribute");
    expect(screen.queryByText(ADO_ENTRA_EDITOR.HIGH_RISK_BADGE)).toBeNull();
    unmount();
    const admin = render(<ADOAccessSummary caps={["read", "policy_admin"]} />);
    expect(admin.container.textContent).toBe("Azure DevOps: Read · Change branch policies High risk");
    expect(screen.getByText(ADO_ENTRA_EDITOR.HIGH_RISK_BADGE)).toBeInTheDocument();
  });

  it("names a partly chosen group by capability, and never folds high risk into a group", () => {
    expect(adoAccessSummary(["code_write", "work_write", "wiki_write"])).toEqual({
      parts: ["Push to the run's own branch", "Work tracking"],
      highRisk: false,
    });
    expect(adoAccessSummary(["policy_admin", "policy_bypass", "repo_admin", "security_admin",
      "serviceendpoint_admin", "build_admin", "project_admin"]).parts).toHaveLength(7);
  });

  it("says nothing for a policy that names no capabilities, or a malformed value", () => {
    for (const caps of [undefined, [], "read", { read: true }]) {
      const { container, unmount } = render(<ADOAccessSummary caps={caps} />);
      expect(container).toBeEmptyDOMElement();
      unmount();
    }
  });
});
