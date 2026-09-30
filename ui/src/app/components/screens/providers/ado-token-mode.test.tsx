/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// How people connect to Azure DevOps (#1428): the three choices and what each
// carries, the organisation check's answers, and every refusal an admin can
// meet (mock states 1, 2, 3, 8c, 8d, 10a, 12b).
import * as React from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { GitProvider } from "../../../lib/api/providers";
import type { ADOTokenHealth } from "../../../lib/types/ado-pat";
import { ADO_PAT } from "../../../lib/ado-pat-copy";
import { gitRowInvalid } from "./display";
import { AdoConvertedRow, AdoRowsContext } from "./ado-token-mode";
import { EntraEditor } from "./entra-editor";

const healthMock = vi.fn();
const orgCheckMock = vi.fn();
vi.mock("../../../lib/api/ado-pat", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/ado-pat")>("../../../lib/api/ado-pat");
  return { ...actual, adoPat: { ...actual.adoPat, health: () => healthMock(), orgCheck: () => orgCheckMock() } };
});

const TENANT = "8f14e45f-ceea-4d2c-a3f9-1a2b3c4d5e6f";
const CLIENT = "3b241101-e2bb-4255-8caf-4136c566a962";

const at = (h: number, m: number) => new Date(2026, 8, 29, h, m).toISOString();

function row(entra: Partial<NonNullable<GitProvider["entra"]>> = {}, over: Partial<GitProvider> = {}): GitProvider {
  return {
    id: "ado",
    kind: "azure_devops",
    base_urls: ["https://dev.azure.com/wardyn-live-test"],
    lanes: ["entra"],
    credential_source: "per_user",
    entra: { tenant_id: TENANT, client_id: CLIENT, capability_ceiling: ["read"], default_profile: ["read"], ...entra },
    ...over,
  };
}

function Harness({
  initial,
  operator = true,
  refusal = null,
  onLatest,
}: {
  initial: GitProvider;
  operator?: boolean;
  refusal?: string | null;
  onLatest?: (r: GitProvider) => void;
}) {
  const [r, setR] = React.useState(initial);
  onLatest?.(r);
  return (
    <AdoRowsContext.Provider value={{ refusal, saveBlocked: false }}>
      <EntraEditor row={r} operator={operator} onUpdate={setR} />
    </AdoRowsContext.Provider>
  );
}

beforeEach(() => {
  healthMock.mockReset();
  healthMock.mockResolvedValue({});
  orgCheckMock.mockReset();
});

