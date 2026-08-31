/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// AccessPanel — the People step's role-mappings acting surface (0.7 SSO Phase
// 3). Every assertion reads its expected string from lib/people-access-copy.ts
// rather than retyping it, mirroring permissions.test.tsx's own convention —
// these tests fail the moment a rendered string stops coming from the canon.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const getAccessMock = vi.fn();
const upsertMappingMock = vi.fn();
const deleteMappingMock = vi.fn();
const previewRoleMock = vi.fn();
vi.mock("../../../lib/api/access", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/access")>("../../../lib/api/access");
  return {
    ...actual,
    access: {
      getAccess: () => getAccessMock(),
      upsertMapping: (...a: unknown[]) => upsertMappingMock(...a),
      deleteMapping: (...a: unknown[]) => deleteMappingMock(...a),
      previewRole: (...a: unknown[]) => previewRoleMock(...a),
    },
  };
});

import { HttpError } from "../../../lib/api/core";
import { AccessPostureFlipRequiredError } from "../../../lib/api/access";
import { ACCESS_ERROR, ACCESS_STATE, GUARD, PEOPLE, PREVIEW } from "../../../lib/people-access-copy";
import type { AccessResponse } from "../../../lib/types";
import { AccessPanel } from "./access-panel";

function baseAccess(over: Partial<AccessResponse> = {}): AccessResponse {
  return {
    mappings: [
      { value: "Wardyn.Admin", role: "admin", source: "chart", shadowed: false, shadow_cause: "" },
      {
        id: "c1",
        value: "alice@corp.example",
        role: "admin",
        source: "console",
        shadowed: false,
        shadow_cause: "",
        created_at: "2026-08-28T00:00:00Z",
        created_by: "admin",
      },
    ],
    default_role: "member",
    operator_emails_present: true,
    allow_email_mappings: false,
    posture: { map_empty: false, before: "an admin", after: "sign in as a member", changes: true },
    ...over,
  };
}

function renderPanel(access: AccessResponse | null, state: "loading" | "ready" | "sso_unavailable" | "fetch_failed" = "ready", onReload = vi.fn()) {
  render(<AccessPanel access={access} state={state} onReload={onReload} />);
  return onReload;
}

// The withMono() display helper (access-panel.tsx) splits a frozen string
// carrying an env-var name across sibling text nodes / <Mono> spans — no
// single text node holds the whole string, so plain screen.findByText(full
// string) can't match it. document.body.textContent concatenates every
// descendant text node in order with nothing inserted between them (String
// .split with a capturing group never drops or adds characters), so this
// reconstructs the original string reliably.
async function expectEventualBodyText(expected: string): Promise<void> {
  await waitFor(() => expect(document.body.textContent).toContain(expected));
}

beforeEach(() => {
  getAccessMock.mockReset();
  upsertMappingMock.mockReset();
  deleteMappingMock.mockReset();
  previewRoleMock.mockReset();
});

describe("AccessPanel — states", () => {
  it("sso_unavailable renders the distinct SSO-not-configured copy", () => {
    renderPanel(null, "sso_unavailable");
    expect(screen.getByText(ACCESS_STATE.SSO_UNAVAILABLE_TITLE)).toBeInTheDocument();
    expect(screen.getByText(ACCESS_STATE.SSO_UNAVAILABLE_BODY)).toBeInTheDocument();
  });

  it("fetch_failed is distinct from sso_unavailable and offers Retry", async () => {
    const onReload = vi.fn();
    renderPanel(null, "fetch_failed", onReload);
    expect(screen.getByText(ACCESS_STATE.FETCH_FAILED_TITLE)).toBeInTheDocument();
    expect(screen.queryByText(ACCESS_STATE.SSO_UNAVAILABLE_TITLE)).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: ACCESS_STATE.FETCH_FAILED_RETRY }));
    expect(onReload).toHaveBeenCalledTimes(1);
  });

  it("empty console list — chart rows still nowhere (mappings.length===0) shows the empty-table copy", () => {
    renderPanel(baseAccess({ mappings: [] }));
    expect(screen.getByText(PEOPLE.EMPTY_TITLE)).toBeInTheDocument();
    expect(screen.getByText(PEOPLE.EMPTY_BODY)).toBeInTheDocument();
  });
});

