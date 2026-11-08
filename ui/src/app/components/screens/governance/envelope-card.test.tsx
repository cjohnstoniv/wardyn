/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The platform envelope card and the two chips in the Profiles table. A security operator sets, edits and
// removes an envelope; anyone else reads their platform's envelope and what applies inside it. Expected
// strings come from ENVELOPE, so these fail the moment a rendered string stops coming from the canon.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

const setEnvelopeMock = vi.fn();
const clearEnvelopeMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return {
    ...actual,
    governance: {
      ...actual.governance,
      setEnvelope: (...a: unknown[]) => setEnvelopeMock(...a),
      clearEnvelope: (...a: unknown[]) => clearEnvelopeMock(...a),
    },
  };
});

import { HttpError, PendingChangeError } from "../../../lib/api/core";
import type { GovernanceSnapshot, GovernanceUnit } from "../../../lib/api/governance";
import type { GovernanceEnvelope } from "../../../lib/types/governance-envelope";
import { CHANGES } from "../../../lib/governance-copy";
import { POOL_DRAWER } from "../../wardyn/copy/runner-pools";
import { ENVELOPE } from "../../wardyn/copy/envelope";
import { OperatorProvider } from "../../wardyn/operator-context";
import { absoluteTime } from "../../../lib/format";
import { aheadByHours } from "../../../lib/test-clock";
import { EnvelopeCard, EnvelopeChip } from "./envelope-card";

function unit(id: string, name: string, parent_id?: string): GovernanceUnit {
  return {
    id,
    name,
    ...(parent_id ? { parent_id } : {}),
    ceiling: { allowed_domains: [], first_use_approval: "deny_with_review", min_confinement_class: "CC1" },
    limits: {},
    created_at: "",
    updated_at: "",
  };
}

// A is a root with B and C below it (C two levels down); D is a root of its own; E sits outside any envelope.
const A = unit("a", "Platform A");
const B = unit("b", "Platform A / build", "a");
const C = unit("c", "Platform A / build / ci", "b");
const D = unit("d", "Platform D");
const E = unit("e", "Sandbox lab");
const PROFILES = [A, B, C, D, E];

const SET_AT = aheadByHours(-72);
const ENV_A: GovernanceEnvelope = { unit_id: "a", approved_groups: ["plat-a-users", "plat-a-builders"], set_by: "morgan@example.com", set_at: SET_AT };
const ENV_D: GovernanceEnvelope = { unit_id: "d", approved_groups: [], set_by: "morgan@example.com", set_at: SET_AT };

function snap(envelopes: GovernanceEnvelope[] | undefined, extra: Partial<GovernanceSnapshot> = {}): GovernanceSnapshot {
  return { profiles: PROFILES, assignments: [], envelopes, unit_names: Object.fromEntries(PROFILES.map((p) => [p.id, p.name])), ...extra };
}

function renderCard(s: GovernanceSnapshot, { security = true, principal = "morgan@example.com", onChanged = vi.fn() } = {}) {
  render(
    <MemoryRouter>
      <OperatorProvider operator={security} securityOperator={security} principal={principal}>
        <EnvelopeCard snapshot={s} securityOperator={security} onChanged={onChanged} />
      </OperatorProvider>
    </MemoryRouter>,
  );
  return { onChanged };
}

beforeEach(() => {
  setEnvelopeMock.mockReset();
  clearEnvelopeMock.mockReset();
});

describe("EnvelopeChip", () => {
  const chip = (p: GovernanceUnit, envelopes: GovernanceEnvelope[], profiles = PROFILES) =>
    render(<EnvelopeChip profile={p} profiles={profiles} envelopes={envelopes} unitNames={{ a: "Platform A" }} />);

  it("marks an envelope root", () => {
    chip(A, [ENV_A]);
    expect(screen.getByText(ENVELOPE.CHIP_ROOT)).toBeInTheDocument();
  });

  it("marks a profile two levels below the root with the root's name", () => {
    chip(C, [ENV_A]);
    expect(screen.getByText(ENVELOPE.CHIP_IN("Platform A"))).toBeInTheDocument();
    expect(screen.queryByText(ENVELOPE.CHIP_ROOT)).toBeNull();
  });

  it("names a root the list does not hold from the unit names", () => {
    chip(B, [ENV_A], [B, C]);
    expect(screen.getByText(ENVELOPE.CHIP_IN("Platform A"))).toBeInTheDocument();
  });

  it("draws nothing outside an envelope", () => {
    const { container } = chip(E, [ENV_A]);
    expect(container).toBeEmptyDOMElement();
  });

  it("draws nothing when the server sent no envelopes", () => {
    const { container } = chip(B, []);
    expect(container).toBeEmptyDOMElement();
  });
});

