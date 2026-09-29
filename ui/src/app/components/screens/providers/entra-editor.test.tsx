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
import { ADO_CAP_COPY, ADO_ENTRA_EDITOR as E } from "../../../lib/workspace-providers-copy";
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
      capability_ceiling: ["code_write", "policy_admin", "pr", "read"],
      default_profile: ["code_write", "pr", "read"],
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

  it("an empty default profile reads as Read, the server's own reading", () => {
    render(<Harness initial={entraRow({ default_profile: [] })} />);
    expect(defaults().getByRole("checkbox", { name: name("read") })).toBeChecked();
    expect(defaults().getByRole("checkbox", { name: name("pr") })).not.toBeChecked();
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
    render(<Harness initial={entraRow({ default_profile: ["policy_admin", "read"] })} onLatest={(r) => (latest = r)} />);
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
        capability_ceiling: ["code_write", "policy_admin", "pr", "read", "work_write"],
        default_profile: ["code_write", "read", "work_write"],
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
