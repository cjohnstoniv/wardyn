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
import { ADOCapabilitiesSection } from "./policy-ado-capabilities";

const BASE: RunPolicySpec = {
  allowed_domains: [],
  first_use_approval: "always_deny",
  min_confinement_class: "CC2",
};

// The mock's row (Member · State 4): read, contribute and policy_admin only.
const CEILING = ["read", "code_write", "pr", "policy_admin"];

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
  it("shows the five groups, in order, with the section lead", () => {
    render(<Harness initial={BASE} seen={[]} />);
    expect(screen.getByText(ADO_ACCESS.SECTION_TITLE)).toBeInTheDocument();
    expect(screen.getByText(ADO_ACCESS.SECTION_LEAD)).toBeInTheDocument();
    expect(screen.getAllByRole("group").map((g) => g.querySelector("legend")?.textContent)).toEqual([
      "Read",
      "Contribute",
      "Work tracking",
      "Pipelines & packages",
      "High risk",
    ]);
    expect(within(screen.getByRole("group", { name: "Contribute" })).getAllByRole("checkbox")).toHaveLength(2);
  });

  it("badges every high-risk capability and warns once on its group", () => {
    render(<Harness initial={BASE} seen={[]} />);
    const risky = screen.getByRole("group", { name: "High risk" });
    const n = ADO_CAPABILITIES.filter((c) => c.highRisk).length;
    // The badge sits inside each capability's label; the group's own heading
    // says "High risk" too, outside any label.
    const badges = (scope: HTMLElement) =>
      within(scope).getAllByText(ADO_ACCESS.HIGH_RISK_BADGE).filter((el) => el.closest("label"));
    expect(badges(risky)).toHaveLength(n);
    expect(within(risky).getByText(ADO_ACCESS.HIGH_RISK_WARN_MEMBER)).toBeInTheDocument();
    expect(badges(document.body)).toHaveLength(n);
  });

  it("writes the checked set in catalogue order, and drops the key when nothing is checked", async () => {
    const seen: RunPolicySpec[] = [];
    render(<Harness initial={BASE} seen={seen} ceiling={CEILING} />);
    const picks = ["Open pull requests", "Read", "Push to the run’s own branch"];
    for (const name of picks) await userEvent.click(box(name));
    expect(seen.at(-1)?.azure_devops_capabilities).toEqual(["code_write", "pr", "read"]);
    for (const name of picks) await userEvent.click(box(name));
    expect(seen.at(-1)).toEqual(BASE);
  });

  it("locks what the ceiling does not grant: disabled, crossed, struck, and says why on hover and to a screen reader", async () => {
    const seen: RunPolicySpec[] = [];
    render(<Harness initial={BASE} seen={seen} ceiling={CEILING} />);
    const locked = box("Manage service connections");
    expect(locked).toBeDisabled();
    expect(locked).toHaveAccessibleName(`Manage service connections ${ADO_ACCESS.HIGH_RISK_BADGE} ${ADO_ACCESS.LOCKED}`);
    const row = locked.closest("li")!;
    expect(row).toHaveAttribute("title", ADO_ACCESS.LOCKED);
    expect(row).toHaveAttribute("aria-disabled", "true");
    expect(within(row).getByText("✕")).toHaveAttribute("aria-hidden", "true");
    expect(within(row).getByText("Manage service connections")).toHaveClass("line-through");
    await userEvent.click(within(row).getByText("Manage service connections"));
    expect(seen).toHaveLength(0);
    // What the ceiling grants is not locked.
    expect(box("Change branch policies")).toBeEnabled();
    expect(box("Change branch policies").closest("li")).not.toHaveAttribute("title");
    // Locked count = everything off the ceiling.
    expect(screen.getAllByTitle(ADO_ACCESS.LOCKED)).toHaveLength(ADO_CAPABILITIES.length - CEILING.length);
  });

  it("lets a stored policy's off-ceiling capability be taken out, never put back", async () => {
    const seen: RunPolicySpec[] = [];
    render(<Harness initial={{ ...BASE, azure_devops_capabilities: ["read", "wiki_write"] }} seen={seen} ceiling={CEILING} />);
    const wiki = box("Wiki");
    expect(wiki).toBeEnabled();
    expect(wiki).toBeChecked();
    expect(wiki.closest("li")).toHaveAttribute("title", ADO_ACCESS.LOCKED);
    await userEvent.click(wiki);
    expect(seen.at(-1)?.azure_devops_capabilities).toEqual(["read"]);
    expect(box("Wiki")).toBeDisabled();
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