describe("the security card", () => {
  it("draws nothing when the snapshot carries no envelopes field", () => {
    renderCard(snap(undefined));
    expect(screen.queryByText(ENVELOPE.TITLE)).toBeNull();
  });

  it("lists one row per envelope with the groups, or the no-groups text", () => {
    renderCard(snap([ENV_A, ENV_D]));
    expect(screen.getByRole("heading", { name: ENVELOPE.TITLE })).toBeInTheDocument();
    expect(screen.getByText(ENVELOPE.LEAD)).toBeInTheDocument();
    expect(screen.getByText(ENVELOPE.HELP)).toBeInTheDocument();
    for (const h of [ENVELOPE.COL_PLATFORM, ENVELOPE.COL_GROUPS, ENVELOPE.COL_SET_BY]) {
      expect(screen.getByRole("columnheader", { name: h })).toBeInTheDocument();
    }
    const rowA = screen.getByRole("row", { name: /Platform A\b(?! \/)/ });
    expect(within(rowA).getByText("plat-a-users")).toBeInTheDocument();
    expect(within(rowA).getByText("plat-a-builders")).toBeInTheDocument();
    const rowD = screen.getByRole("row", { name: /Platform D/ });
    expect(within(rowD).getByText(ENVELOPE.NO_GROUPS)).toBeInTheDocument();
  });

  it("shows Set by over its time, the absolute time in the title", () => {
    renderCard(snap([ENV_A]));
    const row = screen.getByRole("row", { name: /plat-a-users/ });
    expect(within(row).getByText("morgan@example.com")).toBeInTheDocument();
    expect(within(row).getByText("3d ago")).toHaveAttribute("title", absoluteTime(SET_AT));
  });

  it("shows the empty state with Set an envelope when there are none", () => {
    renderCard(snap([]));
    expect(screen.getByText(ENVELOPE.EMPTY_TITLE)).toBeInTheDocument();
    expect(screen.getByText(ENVELOPE.EMPTY_BODY)).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: ENVELOPE.SET }).length).toBeGreaterThan(0);
  });
});

