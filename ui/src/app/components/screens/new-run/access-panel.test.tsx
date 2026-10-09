/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { ComponentFact } from "../../../lib/types/components";
import { ACCESS_ROWS as T } from "../../wardyn/copy/components";
import { AccessPanel, type AccessPanelProps } from "./access-panel";
import { accessRows } from "./access-rows-model";

const user = userEvent.setup({ pointerEventsCheck: 0 });

const custom = (over: Partial<ComponentFact> = {}): ComponentFact => ({
  kind: "custom", id: "c1", name: "Billing API", reason: "self", status: "ready", requirements: [], ...over,
});
const needsInput = custom({
  id: "c2", name: "Report tool", status: "needs_input", self_defined: true,
  requirements: [{ id: "secret:tok", kind: "secret", label: "Secret: tok", required_by: "an env_secret grant (TOK)", status: "missing", fix: { action: "add_secret", secret_name: "tok" } }],
  hosts: ["reports.example"], secrets: [{ delivery: "env", shared: false }],
});
const git = (over: Partial<ComponentFact> = {}): ComponentFact => ({
  kind: "git_provider", provider: "azure_devops", id: "git_provider:azure_devops:entra", reason: "workspace", status: "needs_input", requirements: [], lane: "entra", org: "https://dev.azure.com/contoso", ...over,
});

function Harness({ facts, guardLink = () => () => {}, initialOpen }: { facts: ComponentFact[]; guardLink?: AccessPanelProps["guardLink"]; initialOpen?: string }) {
  const [open, setOpen] = React.useState<string | undefined>(initialOpen);
  return (
    <MemoryRouter>
      <AccessPanel
        adoAccess={undefined}
        savedMode={false}
        savedCaps={undefined}
        adoRefusal={undefined}
        adoConnecting={false}
        onAdoConnect={() => {}}
        rows={accessRows(facts, undefined)}
        openRowId={open}
        onOpenRow={setOpen}
        secretsPath="/secrets"
        guardLink={guardLink}
      />
    </MemoryRouter>
  );
}

const toggle = (name: RegExp) => screen.getByRole("button", { name });

describe("Access panel rows", () => {
  it("renders no list when the run carries no component", () => {
    render(<Harness facts={[]} />);
    expect(screen.queryByRole("list", { name: T.LIST_LABEL })).toBeNull();
  });

  it("renders one row per fact, each with its name, status and reason on one line", () => {
    render(<Harness facts={[custom(), git()]} />);
    const rows = within(screen.getByRole("list", { name: T.LIST_LABEL })).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(within(rows[0]).getByText("Billing API")).toBeInTheDocument();
    expect(within(rows[0]).getByText(T.STATUS.ready)).toBeInTheDocument();
    expect(within(rows[0]).getByText(T.REASON.self)).toBeInTheDocument();
    expect(within(rows[1]).getByText(T.TITLE.azure_devops)).toBeInTheDocument();
  });

  it("opens one row at a time", async () => {
    render(<Harness facts={[custom({ hosts: ["billing.example"] }), custom({ id: "c3", name: "Other", hosts: ["other.example"] })]} />);
    expect(screen.queryByText("billing.example")).toBeNull();
    await user.click(toggle(/^Billing API/));
    expect(screen.getByText("billing.example")).toBeInTheDocument();
    expect(toggle(/^Billing API/)).toHaveAttribute("aria-expanded", "true");
    await user.click(toggle(/^Other/));
    expect(screen.queryByText("billing.example")).toBeNull();
    expect(screen.getByText("other.example")).toBeInTheDocument();
    expect(toggle(/^Billing API/)).toHaveAttribute("aria-expanded", "false");
    await user.click(toggle(/^Other/));
    expect(screen.queryByText("other.example")).toBeNull();
  });

  it("marks a custom row that needs input as an issue: error tone, aria-invalid, its sentence", () => {
    render(<Harness facts={[needsInput]} />);
    const button = toggle(/^Report tool/);
    expect(button).toHaveAttribute("aria-invalid", "true");
    const sentence = screen.getByText(T.ISSUE_NEEDS_INPUT("Report tool"));
    expect(sentence).toHaveClass("text-danger");
    expect(button.getAttribute("aria-describedby")).toBe(sentence.id);
    expect(button.closest("li")).toHaveClass("border-danger");
  });

  it("leaves a Git provider that needs input as a warning: no aria-invalid, no error sentence", () => {
    render(<Harness facts={[git()]} />);
    const button = toggle(/^Azure DevOps/);
    expect(button).not.toHaveAttribute("aria-invalid");
    expect(button).not.toHaveAttribute("aria-describedby");
    expect(screen.getByText(T.STATUS.needs_input)).toBeInTheDocument();
    expect(button.closest("li")).not.toHaveClass("border-danger");
  });

  it("prints the disclosures under the row without opening it, and the cap sentence only when set", () => {
    const { rerender } = render(<Harness facts={[needsInput]} />);
    expect(screen.getByText(T.DISCLOSURE.SELF_DEFINED)).toBeInTheDocument();
    expect(screen.getByText(T.DISCLOSURE.RESIDENT)).toBeInTheDocument();
    expect(screen.queryByText(T.DISCLOSURE.CAP_L1)).toBeNull();
    rerender(<Harness facts={[{ ...needsInput, autonomy_cap: "L1" }]} />);
    expect(screen.getByText(T.DISCLOSURE.CAP_L1)).toBeInTheDocument();
  });

  it("shows the open row's hosts, secrets and what is still needed", () => {
    render(<Harness facts={[needsInput]} initialOpen="c2" />);
    expect(screen.getByText("reports.example")).toBeInTheDocument();
    expect(screen.getByText(/Set as an environment variable\. Yours\./)).toBeInTheDocument();
    expect(screen.getByText("Secret: tok")).toBeInTheDocument();
  });

  it("links an own missing secret to the Secrets page through the unsaved-changes guard", async () => {
    const guard = vi.fn().mockReturnValue((e: React.MouseEvent) => e.preventDefault());
    render(<Harness facts={[needsInput]} initialOpen="c2" guardLink={guard} />);
    const link = screen.getByRole("link", { name: T.BODY.ADD_SECRET });
    expect(link).toHaveAttribute("href", "/secrets");
    await user.click(link);
    expect(guard).toHaveBeenCalledWith("/secrets");
  });

  it("offers no link for a requirement with no fix, and none for a shared secret (which has no name to show)", () => {
    const shared = custom({
      status: "unavailable", secrets: [{ delivery: "header", shared: true }],
    });
    render(<Harness facts={[shared]} initialOpen="c1" />);
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.getByText(/Sent as a header\. Provided by your admin\./)).toBeInTheDocument();
    expect(screen.getByText(T.UNAVAILABLE.custom)).toBeInTheDocument();
  });

  it("shows a Git provider's lane, organisation and repositories when opened", () => {
    render(<Harness facts={[git({ repos: ["https://dev.azure.com/contoso/p/_git/r"] })]} initialOpen="git_provider:azure_devops:entra" />);
    expect(screen.getByText(T.LANE.entra)).toBeInTheDocument();
    expect(screen.getByText("https://dev.azure.com/contoso")).toBeInTheDocument();
    expect(screen.getByText("https://dev.azure.com/contoso/p/_git/r")).toBeInTheDocument();
  });

  it("gives each row the DOM id an issue link focuses", () => {
    render(<Harness facts={[needsInput]} />);
    expect(document.getElementById("nr-access-row-c2")).toBe(toggle(/^Report tool/).closest("li"));
  });
});
