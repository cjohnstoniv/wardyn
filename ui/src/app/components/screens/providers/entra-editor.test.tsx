/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Azure DevOps row's Entra section: the ceiling bounds the default, a
// default off the ceiling is locked, a default left outside a narrowed ceiling
// blocks Save with the banner, and every edit lands on the row's entra block.
import * as React from "react";
import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { GitProvider } from "../../../lib/api/providers";
import { ADO_CAP_COPY, ADO_ENTRA_EDITOR as E, ADO_GROUP_COPY } from "../../../lib/workspace-providers-copy";
import { gitRowInvalid } from "./display";
import { EntraEditor } from "./entra-editor";
import { GitTab } from "./git-tab";

const TENANT = "8f14e45f-ceea-4d2c-a3f9-1a2b3c4d5e6f";
const CLIENT = "3b241101-e2bb-4255-8caf-4136c566a962";

function entraRow(entra: Partial<NonNullable<GitProvider["entra"]>> = {}): GitProvider {
  return {
    id: "ado",
    kind: "azure_devops",
    base_urls: ["https://dev.azure.com/wardyn-live-test"],
    lanes: ["entra"],
    credential_source: "per_user",
    entra: {
      tenant_id: TENANT,
      client_id: CLIENT,
      capability_ceiling: ["code_write", "policy_admin", "pr", "code_read", "project_read"],
      default_profile: ["code_write", "pr", "code_read"],
      token_mode: "bearer",
      ...entra,
    },
  };
}

function Harness({ initial, operator = true, onLatest }: { initial: GitProvider; operator?: boolean; onLatest?: (r: GitProvider) => void }) {
  const [row, setRow] = React.useState(initial);
  onLatest?.(row);
  return <EntraEditor row={row} operator={operator} onUpdate={setRow} />;
}

const ceiling = () => within(screen.getByRole("group", { name: E.CEILING_TITLE }));
const defaults = () => within(screen.getByRole("group", { name: E.DEFAULT_TITLE }));
const name = (cap: string) => ADO_CAP_COPY[cap].name;