describe("Edit groups", () => {
  const open = async () => {
    const r = renderCard(snap([ENV_A]));
    await userEvent.click(screen.getByRole("button", { name: `${ENVELOPE.EDIT} Platform A` }));
    return r;
  };

  it("opens under the table with the current groups", async () => {
    await open();
    expect(screen.getByText(ENVELOPE.EDIT_TITLE("Platform A"))).toBeInTheDocument();
    expect(screen.getByText(ENVELOPE.EDIT_LEAD)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: POOL_DRAWER.REMOVE_SUBJECT("plat-a-users") })).toBeInTheDocument();
  });

  it("adds a trimmed group, ignores a duplicate and a blank, and empties the input", async () => {
    await open();
    const input = screen.getByLabelText(ENVELOPE.ADD_LABEL);
    expect(input).toHaveAttribute("placeholder", ENVELOPE.ADD_PLACEHOLDER);
    await userEvent.type(input, "  plat-a-extra  ");
    await userEvent.click(screen.getByRole("button", { name: POOL_DRAWER.ADD_TO_LIST }));
    expect(screen.getByRole("button", { name: POOL_DRAWER.REMOVE_SUBJECT("plat-a-extra") })).toBeInTheDocument();
    expect(input).toHaveValue("");
    await userEvent.type(input, "plat-a-users");
    await userEvent.click(screen.getByRole("button", { name: POOL_DRAWER.ADD_TO_LIST }));
    expect(screen.getAllByRole("button", { name: POOL_DRAWER.REMOVE_SUBJECT("plat-a-users") })).toHaveLength(1);
    await userEvent.click(screen.getByRole("button", { name: POOL_DRAWER.ADD_TO_LIST }));
    expect(screen.getAllByRole("listitem")).toHaveLength(3);
  });

  it("removes a group from the draft and saves the draft", async () => {
    const { onChanged } = await open();
    setEnvelopeMock.mockResolvedValue({ ...ENV_A, approved_groups: ["plat-a-builders", "plat-a-new"] });
    await userEvent.click(screen.getByRole("button", { name: POOL_DRAWER.REMOVE_SUBJECT("plat-a-users") }));
    await userEvent.type(screen.getByLabelText(ENVELOPE.ADD_LABEL), "plat-a-new");
    await userEvent.click(screen.getByRole("button", { name: POOL_DRAWER.ADD_TO_LIST }));
    await userEvent.click(screen.getByRole("button", { name: ENVELOPE.SAVE_GROUPS }));
    expect(setEnvelopeMock).toHaveBeenCalledWith("a", ["plat-a-builders", "plat-a-new"]);
    expect(onChanged).toHaveBeenCalledTimes(1);
    expect(screen.queryByText(ENVELOPE.EDIT_TITLE("Platform A"))).toBeNull();
  });

  it("Cancel closes the form without a write", async () => {
    await open();
    await userEvent.click(screen.getByRole("button", { name: ENVELOPE.CANCEL }));
    expect(screen.queryByText(ENVELOPE.EDIT_TITLE("Platform A"))).toBeNull();
    expect(setEnvelopeMock).not.toHaveBeenCalled();
  });

  it("shows a held save as the submitted note, closes the form and re-reads", async () => {
    const { onChanged } = await open();
    setEnvelopeMock.mockRejectedValue(
      new PendingChangeError({
        id: "c1",
        target_kind: "governance_envelope",
        op: "set",
        target_key: "a",
        state: "pending",
        proposed_by: "morgan@example.com",
        proposed_at: aheadByHours(0),
        expires_at: aheadByHours(72),
        diff: { changed: ["approved_groups"] },
      }),
    );
    await userEvent.click(screen.getByRole("button", { name: ENVELOPE.SAVE_GROUPS }));
    expect(await screen.findByText(CHANGES.SUBMITTED_TITLE)).toBeInTheDocument();
    expect(screen.queryByText(ENVELOPE.EDIT_TITLE("Platform A"))).toBeNull();
    expect(onChanged).toHaveBeenCalledTimes(1);
  });

  it("shows a 409 envelope_group_taken sentence in the red note, keeps the form, and clears the Add input", async () => {
    const { onChanged } = await open();
    const sentence = `the group "plat-b-users" is already approved on the envelope of unit "Platform B": a group sits on at most one envelope`;
    setEnvelopeMock.mockRejectedValue(new HttpError(409, sentence, "envelope_group_taken"));
    const input = screen.getByLabelText(ENVELOPE.ADD_LABEL);
    await userEvent.type(input, "plat-b-users");
    await userEvent.click(screen.getByRole("button", { name: POOL_DRAWER.ADD_TO_LIST }));
    await userEvent.type(input, "left-over");
    await userEvent.click(screen.getByRole("button", { name: ENVELOPE.SAVE_GROUPS }));
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText(ENVELOPE.REFUSED_TITLE)).toBeInTheDocument();
    expect(within(alert).getByText(sentence)).toBeInTheDocument();
    expect(screen.getByText(ENVELOPE.EDIT_TITLE("Platform A"))).toBeInTheDocument();
    expect(input).toHaveValue("");
    expect(input).not.toHaveAttribute("aria-invalid");
    expect(onChanged).not.toHaveBeenCalled();
  });
});