describe("AccessPanel — merged table (Variant A)", () => {
  it("renders a chart row read-only with its hint, and a console row with a Delete affordance", () => {
    renderPanel(baseAccess());
    expect(screen.getByText("Wardyn.Admin")).toBeInTheDocument();
    expect(screen.getByText(PEOPLE.CHART_HINT)).toBeInTheDocument();
    expect(screen.getByText("alice@corp.example")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: `${PEOPLE.DELETE} alice@corp.example` })).toBeInTheDocument();
    // Chart row has no delete control.
    expect(screen.queryByRole("button", { name: `${PEOPLE.DELETE} Wardyn.Admin` })).not.toBeInTheDocument();
  });

  it("a shadowed-by-chart console row carries the badge and its body", () => {
    renderPanel(
      baseAccess({
        mappings: [
          { value: "Wardyn.Contractors", role: "member", source: "chart", shadowed: false, shadow_cause: "" },
          {
            id: "c2",
            value: "Wardyn.Contractors",
            role: "member",
            source: "console",
            shadowed: true,
            shadow_cause: "chart",
            created_at: "2026-08-30T00:00:00Z",
          },
        ],
      }),
    );
    expect(screen.getByText(PEOPLE.SHADOWED_BADGE)).toBeInTheDocument();
    expect(screen.getByText(PEOPLE.SHADOWED_BODY)).toBeInTheDocument();
  });

  it("a shadowed-by-operator-allowlist console row carries its own badge and body", () => {
    renderPanel(
      baseAccess({
        mappings: [
          {
            id: "c3",
            value: "carol@corp.example",
            role: "member",
            source: "console",
            shadowed: true,
            shadow_cause: "operator_allowlist",
            created_at: "2026-08-30T00:00:00Z",
          },
        ],
      }),
    );
    expect(screen.getByText(PEOPLE.SHADOWED_OPERATOR_BADGE)).toBeInTheDocument();
  });

  const emailRow = {
    id: "c4",
    value: "bob@corp.example",
    role: "member" as const,
    source: "console" as const,
    shadowed: false,
    shadow_cause: "" as const,
    created_at: "2026-08-30T00:00:00Z",
  };

  it("an email-shaped console row gets NO unverified-claim badge when allow_email_mappings is false", () => {
    renderPanel(baseAccess({ mappings: [emailRow], allow_email_mappings: false }));
    expect(screen.queryByText(PEOPLE.EMAIL_KEY_BADGE)).not.toBeInTheDocument();
  });

  it("an email-shaped console row gets the unverified-claim badge when allow_email_mappings is true", () => {
    renderPanel(baseAccess({ mappings: [emailRow], allow_email_mappings: true }));
    expect(screen.getByText(PEOPLE.EMAIL_KEY_BADGE)).toBeInTheDocument();
  });

  it("Defaults block: a set default role renders as a chip", () => {
    renderPanel(baseAccess({ default_role: "member" }));
    expect(within(screen.getByTestId("access-defaults")).getByText(PEOPLE.ROLE_MEMBER)).toBeInTheDocument();
  });

  it("Defaults block: an unset default role renders DEFAULT_ROLE_UNSET", () => {
    renderPanel(baseAccess({ default_role: "" }));
    expect(screen.getByText(PEOPLE.DEFAULT_ROLE_UNSET)).toBeInTheDocument();
  });

  it("Defaults block: no operator emails renders OPERATOR_EMAILS_EMPTY", () => {
    renderPanel(baseAccess({ operator_emails_present: false }));
    expect(screen.getByText(PEOPLE.OPERATOR_EMAILS_EMPTY)).toBeInTheDocument();
  });
});