describe("EntraEditor", () => {
  it("draws the ceiling and the default from the row", () => {
    render(<Harness initial={entraRow()} />);
    expect(screen.getByLabelText(E.FIELD_TENANT)).toHaveValue(TENANT);
    expect(screen.getByLabelText(E.FIELD_CLIENT)).toHaveValue(CLIENT);
    expect(screen.getByRole("switch", { name: E.REST_TOGGLE })).toHaveAttribute("aria-checked", "true");
    expect(ceiling().getByRole("checkbox", { name: name("policy_admin") })).toBeChecked();
    expect(ceiling().getByRole("checkbox", { name: name("work_write") })).not.toBeChecked();
    expect(defaults().getByRole("checkbox", { name: name("pr") })).toBeChecked();
    expect(defaults().getByRole("checkbox", { name: name("policy_admin") })).not.toBeChecked();
    expect(screen.getByText(E.HIGH_RISK_WARN)).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("draws the seven areas in the per-area mock's order, High-risk rows inside their area", () => {
    render(<Harness initial={entraRow()} />);
    const names = ceiling().getAllByRole("checkbox").map((b) => b.getAttribute("aria-label"));
    expect(names.slice(0, 9)).toEqual(
      ["code_read", "code_write", "pr", "policy_admin", "policy_bypass", "repo_admin", "work_read", "work_write", "work_admin"].map(name),
    );
    expect(names).toHaveLength(28);
    expect(screen.getByTestId("entra-ceiling-policy_admin")).toHaveClass("border-l-danger");
    expect(screen.getByTestId("entra-ceiling-code_read")).not.toHaveClass("border-l-danger");
  });

  it("shows each row's Azure DevOps line, the second ceiling lead and the High-risk legend", () => {
    render(<Harness initial={entraRow()} />);
    for (const cap of ["code_read", "work_admin", "library_read"]) {
      expect(screen.getByTestId(`entra-ceiling-${cap}-ado`)).toHaveTextContent(ADO_CAP_COPY[cap].ado);
    }
    expect(screen.getByText(E.CEILING_LEAD_ADO)).toBeInTheDocument();
    expect(within(screen.getByText(E.HIGH_RISK_WARN).closest("p")!).getByText(E.HIGH_RISK_BADGE)).toBeInTheDocument();
    expect(screen.getByText(E.DEFAULT_EMPTY_HINT)).toBeInTheDocument();
    // The default section draws names only.
    expect(screen.queryByTestId("entra-default-code_read-ado")).not.toBeInTheDocument();
  });

  // cn() (tailwind-merge) drops text-meta/text-body when a text colour joins
  // them; both the size and the colour must reach the DOM.
  it("keeps both the size and the colour class on names and leads", () => {
    render(<Harness initial={entraRow()} />);
    expect(screen.getAllByText(ADO_GROUP_COPY.repos.name)[0]).toHaveClass("text-body");
    expect(screen.getByText(ADO_GROUP_COPY.repos.lead)).toHaveClass("text-meta", "text-muted-foreground");
    expect(screen.getByText(E.HIGH_RISK_WARN).closest("p")).toHaveClass("text-meta", "text-danger");
    expect(ceiling().getByText(name("pr"))).toHaveClass("text-body", "text-foreground");
    expect(ceiling().getByText(name("work_write"))).toHaveClass("text-body", "text-muted-foreground");
  });

  it("an empty default profile reads as Read code and View projects & teams, the server's own reading", () => {
    render(<Harness initial={entraRow({ default_profile: [] })} />);
    expect(defaults().getByRole("checkbox", { name: name("code_read") })).toBeChecked();
    expect(defaults().getByRole("checkbox", { name: name("project_read") })).toBeChecked();
    expect(defaults().getByRole("checkbox", { name: name("pr") })).not.toBeChecked();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("a default off the ceiling is disabled and locked, with its reason in the tooltip", () => {
    render(<Harness initial={entraRow()} />);
    const box = defaults().getByRole("checkbox", { name: name("policy_bypass") });
    expect(box).toBeDisabled();
    expect(screen.getByTestId("entra-default-policy_bypass")).toHaveAttribute("title", E.DEFAULT_OFF_CEILING_TIP);
    // On the ceiling but High risk: enabled, with the never-defaulted tooltip.
    expect(defaults().getByRole("checkbox", { name: name("policy_admin") })).toBeEnabled();
    expect(screen.getByTestId("entra-default-policy_admin")).toHaveAttribute("title", E.DEFAULT_HIGH_RISK_TIP);
  });

  it("putting a capability on the ceiling enables its default box", async () => {
    render(<Harness initial={entraRow()} />);
    expect(defaults().getByRole("checkbox", { name: name("work_write") })).toBeDisabled();
    await userEvent.click(ceiling().getByRole("checkbox", { name: name("work_write") }));
    expect(defaults().getByRole("checkbox", { name: name("work_write") })).toBeEnabled();
    expect(screen.getByTestId("entra-default-work_write")).not.toHaveAttribute("title");
  });

  it("narrowing the ceiling under a default shows the banner, and the row cannot be saved", async () => {
    let latest: GitProvider | undefined;
    render(<Harness initial={entraRow({ default_profile: ["policy_admin", "code_read"] })} onLatest={(r) => (latest = r)} />);
    expect(gitRowInvalid(latest!)).toBe(false);

    await userEvent.click(ceiling().getByRole("checkbox", { name: name("policy_admin") }));
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent(E.ERROR_TITLE);
    expect(alert).toHaveTextContent(E.ERROR_DEFAULT_OFF_CEILING(name("policy_admin")));
    expect(gitRowInvalid(latest!)).toBe(true);

    // The flagged default stays uncheckable, so the admin can clear it.
    const box = defaults().getByRole("checkbox", { name: name("policy_admin") });
    expect(box).toBeEnabled();
    await userEvent.click(box);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(gitRowInvalid(latest!)).toBe(false);
  });

  it("edits land on the row's entra block only, in catalogue order", async () => {
    let latest: GitProvider | undefined;
    render(<Harness initial={entraRow()} onLatest={(r) => (latest = r)} />);
    await userEvent.click(ceiling().getByRole("checkbox", { name: name("work_write") }));
    await userEvent.click(defaults().getByRole("checkbox", { name: name("work_write") }));
    await userEvent.click(defaults().getByRole("checkbox", { name: name("pr") }));
    await userEvent.click(screen.getByRole("switch", { name: E.REST_TOGGLE }));
    const tenant = screen.getByLabelText(E.FIELD_TENANT);
    await userEvent.clear(tenant);
    await userEvent.type(tenant, CLIENT);

    expect(latest).toEqual({
      id: "ado",
      kind: "azure_devops",
      base_urls: ["https://dev.azure.com/wardyn-live-test"],
      lanes: ["entra"],
      credential_source: "per_user",
      entra: {
        tenant_id: CLIENT,
        client_id: CLIENT,
        capability_ceiling: ["code_read", "code_write", "pr", "policy_admin", "work_write", "project_read"],
        default_profile: ["code_read", "code_write", "work_write"],
        token_mode: "bearer",
        rest_api: false,
      },
    });
  });

  it("is read-only for a caller who can't edit", () => {
    render(<Harness initial={entraRow()} operator={false} />);
    expect(screen.getByText(E.READ_ONLY_NOTE)).toBeInTheDocument();
    expect(screen.getByLabelText(E.FIELD_TENANT)).toBeDisabled();
    expect(screen.getByLabelText(E.FIELD_CLIENT)).toBeDisabled();
    expect(screen.getByRole("switch", { name: E.REST_TOGGLE })).toBeDisabled();
    for (const box of screen.getAllByRole("checkbox")) expect(box).toBeDisabled();
    expect(screen.queryByText(E.CEILING_LEAD)).not.toBeInTheDocument();
    // The Azure DevOps lines stay, so a person who can't edit still sees what each row means there.
    expect(screen.getByTestId("entra-ceiling-code_read-ado")).toHaveTextContent(ADO_CAP_COPY.code_read.ado);
  });
});

describe("GitTab — the Entra section", () => {
  const tab = (git: GitProvider[]) =>
    render(<GitTab git={git} onChange={() => {}} present={[]} githubApp={false} operator onStatusRefresh={() => {}} />);

  it("renders on an Azure DevOps row that carries the entra lane", () => {
    tab([entraRow()]);
    expect(within(screen.getByTestId("provider-row-azure_devops")).getByTestId("entra-editor")).toBeInTheDocument();
  });

  it("is absent on an Azure DevOps row without it", () => {
    tab([{ id: "ado", kind: "azure_devops", base_urls: ["https://dev.azure.com/acme"] }]);
    expect(screen.queryByTestId("entra-editor")).not.toBeInTheDocument();
  });
});