describe("Set an envelope", () => {
  const open = async () => {
    const r = renderCard(snap([ENV_A]));
    await userEvent.click(screen.getByRole("button", { name: ENVELOPE.SET }));
    return r;
  };

  it("offers only profiles that are not a root and not inside an envelope", async () => {
    await open();
    expect(screen.getByText(ENVELOPE.ROOT_HINT)).toBeInTheDocument();
    expect(screen.getByText(ENVELOPE.GROUPS_HINT)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("combobox", { name: ENVELOPE.ROOT_LABEL }));
    const options = (await screen.findAllByRole("option")).map((o) => o.textContent);
    expect(options).toEqual([D.name, E.name]);
  });

  it("keeps Save envelope off until a root is chosen, then sends the split, trimmed, unique groups", async () => {
    const { onChanged } = await open();
    setEnvelopeMock.mockResolvedValue({ unit_id: "e", approved_groups: ["g1", "g2"], set_by: "me", set_at: SET_AT });
    const save = screen.getByRole("button", { name: ENVELOPE.SAVE_ENVELOPE });
    expect(save).toBeDisabled();
    await userEvent.type(screen.getByLabelText(ENVELOPE.GROUPS_LABEL), "g1\n  g2  \n\ng1\n");
    expect(save).toBeDisabled();
    await userEvent.click(screen.getByRole("combobox", { name: ENVELOPE.ROOT_LABEL }));
    await userEvent.click(await screen.findByRole("option", { name: E.name }));
    expect(save).toBeEnabled();
    await userEvent.click(save);
    expect(setEnvelopeMock).toHaveBeenCalledWith("e", ["g1", "g2"]);
    expect(onChanged).toHaveBeenCalledTimes(1);
  });

  it("shows the server's envelope_nested sentence and keeps the form open", async () => {
    const { onChanged } = await open();
    const sentence = "an envelope already covers this unit's path: a unit above or below it has one, and only one envelope may apply to a unit";
    setEnvelopeMock.mockRejectedValue(new HttpError(409, sentence, "envelope_nested"));
    await userEvent.click(screen.getByRole("combobox", { name: ENVELOPE.ROOT_LABEL }));
    await userEvent.click(await screen.findByRole("option", { name: D.name }));
    await userEvent.click(screen.getByRole("button", { name: ENVELOPE.SAVE_ENVELOPE }));
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText(ENVELOPE.REFUSED_TITLE)).toBeInTheDocument();
    expect(within(alert).getByText(sentence)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: ENVELOPE.SAVE_ENVELOPE })).toBeInTheDocument();
    expect(onChanged).not.toHaveBeenCalled();
  });

  it("names a non-HTTP failure with its message in the same note", async () => {
    await open();
    setEnvelopeMock.mockRejectedValue(new Error("network down"));
    await userEvent.click(screen.getByRole("combobox", { name: ENVELOPE.ROOT_LABEL }));
    await userEvent.click(await screen.findByRole("option", { name: D.name }));
    await userEvent.click(screen.getByRole("button", { name: ENVELOPE.SAVE_ENVELOPE }));
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText(ENVELOPE.REFUSED_TITLE)).toBeInTheDocument();
    expect(within(alert).getByText("network down")).toBeInTheDocument();
  });
});

describe("Remove", () => {
  it("confirms with the platform's name, then clears the envelope and re-reads", async () => {
    const { onChanged } = renderCard(snap([ENV_A]));
    clearEnvelopeMock.mockResolvedValue(undefined);
    await userEvent.click(screen.getByRole("button", { name: `${ENVELOPE.REMOVE} Platform A` }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(ENVELOPE.REMOVE_TITLE("Platform A"))).toBeInTheDocument();
    expect(within(dialog).getByText(ENVELOPE.REMOVE_BODY("Platform A"))).toBeInTheDocument();
    expect(clearEnvelopeMock).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole("button", { name: ENVELOPE.REMOVE_CONFIRM }));
    expect(clearEnvelopeMock).toHaveBeenCalledWith("a");
    expect(onChanged).toHaveBeenCalledTimes(1);
  });

  it("shows a held removal as the submitted note", async () => {
    const { onChanged } = renderCard(snap([ENV_A]));
    clearEnvelopeMock.mockRejectedValue(
      new PendingChangeError({
        id: "c2",
        target_kind: "governance_envelope",
        op: "delete",
        target_key: "a",
        state: "pending",
        proposed_by: "morgan@example.com",
        proposed_at: aheadByHours(0),
        expires_at: aheadByHours(72),
        diff: { changed: [] },
      }),
    );
    await userEvent.click(screen.getByRole("button", { name: `${ENVELOPE.REMOVE} Platform A` }));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: ENVELOPE.REMOVE_CONFIRM }));
    expect(await screen.findByText(CHANGES.SUBMITTED_TITLE)).toBeInTheDocument();
    expect(onChanged).toHaveBeenCalledTimes(1);
  });
});