describe("AccessPanel — add mapping, posture guard (§2.1/§7.3)", () => {
  it("submits directly when the write would NOT flip posture (map already non-empty)", async () => {
    upsertMappingMock.mockResolvedValue({ mapping: { id: "new", value: "eng-team", role: "member" }, created: true });
    const onReload = vi.fn();
    renderPanel(baseAccess({ posture: { map_empty: false, before: "an admin", after: "sign in as a member", changes: true } }), "ready", onReload);

    await userEvent.type(screen.getByLabelText(PEOPLE.FIELD_VALUE), "eng-team");
    await userEvent.click(screen.getByRole("button", { name: PEOPLE.ADD_CTA }));

    expect(upsertMappingMock).toHaveBeenCalledWith({ value: "eng-team", role: "admin", acknowledge_access_change: undefined });
    expect(onReload).toHaveBeenCalled();
  });

  it("shows the FIRST_ROW ack dialog pre-emptively when the map is empty and the flip changes the outcome — blocks submit until acked", async () => {
    upsertMappingMock.mockResolvedValue({ mapping: { id: "new", value: "eng-team", role: "member" }, created: true });
    renderPanel(baseAccess({ mappings: [], posture: { map_empty: true, before: "an admin", after: "be denied", changes: true } }));

    await userEvent.type(screen.getByLabelText(PEOPLE.FIELD_VALUE), "eng-team");
    await userEvent.click(screen.getByRole("button", { name: PEOPLE.ADD_CTA }));

    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(GUARD.FIRST_ROW_TITLE)).toBeInTheDocument();
    expect(within(dialog).getByText(GUARD.FIRST_ROW_BODY("an admin", "be denied"))).toBeInTheDocument();
    expect(within(dialog).getByText(GUARD.ALLOWLIST_NOTE)).toBeInTheDocument(); // operator_emails_present defaults true in baseAccess
    expect(upsertMappingMock).not.toHaveBeenCalled();

    // GUARD.FIRST_ROW_CONFIRM and PEOPLE.ADD_CTA are BOTH literally "Add
    // mapping" (§7.2/§7.3) — the form's own submit button is still in the DOM
    // behind the dialog, so the confirm button must be found scoped to it.
    const confirm = within(dialog).getByRole("button", { name: GUARD.FIRST_ROW_CONFIRM });
    expect(confirm).toBeDisabled();
    await userEvent.click(within(dialog).getByLabelText(GUARD.GUARD_ACK_LABEL));
    expect(confirm).not.toBeDisabled();
    await userEvent.click(confirm);
    expect(upsertMappingMock).toHaveBeenCalledWith({ value: "eng-team", role: "admin", acknowledge_access_change: true });
  });
});

describe("AccessPanel — add mapping error classification", () => {
  const chartAndConsole = baseAccess({
    mappings: [
      { value: "Wardyn.Admin", role: "admin", source: "chart", shadowed: false, shadow_cause: "" },
    ],
  });

  it("a chart-collision 400 renders COLLISION_ERROR_CHART for a value the chart already maps", async () => {
    upsertMappingMock.mockRejectedValue(
      new HttpError(400, 'value "wardyn.admin" is already set by your chart config (WARDYN_OIDC_ROLE_MAP or WARDYN_OIDC_OPERATOR_EMAILS) and cannot be overridden here'),
    );
    renderPanel(chartAndConsole);
    await userEvent.type(screen.getByLabelText(PEOPLE.FIELD_VALUE), "Wardyn.Admin");
    await userEvent.click(screen.getByRole("button", { name: PEOPLE.ADD_CTA }));
    await expectEventualBodyText(ACCESS_ERROR.COLLISION_ERROR_CHART("Wardyn.Admin"));
  });

  it("the SAME server wording for a value NOT in the chart classifies as an operator-allowlist collision", async () => {
    upsertMappingMock.mockRejectedValue(
      new HttpError(400, 'value "ops@corp.example" is already set by your chart config (WARDYN_OIDC_ROLE_MAP or WARDYN_OIDC_OPERATOR_EMAILS) and cannot be overridden here'),
    );
    renderPanel(chartAndConsole);
    await userEvent.type(screen.getByLabelText(PEOPLE.FIELD_VALUE), "ops@corp.example");
    await userEvent.click(screen.getByRole("button", { name: PEOPLE.ADD_CTA }));
    await expectEventualBodyText(ACCESS_ERROR.COLLISION_ERROR_OPERATOR("ops@corp.example"));
  });

  it("EMAIL_KEY_REFUSED renders verbatim", async () => {
    upsertMappingMock.mockRejectedValue(new HttpError(400, ACCESS_ERROR.EMAIL_KEY_REFUSED));
    renderPanel(chartAndConsole);
    await userEvent.type(screen.getByLabelText(PEOPLE.FIELD_VALUE), "bob@corp.example");
    await userEvent.click(screen.getByRole("button", { name: PEOPLE.ADD_CTA }));
    await expectEventualBodyText(ACCESS_ERROR.EMAIL_KEY_REFUSED);
  });

  it("a lockout 400 renders the frozen LOCKOUT_ERROR, not the server's own terse text", async () => {
    upsertMappingMock.mockRejectedValue(
      new HttpError(400, "this change would remove your own admin access (checked against your last sign-in)"),
    );
    renderPanel(chartAndConsole);
    await userEvent.type(screen.getByLabelText(PEOPLE.FIELD_VALUE), "*");
    await userEvent.click(screen.getByRole("button", { name: PEOPLE.ADD_CTA }));
    expect(await screen.findByText(ACCESS_ERROR.LOCKOUT_ERROR)).toBeInTheDocument();
  });

  it("a raw/unrecognized 400 falls back to the server's own message rather than fabricating a frozen string", async () => {
    upsertMappingMock.mockRejectedValue(new HttpError(400, "value: invalid"));
    renderPanel(chartAndConsole);
    await userEvent.type(screen.getByLabelText(PEOPLE.FIELD_VALUE), "!!!");
    await userEvent.click(screen.getByRole("button", { name: PEOPLE.ADD_CTA }));
    expect(await screen.findByText("value: invalid")).toBeInTheDocument();
  });

  it("a reactive posture-flip 400 (race) re-opens the guard dialog using the error body's before/after", async () => {
    upsertMappingMock.mockRejectedValue(
      new AccessPostureFlipRequiredError(400, {
        error: "…",
        required_acknowledgement: true,
        before: "an admin",
        after: "sign in as a member",
      }),
    );
    // posture.map_empty is FALSE here — the pre-check wouldn't have opened the
    // dialog on its own, proving this came from the reactive path.
    renderPanel(baseAccess({ posture: { map_empty: false, before: "x", after: "y", changes: false } }));
    await userEvent.type(screen.getByLabelText(PEOPLE.FIELD_VALUE), "eng-team");
    await userEvent.click(screen.getByRole("button", { name: PEOPLE.ADD_CTA }));
    expect(await screen.findByText(GUARD.FIRST_ROW_BODY("an admin", "sign in as a member"))).toBeInTheDocument();
  });
});