describe("state 1: how people connect", () => {
  it("draws the three choices in the mock's words, with the chosen one open", async () => {
    render(<Harness initial={row({ token_mode: "minted_pat" })} />);
    const group = screen.getByRole("radiogroup", { name: ADO_PAT.SECTION_TITLE });
    expect(screen.getByRole("heading", { name: ADO_PAT.SECTION_TITLE })).toBeInTheDocument();
    expect(within(group).getByRole("radio", { name: ADO_PAT.MODE_MINTED })).toBeChecked();
    expect(within(group).getByRole("radio", { name: ADO_PAT.MODE_BEARER })).not.toBeChecked();
    expect(within(group).getByRole("radio", { name: ADO_PAT.MODE_OWN })).not.toBeChecked();
    expect(screen.getByText(ADO_PAT.MODE_MINTED_HELP)).toBeInTheDocument();
    expect(screen.getByText(ADO_PAT.MODE_BEARER_HELP)).toBeInTheDocument();
    expect(screen.getByText(ADO_PAT.MODE_OWN_HELP)).toBeInTheDocument();
    expect(screen.getByText(ADO_PAT.RECOMMENDED_SETTINGS)).toBeInTheDocument();
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
  });

  it("the token option carries the setup note, the console-redirect line, the check button and the longest life", async () => {
    render(<Harness initial={row({ token_mode: "minted_pat" })} />);
    await act(async () => {});
    expect(screen.getByText(ADO_PAT.MINTED_SETUP)).toBeInTheDocument();
    // The plan review's F5: register the redirect under Web, then add the secret.
    expect(screen.getByText(ADO_PAT.MINTED_SETUP_REDIRECT)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: ADO_PAT.CHECK_BUTTON })).toBeInTheDocument();
    expect(screen.getByLabelText(ADO_PAT.TOKEN_LIFE_LABEL)).toHaveValue("8");
    expect(screen.getByText(ADO_PAT.TOKEN_LIFE_UNIT)).toBeInTheDocument();
    expect(screen.getByText(ADO_PAT.TOKEN_LIFE_HINT)).toBeInTheDocument();
  });

  it("reads the row's own longest life, and writes a new one to pat_max_hours", async () => {
    let latest: GitProvider | undefined;
    render(<Harness initial={row({ token_mode: "minted_pat", pat_max_hours: 24 })} onLatest={(r) => (latest = r)} />);
    const field = screen.getByLabelText(ADO_PAT.TOKEN_LIFE_LABEL);
    expect(field).toHaveValue("24");
    await userEvent.clear(field);
    await userEvent.type(field, "12");
    expect(latest?.entra?.pat_max_hours).toBe(12);
  });

  it("picking a choice writes token_mode, and the unset default reads as the Entra sign-in", async () => {
    let latest: GitProvider | undefined;
    render(<Harness initial={row()} onLatest={(r) => (latest = r)} />);
    expect(screen.getByRole("radio", { name: ADO_PAT.MODE_BEARER })).toBeChecked();
    await userEvent.click(screen.getByRole("radio", { name: ADO_PAT.MODE_MINTED }));
    expect(latest?.entra?.token_mode).toBe("minted_pat");
    await userEvent.click(screen.getByRole("radio", { name: ADO_PAT.MODE_BEARER }));
    expect(latest?.entra?.token_mode).toBeUndefined();
  });

  it("a member's read-only view can change nothing and offers no check", () => {
    render(<Harness initial={row({ token_mode: "minted_pat" })} operator={false} />);
    expect(screen.getByRole("radio", { name: ADO_PAT.MODE_BEARER })).toBeDisabled();
    expect(screen.queryByRole("button", { name: ADO_PAT.CHECK_BUTTON })).not.toBeInTheDocument();
    expect(healthMock).not.toHaveBeenCalled();
  });

  it("an untouched Entra sign-in row asks the server nothing", () => {
    render(<Harness initial={row({ token_mode: "bearer" })} />);
    expect(healthMock).not.toHaveBeenCalled();
  });
});

describe("state 2: check organisation settings", () => {
  const ok: ADOTokenHealth = { checked_at: at(9, 12), permissions: "granted", lifespan: "on", lifespan_hours: 8 };

  it("runs the check on request and draws both good answers", async () => {
    orgCheckMock.mockResolvedValue(ok);
    render(<Harness initial={row({ token_mode: "minted_pat" })} />);
    await userEvent.click(screen.getByRole("button", { name: ADO_PAT.CHECK_BUTTON }));
    const card = await screen.findByTestId("ado-org-check");
    expect(within(card).getByText(ADO_PAT.CHECK_TITLE)).toBeInTheDocument();
    expect(within(card).getByText(ADO_PAT.CHECK_CHIP("09:12"))).toBeInTheDocument();
    expect(within(card).getByText(ADO_PAT.CHECK_PERMS_OK)).toBeInTheDocument();
    expect(within(card).getByText(ADO_PAT.CHECK_LIFESPAN_ON(8))).toBeInTheDocument();
    expect(screen.getByText(ADO_PAT.CHECK_LAST("09:12"))).toBeInTheDocument();
    expect(orgCheckMock).toHaveBeenCalledTimes(1);
  });

  it("draws the last answer without a click, and both warnings when permissions are missing and the lifespan limit is off", async () => {
    healthMock.mockResolvedValue({ checked_at: at(9, 12), permissions: "missing", lifespan: "off" });
    render(<Harness initial={row({ token_mode: "minted_pat" })} />);
    const card = await screen.findByTestId("ado-org-check");
    expect(within(card).getByText(ADO_PAT.CHECK_PERMS_MISSING)).toBeInTheDocument();
    expect(within(card).getByText(ADO_PAT.CHECK_LIFESPAN_OFF)).toBeInTheDocument();
    expect(within(card).queryByText(ADO_PAT.CHECK_PERMS_OK)).not.toBeInTheDocument();
  });

  it("says nothing before any check has run", async () => {
    render(<Harness initial={row({ token_mode: "minted_pat" })} />);
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(screen.queryByTestId("ado-org-check")).not.toBeInTheDocument();
    expect(screen.queryByText(/Last checked/)).not.toBeInTheDocument();
  });
});

