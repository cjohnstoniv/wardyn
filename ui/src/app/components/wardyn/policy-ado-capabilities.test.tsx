/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { RunPolicySpec } from "../../lib/types";
import { ADO } from "../../lib/ado-entra-copy";
import { ADO_CAPABILITIES } from "../../lib/ado-capabilities";
import { ADOCapabilitiesSection } from "./policy-ado-capabilities";

const BASE: RunPolicySpec = {
  allowed_domains: [],
  first_use_approval: "always_deny",
  min_confinement_class: "CC2",
};

// The controlled harness: the section writes the spec, the harness feeds it back.
function Harness({ initial, seen }: { initial: RunPolicySpec; seen: RunPolicySpec[] }) {
  const [spec, setSpec] = React.useState(initial);
  return (
    <ADOCapabilitiesSection
      spec={spec}
      onSpecChange={(next) => {
        seen.push(next);
        setSpec(next);
      }}
    />
  );
}

// A capability's checkbox, by its label AND wire name ("Push" alone also
// prefixes "Push past a branch policy").
const box = (label: string, cap: string) =>
  screen.getByRole("checkbox", { name: new RegExp(`^${label}\\s*${cap}$`) });

describe("ADOCapabilitiesSection — azure_devops_capabilities as a checklist", () => {
  it("writes the checked set in catalogue order, and drops the key when nothing is checked", async () => {
    const seen: RunPolicySpec[] = [];
    render(<Harness initial={BASE} seen={seen} />);
    const picks: [string, string][] = [
      [ADO.CAP_PR, "pr"],
      [ADO.CAP_READ, "read"],
      [ADO.CAP_CODE_WRITE, "code_write"],
    ];
    for (const [label, cap] of picks) await userEvent.click(box(label, cap));
    expect(seen.at(-1)?.azure_devops_capabilities).toEqual(["read", "code_write", "pr"]);
    for (const [label, cap] of picks) await userEvent.click(box(label, cap));
    expect(seen.at(-1)).not.toHaveProperty("azure_devops_capabilities");
    expect(seen.at(-1)).toEqual(BASE);
  });

  it("reflects a stored choice, and renders a capability with no canon label as its wire name", () => {
    render(<Harness initial={{ ...BASE, azure_devops_capabilities: ["read", "policy_admin"] }} seen={[]} />);
    expect(box(ADO.CAP_POLICY_ADMIN, "policy_admin")).toBeChecked();
    expect(box(ADO.CAP_READ, "read")).toBeChecked();
    expect(box(ADO.CAP_PR, "pr")).not.toBeChecked();
    expect(screen.getByRole("checkbox", { name: "security_admin" })).not.toBeChecked();
    expect(screen.getAllByRole("checkbox")).toHaveLength(ADO_CAPABILITIES.length);
  });

  it("groups the checklist and marks every high-risk capability", () => {
    render(<Harness initial={BASE} seen={[]} />);
    expect(screen.getByRole("group", { name: "Contribute" })).toBeInTheDocument();
    const risky = screen.getByRole("group", { name: "High-risk" });
    expect(within(risky).getAllByRole("checkbox")).toHaveLength(ADO_CAPABILITIES.filter((c) => c.highRisk).length);
    expect(within(risky).getAllByText("High risk")).toHaveLength(ADO_CAPABILITIES.filter((c) => c.highRisk).length);
    expect(within(screen.getByRole("group", { name: "Contribute" })).queryByText("High risk")).toBeNull();
  });

  it("reads a non-list value as nothing checked instead of throwing", () => {
    const junk = { ...BASE, azure_devops_capabilities: "read" } as unknown as RunPolicySpec;
    render(<Harness initial={junk} seen={[]} />);
    for (const box of screen.getAllByRole("checkbox")) expect(box).not.toBeChecked();
  });
});