describe("AccessPanel — delete mapping", () => {
  it("a plain delete (not the last row) shows the plain confirm, no ack required", async () => {
    deleteMappingMock.mockResolvedValue(undefined);
    const onReload = vi.fn();
    renderPanel(chartPlusTwoConsole(), "ready", onReload);

    await userEvent.click(screen.getByRole("button", { name: `${PEOPLE.DELETE} alice@corp.example` }));
    expect(await screen.findByText(`Delete the mapping for "alice@corp.example"?`)).toBeInTheDocument();
    expect(screen.queryByText(GUARD.GUARD_ACK_LABEL)).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /^Delete$/ }));
    expect(deleteMappingMock).toHaveBeenCalledWith("c1", false);
    expect(onReload).toHaveBeenCalled();
  });

  it("deleting the LAST console row (chart empty, posture changes) shows the ack-guarded LAST_ROW dialog", async () => {
    deleteMappingMock.mockResolvedValue(undefined);
    renderPanel(
      baseAccess({
        mappings: [
          {
            id: "only",
            value: "alice@corp.example",
            role: "admin",
            source: "console",
            shadowed: false,
            shadow_cause: "",
            created_at: "2026-08-28T00:00:00Z",
          },
        ],
        posture: { map_empty: false, before: "an admin", after: "sign in as a member", changes: true },
      }),
    );
    await userEvent.click(screen.getByRole("button", { name: `${PEOPLE.DELETE} alice@corp.example` }));
    expect(await screen.findByText(GUARD.LAST_ROW_TITLE)).toBeInTheDocument();
    // Swapped relative to the add direction — LAST_ROW_BODY(posture.after, posture.before).
    expect(screen.getByText(GUARD.LAST_ROW_BODY("sign in as a member", "an admin"))).toBeInTheDocument();

    const confirm = screen.getByRole("button", { name: GUARD.LAST_ROW_CONFIRM });
    expect(confirm).toBeDisabled();
    await userEvent.click(screen.getByLabelText(GUARD.GUARD_ACK_LABEL));
    await userEvent.click(confirm);
    expect(deleteMappingMock).toHaveBeenCalledWith("only", true);
  });

  it("a lockout 400 on delete renders LOCKOUT_ERROR INSIDE the still-open dialog, Delete stays enabled (§2.2 — post-attempt, not a client pre-check)", async () => {
    deleteMappingMock.mockRejectedValue(
      new HttpError(400, "this change would remove your own admin access (checked against your last sign-in)"),
    );
    renderPanel(chartPlusTwoConsole());
    await userEvent.click(screen.getByRole("button", { name: `${PEOPLE.DELETE} alice@corp.example` }));
    const deleteBtn = screen.getByRole("button", { name: /^Delete$/ });
    await userEvent.click(deleteBtn);
    expect(await screen.findByText(ACCESS_ERROR.LOCKOUT_ERROR)).toBeInTheDocument();
    expect(deleteBtn).not.toBeDisabled();
  });
});