describe("state 3: setup errors", () => {
  it("a longest life outside 1 to 168 shows the range and withholds Save", async () => {
    let latest: GitProvider | undefined;
    render(<Harness initial={row({ token_mode: "minted_pat" })} onLatest={(r) => (latest = r)} />);
    const field = screen.getByLabelText(ADO_PAT.TOKEN_LIFE_LABEL);
    await userEvent.clear(field);
    await userEvent.type(field, "400");
    expect(screen.getByText(ADO_PAT.TOKEN_LIFE_RANGE)).toBeInTheDocument();
    expect(screen.queryByText(ADO_PAT.TOKEN_LIFE_HINT)).not.toBeInTheDocument();
    expect(field).toHaveAttribute("aria-invalid", "true");
    expect(gitRowInvalid(latest!)).toBe(true);
    await userEvent.clear(field);
    await userEvent.type(field, "168");
    expect(screen.queryByText(ADO_PAT.TOKEN_LIFE_RANGE)).not.toBeInTheDocument();
    expect(gitRowInvalid(latest!)).toBe(false);
  });

  it("a Save refused for want of a client secret says so under the token option", async () => {
    render(<Harness initial={row({ token_mode: "minted_pat" })} refusal={ADO_PAT.NO_CLIENT_SECRET} />);
    await act(async () => {});
    expect(screen.getByText(ADO_PAT.NO_CLIENT_SECRET)).toBeInTheDocument();
  });

  it("the client-secret refusal is not drawn on another choice", () => {
    render(<Harness initial={row({ token_mode: "own_pat" })} refusal={ADO_PAT.NO_CLIENT_SECRET} />);
    expect(screen.queryByText(ADO_PAT.NO_CLIENT_SECRET)).not.toBeInTheDocument();
  });

  it("a lifespan the organisation refused reads with the longest life it accepted", async () => {
    healthMock.mockResolvedValue({ checked_at: at(9, 12), permissions: "granted", lifespan: "too_long", lifespan_hours: 24 });
    render(<Harness initial={row({ token_mode: "minted_pat" })} />);
    expect(await screen.findByText(ADO_PAT.LIFESPAN_REFUSAL(24))).toBeInTheDocument();
    expect(screen.getByText("Longest token life is above your organisation's maximum token lifespan. Lower it to 24 hours or less.")).toBeInTheDocument();
  });

  it("and reads 'Lower it.' when it does not know one", async () => {
    healthMock.mockResolvedValue({ checked_at: at(9, 12), lifespan: "too_long" });
    render(<Harness initial={row({ token_mode: "minted_pat" })} />);
    expect(await screen.findByText("Longest token life is above your organisation's maximum token lifespan. Lower it.")).toBeInTheDocument();
  });
});

describe("state 8: the organisation blocks token creation", () => {
  it("8c: the banner names the person and offers Switch to Entra sign-in", async () => {
    healthMock.mockResolvedValue({ blocked_person: "Priya Shah" });
    let latest: GitProvider | undefined;
    render(<Harness initial={row({ token_mode: "minted_pat" })} onLatest={(r) => (latest = r)} />);
    expect(await screen.findByText(ADO_PAT.POLICY_BANNER("Priya Shah"))).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: ADO_PAT.POLICY_BANNER_BUTTON }));
    expect(latest?.entra?.token_mode).toBeUndefined();
    expect(screen.getByRole("radio", { name: ADO_PAT.MODE_BEARER })).toBeChecked();
    // Already on the Entra sign-in: nothing left to switch to.
    expect(screen.queryByRole("button", { name: ADO_PAT.POLICY_BANNER_BUTTON })).not.toBeInTheDocument();
  });

  it("8d: choosing the Entra sign-in while the app holds the token permissions is refused inline", async () => {
    healthMock.mockResolvedValue({ checked_at: at(9, 12), permissions: "granted", lifespan: "on", lifespan_hours: 8 });
    render(<Harness initial={row({ token_mode: "minted_pat" })} />);
    await screen.findByTestId("ado-org-check");
    expect(screen.queryByText(ADO_PAT.BEARER_WITH_TOKEN_PERMS)).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("radio", { name: ADO_PAT.MODE_BEARER }));
    expect(screen.getByText(ADO_PAT.BEARER_WITH_TOKEN_PERMS)).toBeInTheDocument();
  });

  it("8d: and so is a Save the server refused for the same reason", () => {
    render(<Harness initial={row({ token_mode: "bearer" })} refusal={ADO_PAT.BEARER_WITH_TOKEN_PERMS} />);
    expect(screen.getByText(ADO_PAT.BEARER_WITH_TOKEN_PERMS)).toBeInTheDocument();
  });
});

