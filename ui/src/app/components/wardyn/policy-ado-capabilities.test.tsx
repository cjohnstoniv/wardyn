/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { RunPolicySpec } from "../../lib/types";
import { ADO_CAPABILITIES } from "../../lib/ado-capabilities";
import { ADO_ACCESS } from "../../lib/ado-access-copy";
import { ADO_ENTRA_EDITOR } from "../../lib/workspace-providers-copy";
import { ADOCapabilitiesSection } from "./policy-ado-capabilities";

const BASE: RunPolicySpec = {
  allowed_domains: [],
  first_use_approval: "always_deny",
  min_confinement_class: "CC2",
};

// The per-area packet's row (Member · State 3), as its locks show it.
const CEILING = [
  "code_read", "code_write", "pr", "policy_admin", "work_read", "work_write", "wiki_read",
  "build_read", "packaging_read", "project_read",
];

function Harness({ initial, seen, ceiling }: { initial: RunPolicySpec; seen: RunPolicySpec[]; ceiling?: string[] }) {
  const [spec, setSpec] = React.useState(initial);
  return (
    <ADOCapabilitiesSection
      spec={spec}
      ceiling={ceiling}
      onSpecChange={(next) => {
        seen.push(next);
        setSpec(next);
      }}
    />
  );
}

const box = (name: string) => screen.getByRole("checkbox", { name: new RegExp(`^${name}`) });

describe("ADOCapabilitiesSection — the mock's “Azure DevOps access” section", () => {
  it("shows the seven Azure DevOps areas, in order, with the section lead and the High-risk legend", () => {
    render(<Harness initial={BASE} seen={[]} />);
    expect(screen.getByText(ADO_ACCESS.SECTION_TITLE)).toBeInTheDocument();
    expect(screen.getByText(ADO_ACCESS.SECTION_LEAD)).toBeInTheDocument();
    expect(screen.getAllByRole("group").map((g) => g.querySelector("legend")?.textContent)).toEqual([
      "Repos",
      "Boards",
      "Wiki",
      "Pipelines",
      "Artifacts",
      "Test Plans",
      "Organization",
    ]);
    expect(within(screen.getByRole("group", { name: "Boards" })).getAllByRole("checkbox")).toHaveLength(3);
    const legend = screen.getByText(ADO_ACCESS.HIGH_RISK_WARN_MEMBER).closest("p")!;
    expect(within(legend).getByText(ADO_ENTRA_EDITOR.HIGH_RISK_BADGE)).toBeInTheDocument();
  });

  it("keeps each High-risk row inside its area, with the badge and the red left edge", () => {
    render(<Harness initial={BASE} seen={[]} />);
    const n = ADO_CAPABILITIES.filter((c) => c.highRisk).length;
    const badges = screen.getAllByText(ADO_ENTRA_EDITOR.HIGH_RISK_BADGE).filter((el) => el.closest("label"));
    expect(badges).toHaveLength(n);
    const rows = document.querySelectorAll("li[data-high-risk]");
    expect(rows).toHaveLength(n);
    for (const row of rows) expect(row).toHaveClass("border-l-danger");
    expect(box("Edit branch policies").closest("fieldset")).toBe(screen.getByRole("group", { name: "Repos" }));
    expect(box("Read code").closest("li")).not.toHaveClass("border-l-danger");
  });

  it("writes the checked set in catalogue order, and drops the key when nothing is checked", async () => {
    const seen: RunPolicySpec[] = [];
    render(<Harness initial={BASE} seen={seen} ceiling={CEILING} />);
    const picks = ["View projects & teams", "Contribute to pull requests", "Read code", "Push to the run's own branch"];
    for (const name of picks) await userEvent.click(box(name));
    expect(seen.at(-1)?.azure_devops_capabilities).toEqual(["code_read", "code_write", "pr", "project_read"]);
    for (const name of picks) await userEvent.click(box(name));
    expect(seen.at(-1)).not.toHaveProperty("azure_devops_capabilities");
    expect(seen.at(-1)).toEqual(BASE);
  });

  it("locks what the ceiling does not grant: disabled, crossed, struck, and says why on hover and to a screen reader", async () => {
    const seen: RunPolicySpec[] = [];
    render(<Harness initial={BASE} seen={seen} ceiling={CEILING} />);
    const locked = box("Manage service connections");
    expect(locked).toBeDisabled();
    expect(locked).toHaveAccessibleName(`Manage service connections ${ADO_ENTRA_EDITOR.HIGH_RISK_BADGE} ${ADO_ACCESS.LOCKED}`);
    const row = locked.closest("li")!;
    expect(row).toHaveAttribute("title", ADO_ACCESS.LOCKED);
    expect(row).toHaveAttribute("aria-disabled", "true");
    expect(within(row).getByText("✕")).toHaveAttribute("aria-hidden", "true");
    expect(within(row).getByText("Manage service connections")).toHaveClass("line-through");
    // The label keeps its size token beside a colour class (cn() dropped it).
    expect(within(row).getByText("Manage service connections").closest("label")).toHaveClass("text-body");
    expect(within(row).getByText("✕")).toHaveClass("text-meta", "text-danger");
    expect(within(row).getByText(ADO_ENTRA_EDITOR.HIGH_RISK_BADGE)).toHaveClass("text-meta", "text-danger");
    await userEvent.click(within(row).getByText("Manage service connections"));
    expect(seen).toHaveLength(0);
    // What the ceiling grants is not locked.
    expect(box("Edit branch policies")).toBeEnabled();
    expect(box("Edit branch policies").closest("li")).not.toHaveAttribute("title");
    // Locked count = everything off the ceiling.
    expect(screen.getAllByTitle(ADO_ACCESS.LOCKED)).toHaveLength(ADO_CAPABILITIES.length - CEILING.length);
  });

  it("lets a stored policy's off-ceiling capability be taken out, never put back", async () => {
    const seen: RunPolicySpec[] = [];
    render(<Harness initial={{ ...BASE, azure_devops_capabilities: ["code_read", "wiki_write"] }} seen={seen} ceiling={CEILING} />);
    const wiki = box("Edit wikis");
    expect(wiki).toBeEnabled();
    expect(wiki).toBeChecked();
    expect(wiki.closest("li")).toHaveAttribute("title", ADO_ACCESS.LOCKED);
    await userEvent.click(wiki);
    expect(seen.at(-1)?.azure_devops_capabilities).toEqual(["code_read"]);
    expect(box("Edit wikis")).toBeDisabled();
  });

  it("locks nothing when the ceiling is unknown", () => {
    render(<Harness initial={BASE} seen={[]} />);
    expect(screen.queryAllByTitle(ADO_ACCESS.LOCKED)).toHaveLength(0);
    for (const b of screen.getAllByRole("checkbox")) expect(b).toBeEnabled();
  });

  it("reads a non-list value as nothing checked instead of throwing", () => {
    const junk = { ...BASE, azure_devops_capabilities: "read" } as unknown as RunPolicySpec;
    render(<Harness initial={junk} seen={[]} />);
    for (const b of screen.getAllByRole("checkbox")) expect(b).not.toBeChecked();
  });
});