describe("a unit admin's read-only card", () => {
  const NAMED = (extra: Partial<GovernanceSnapshot> = {}) =>
    snap([{ ...ENV_A, set_by: "security" }], { profiles: [B, C], ...extra });
  const viewer = { security: false, principal: "uma@example.com" };

  it("draws nothing when no envelope covers a listed profile", () => {
    renderCard(snap([{ ...ENV_D, set_by: "security" }], { profiles: [B, C, E] }), viewer);
    expect(screen.queryByText(ENVELOPE.TITLE_UNIT_ADMIN)).toBeNull();
  });

  it("draws nothing when the server sent no envelopes", () => {
    renderCard(snap(undefined, { profiles: [B, C] }), viewer);
    expect(screen.queryByText(ENVELOPE.TITLE_UNIT_ADMIN)).toBeNull();
  });

  it("reads title, lead, help, the platform row with Security as Set by, and the three lists, with no buttons", () => {
    renderCard(NAMED(), viewer);
    expect(screen.getByRole("heading", { name: ENVELOPE.TITLE_UNIT_ADMIN })).toBeInTheDocument();
    expect(screen.getByText(ENVELOPE.LEAD_UNIT_ADMIN)).toBeInTheDocument();
    expect(screen.getByText(ENVELOPE.HELP)).toBeInTheDocument();
    const row = screen.getByRole("row", { name: /plat-a-users/ });
    expect(within(row).getByText("Platform A")).toBeInTheDocument();
    expect(within(row).getByText(ENVELOPE.SET_BY_SECURITY)).toBeInTheDocument();
    for (const s of ENVELOPE.APPLIES("Platform A")) expect(screen.getByText(s)).toBeInTheDocument();
    for (const s of ENVELOPE.SECOND("Platform A")) expect(screen.getByText(s)).toBeInTheDocument();
    expect(screen.getByText(ENVELOPE.SECURITY_ONLY("Platform A"))).toBeInTheDocument();
    for (const t of [ENVELOPE.APPLIES_TITLE, ENVELOPE.SECOND_TITLE, ENVELOPE.SECURITY_TITLE]) {
      expect(screen.getByRole("heading", { name: t })).toBeInTheDocument();
    }
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.queryByText(ENVELOPE.ROOT_ADMIN_TITLE)).toBeNull();
  });

  it("shows the root-admin note only to a viewer whose grant is on the root", () => {
    const grant = (subject: string, unit_id = "a") => ({
      id: "g1",
      unit_id,
      subject_type: "user" as const,
      subject,
      granted_by: "security",
      created_at: "",
    });
    renderCard(NAMED({ unit_admins: [grant("uma@example.com")] }), viewer);
    expect(screen.getByText(ENVELOPE.ROOT_ADMIN_TITLE)).toBeInTheDocument();
    expect(screen.getByText(ENVELOPE.ROOT_ADMIN_BODY("Platform A"))).toBeInTheDocument();
  });

  it("omits the root-admin note for a grant on a unit below the root", () => {
    renderCard(
      NAMED({ unit_admins: [{ id: "g2", unit_id: "b", subject_type: "user", subject: "uma@example.com", granted_by: "security", created_at: "" }] }),
      viewer,
    );
    expect(screen.queryByText(ENVELOPE.ROOT_ADMIN_TITLE)).toBeNull();
  });
});