describe("state 10a: each person adds their own token", () => {
  it("shows the longest expiry with its range, and no tenant or client to fill", () => {
    render(<Harness initial={row({ token_mode: "own_pat", tenant_id: "", client_id: "" })} />);
    expect(screen.getByRole("radio", { name: ADO_PAT.MODE_OWN })).toBeChecked();
    expect(screen.getByLabelText(ADO_PAT.OWN_EXPIRY_LABEL)).toHaveValue("30");
    expect(screen.getByText(ADO_PAT.OWN_EXPIRY_UNIT)).toBeInTheDocument();
    expect(screen.getByText(ADO_PAT.OWN_EXPIRY_RANGE)).toBeInTheDocument();
    expect(screen.queryByLabelText("Directory (tenant) ID")).not.toBeInTheDocument();
    expect(screen.queryByText(ADO_PAT.MINTED_SETUP)).not.toBeInTheDocument();
    expect(healthMock).not.toHaveBeenCalled();
  });

  it("writes pat_max_days, and an expiry past 90 days shows the range and withholds Save", async () => {
    let latest: GitProvider | undefined;
    render(<Harness initial={row({ token_mode: "own_pat" })} onLatest={(r) => (latest = r)} />);
    const field = screen.getByLabelText(ADO_PAT.OWN_EXPIRY_LABEL);
    await userEvent.clear(field);
    await userEvent.type(field, "45");
    expect(latest?.entra?.pat_max_days).toBe(45);
    await userEvent.clear(field);
    await userEvent.type(field, "91");
    expect(gitRowInvalid(latest!)).toBe(true);
    expect(screen.getByText(ADO_PAT.OWN_EXPIRY_RANGE)).toHaveClass("text-danger");
  });
});

describe("state 12b: the row the upgrade switched off", () => {
  const converted = (): GitProvider =>
    row(
      { token_mode: "own_pat", tenant_id: "", client_id: "", capability_ceiling: ["read"], default_profile: [] },
      { disabled: true },
    );

  it("says why it is off, offers the choice, and names what it may ever do", () => {
    render(<AdoConvertedRow row={converted()} operator onUpdate={() => {}} />);
    expect(screen.getByText(ADO_PAT.CONVERTED_NOTE)).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: ADO_PAT.MODE_OWN })).toBeChecked();
    expect(screen.getByText(/^Read-only, carried over from the retired token: .+\.$/)).toBeInTheDocument();
  });

  it("Save and turn on asks the screen to save this row switched on", async () => {
    const saveAndEnable = vi.fn();
    render(
      <AdoRowsContext.Provider value={{ refusal: null, saveAndEnable, saveBlocked: false }}>
        <AdoConvertedRow row={converted()} operator onUpdate={() => {}} />
      </AdoRowsContext.Provider>,
    );
    await userEvent.click(screen.getByRole("button", { name: ADO_PAT.CONVERTED_SAVE_ON }));
    expect(saveAndEnable).toHaveBeenCalledWith("ado");
  });

  it("withholds Save and turn on while the screen cannot save", () => {
    render(
      <AdoRowsContext.Provider value={{ refusal: null, saveAndEnable: vi.fn(), saveBlocked: true }}>
        <AdoConvertedRow row={converted()} operator onUpdate={() => {}} />
      </AdoRowsContext.Provider>,
    );
    expect(screen.getByRole("button", { name: ADO_PAT.CONVERTED_SAVE_ON })).toBeDisabled();
  });

  it("a git-only Server row has the note and the button but no sign-in choice", () => {
    const server: GitProvider = { id: "ado", kind: "azure_devops", base_urls: ["https://tfs.example.com/c"], lanes: ["pat"], credential_source: "per_user", disabled: true };
    render(<AdoConvertedRow row={server} operator onUpdate={() => {}} />);
    expect(screen.getByText(ADO_PAT.CONVERTED_NOTE)).toBeInTheDocument();
    expect(screen.queryByRole("radiogroup")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: ADO_PAT.CONVERTED_SAVE_ON })).toBeInTheDocument();
  });
});