function chartPlusTwoConsole(): AccessResponse {
  return baseAccess({
    mappings: [
      { value: "Wardyn.Admin", role: "admin", source: "chart", shadowed: false, shadow_cause: "" },
      {
        id: "c1",
        value: "alice@corp.example",
        role: "admin",
        source: "console",
        shadowed: false,
        shadow_cause: "",
        created_at: "2026-08-28T00:00:00Z",
      },
      {
        id: "c2",
        value: "eng-team",
        role: "member",
        source: "console",
        shadowed: false,
        shadow_cause: "",
        created_at: "2026-08-29T00:00:00Z",
      },
    ],
  });
}

describe("AccessPanel — preview panel", () => {
  it("RESULT_MATCHED for an ok+matched response", async () => {
    previewRoleMock.mockResolvedValue({ role: "admin", ok: true, matched: [{ value: "Wardyn.Admin", role: "admin", source: "chart" }] });
    renderPanel(baseAccess());
    await userEvent.type(screen.getByLabelText(PREVIEW.FIELD_CLAIMS), "Wardyn.Admin");
    await userEvent.click(screen.getByRole("button", { name: PREVIEW.RUN_CTA }));
    expect(await screen.findByText(PREVIEW.RESULT_MATCHED("admin", "Wardyn.Admin"))).toBeInTheDocument();
  });

  it("RESULT_DEFAULT when ok, nothing matched, and the combined map is non-empty", async () => {
    previewRoleMock.mockResolvedValue({ role: "member", ok: true, matched: [] });
    renderPanel(baseAccess({ posture: { map_empty: false, before: "x", after: "y", changes: false } }));
    await userEvent.type(screen.getByLabelText(PREVIEW.FIELD_CLAIMS), "nobody");
    await userEvent.click(screen.getByRole("button", { name: PREVIEW.RUN_CTA }));
    expect(await screen.findByText(PREVIEW.RESULT_DEFAULT("member"))).toBeInTheDocument();
  });

  it("RESULT_LEGACY (distinct from RESULT_DEFAULT) when ok, nothing matched, and the combined map is EMPTY", async () => {
    previewRoleMock.mockResolvedValue({ role: "admin", ok: true, matched: [] });
    renderPanel(baseAccess({ mappings: [], posture: { map_empty: true, before: "x", after: "y", changes: false } }));
    await userEvent.type(screen.getByLabelText(PREVIEW.FIELD_CLAIMS), "nobody");
    await userEvent.click(screen.getByRole("button", { name: PREVIEW.RUN_CTA }));
    expect(await screen.findByText(PREVIEW.RESULT_LEGACY("admin"))).toBeInTheDocument();
  });

  it("RESULT_DENIED when ok=false", async () => {
    previewRoleMock.mockResolvedValue({ role: "", ok: false, matched: [] });
    renderPanel(baseAccess());
    await userEvent.type(screen.getByLabelText(PREVIEW.FIELD_CLAIMS), "nobody");
    await userEvent.click(screen.getByRole("button", { name: PREVIEW.RUN_CTA }));
    expect(await screen.findByText(PREVIEW.RESULT_DENIED)).toBeInTheDocument();
  });

  it("RESULT_UNKNOWN when the response carries an error field", async () => {
    previewRoleMock.mockResolvedValue({ role: "", ok: false, matched: [], error: "role_check_unavailable" });
    renderPanel(baseAccess());
    await userEvent.type(screen.getByLabelText(PREVIEW.FIELD_CLAIMS), "nobody");
    await userEvent.click(screen.getByRole("button", { name: PREVIEW.RUN_CTA }));
    expect(await screen.findByText(PREVIEW.RESULT_UNKNOWN)).toBeInTheDocument();
  });

  it("'test with my own session' runs a use_session preview without requiring the textarea", async () => {
    previewRoleMock.mockResolvedValue({ role: "admin", ok: true, matched: [{ value: "own-session", role: "admin", source: "console" }] });
    renderPanel(baseAccess());
    await userEvent.click(screen.getByRole("button", { name: PREVIEW.OWN_SESSION_CTA }));
    expect(previewRoleMock).toHaveBeenCalledWith({ use_session: true });
    expect(await screen.findByText(PREVIEW.RESULT_MATCHED("admin", "own-session"))).toBeInTheDocument();
  });
});
