/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Deciders section of a profile: who besides security admins decides each kind of hold.
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { toast } from "sonner";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

const putMock = vi.fn();
vi.mock("../../../lib/api/deciders", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/deciders")>("../../../lib/api/deciders");
  return { ...actual, putUnitDeciders: (...a: unknown[]) => putMock(...a) };
});

// The anchor test mounts the whole editor, which grades its ceiling and reads setup status.
vi.mock("../../../lib/api/runs", () => ({ runs: { gradePolicy: vi.fn().mockResolvedValue({ overall_risk: "medium", risk_assessment: [] }) } }));
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: vi.fn().mockResolvedValue({}) } }));

import { HttpError, PendingChangeError } from "../../../lib/api/core";
import type { DeciderRule, GovernanceDeciders } from "../../../lib/api/deciders";
import type { GovernanceUnit } from "../../../lib/api/governance";
import { CHANGES, GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { aheadByHours } from "../../../lib/test-clock";
import { DECIDERS, DECIDER_KIND_LABEL } from "../../wardyn/copy/deciders";
import { ProfileEditor } from "./profile-editor";
import { whoPhrase } from "./decider-rules";
import { UnitDeciders } from "./unit-deciders";

const unit = (over: Partial<GovernanceUnit> & { id: string; name: string }): GovernanceUnit => ({
  ceiling: {},
  limits: {},
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
  ...over,
});

const P = unit({ id: "p1", name: "Payments" });
const CHILD = unit({ id: "c1", name: "Payments web", parent_id: "p1" });

const FOUR: DeciderRule[] = [
  { kinds: ["egress_domain"], who: ["owner", "group:eng-leads"], max_scope: "run" },
  { kinds: ["ado_escalation", "mcp_call", "kube_call"], who: ["unit_admins", "group:eng-peers"], second_human: true },
  { kinds: ["credential", "push_content"], who: ["unit_admins"], second_human: true, max_scope: "once" },
  { kinds: ["elevation"], who: ["unit_admins", "group:support-leads"], max_elevation: "4h", eligible_requesters: ["group:support"] },
];
const ownEntry = (rules: DeciderRule[], extra: Partial<GovernanceDeciders> = {}): GovernanceDeciders => ({ unit_id: "p1", rules, ...extra });

function mount(over: Partial<React.ComponentProps<typeof UnitDeciders>> = {}) {
  const onChanged = vi.fn();
  const view = render(
    <UnitDeciders profile={P} profiles={[P]} deciders={[ownEntry(FOUR)]} supported unitNames={{ p1: "Payments" }} disabled={false} onChanged={onChanged} {...over} />,
  );
  return { onChanged, ...view };
}

const button = (name: string | RegExp) => screen.getByRole("button", { name });
const checkbox = (name: string | RegExp) => screen.getByRole("checkbox", { name });
const editorBox = (i: number) => screen.getByTestId(`decider-rule-editor-${i}`);

beforeEach(() => {
  putMock.mockReset();
  vi.mocked(toast.success).mockReset();
});

describe("UnitDeciders: frame A, a profile with four rules", () => {
  it("lists each rule with its kinds, who decides and the fact line, and keeps Save off until an edit", () => {
    mount();
    expect(screen.getByRole("heading", { name: DECIDERS.TITLE })).toBeInTheDocument();
    expect(screen.getByText(DECIDERS.LEAD)).toBeInTheDocument();
    expect(screen.getByText(DECIDERS.HELP)).toBeInTheDocument();
    for (const k of ["egress_domain", "ado_escalation", "mcp_call", "kube_call", "credential", "push_content", "elevation"] as const) {
      expect(screen.getAllByText(DECIDER_KIND_LABEL[k]).length).toBeGreaterThan(0);
    }
    expect(screen.getByText(DECIDERS.WHO_OWNER)).toBeInTheDocument();
    expect(screen.getByText(DECIDERS.WHO_GROUP("eng-leads"))).toBeInTheDocument();
    expect(screen.getByText(`${DECIDERS.FACT_AT_MOST(DECIDERS.SCOPE_RUN)}`)).toBeInTheDocument();
    expect(screen.getByText(`${DECIDERS.FACT_SECOND} · ${DECIDERS.FACT_AT_MOST(DECIDERS.SCOPE_ONCE)}`)).toBeInTheDocument();
    expect(screen.getByText(`${DECIDERS.FACT_UP_TO("4 hours")} · ${DECIDERS.FACT_ASK(DECIDERS.WHO_GROUP("support"))}`)).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: DECIDERS.EDIT })).toHaveLength(4);
    expect(screen.getByText(DECIDERS.ALWAYS_NOTE)).toBeInTheDocument();
    expect(button(DECIDERS.SAVE)).toBeDisabled();
    expect(button(DECIDERS.DISCARD)).toBeDisabled();
  });

  it("offers no lock checkbox", () => {
    mount();
    expect(screen.queryByText(/lock for units below/i)).toBeNull();
  });

  it("turns Save on after an edit and puts the draft back on Discard", async () => {
    mount();
    await userEvent.click(screen.getAllByRole("button", { name: DECIDERS.REMOVE })[0]);
    expect(screen.queryByText(DECIDERS.WHO_OWNER)).toBeNull();
    expect(button(DECIDERS.SAVE)).toBeEnabled();
    await userEvent.click(button(DECIDERS.DISCARD));
    expect(screen.getByText(DECIDERS.WHO_OWNER)).toBeInTheDocument();
    expect(button(DECIDERS.SAVE)).toBeDisabled();
  });
});

describe("UnitDeciders: frame D, inherited and locked", () => {
  const inherited = [
    { unit_id: "c1", rules: [], from: { egress_domain: "p1" } } as GovernanceDeciders,
    ownEntry([FOUR[0]]),
  ];

  it("shows a rule from above read only with the unit's name, and offers to set rules here", () => {
    mount({ profile: CHILD, profiles: [P, CHILD], deciders: inherited, unitNames: { p1: "Payments" } });
    expect(screen.getByText(DECIDERS.FROM("Payments"))).toBeInTheDocument();
    expect(screen.getByText(DECIDERS.WHO_OWNER)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: DECIDERS.EDIT })).toBeNull();
    expect(screen.getByText(DECIDERS.INHERITED_HELPER("Payments"))).toBeInTheDocument();
    expect(button(DECIDERS.SET_HERE)).toBeInTheDocument();
  });

  it("says From a unit above when the name is not known", () => {
    mount({ profile: CHILD, profiles: [P, CHILD], deciders: inherited, unitNames: {} });
    expect(screen.getByText(DECIDERS.FROM_ABOVE)).toBeInTheDocument();
  });

  it("is read only under a lock, with the badge, the explanation and no add of any kind", () => {
    const locked = { ...P, locked_knobs: ["deciders"] };
    mount({ profile: CHILD, profiles: [locked, CHILD], deciders: inherited });
    expect(screen.getByText(DECIDERS.LOCKED_BY("Payments"))).toBeInTheDocument();
    expect(screen.getByText(DECIDERS.LOCKED_EXPLAIN("Payments"))).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: DECIDERS.ADD })).toBeNull();
    expect(screen.queryByRole("button", { name: DECIDERS.SET_HERE })).toBeNull();
    expect(screen.queryByRole("button", { name: DECIDERS.SAVE })).toBeNull();
  });

  it("is read only under a lock for rules of its own as well", () => {
    const locked = { ...P, locked_knobs: ["deciders"] };
    mount({ profile: CHILD, profiles: [locked, CHILD], deciders: [{ unit_id: "c1", rules: [FOUR[1]] }, ownEntry([FOUR[0]])] });
    expect(screen.getByText(DECIDERS.WHO_GROUP("eng-peers"))).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: DECIDERS.EDIT })).toBeNull();
    expect(screen.queryByRole("button", { name: DECIDERS.REMOVE })).toBeNull();
  });
});

describe("UnitDeciders: frame F, the states", () => {
  it("says an older server does not support deciders", () => {
    mount({ supported: false, deciders: undefined });
    expect(screen.getByText(DECIDERS.OLDER_SERVER)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: DECIDERS.ADD })).toBeNull();
  });

  it("asks a new profile to be saved first", () => {
    mount({ profile: null });
    expect(screen.getByText(DECIDERS.NEW_PROFILE)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: DECIDERS.ADD })).toBeNull();
  });

  it("explains that no rules means holds are decided as before, and offers Add rule", () => {
    mount({ deciders: [] });
    expect(screen.getByText(DECIDERS.EMPTY_TITLE)).toBeInTheDocument();
    expect(screen.getByText(DECIDERS.EMPTY_BODY)).toBeInTheDocument();
    expect(button(DECIDERS.ADD)).toBeInTheDocument();
    expect(screen.queryByText(/not used yet/i)).toBeNull();
  });

  it("is read only when disabled", () => {
    mount({ disabled: true });
    expect(screen.getByText(DECIDERS.WHO_OWNER)).toBeInTheDocument();
    for (const name of [DECIDERS.EDIT, DECIDERS.REMOVE, DECIDERS.ADD, DECIDERS.SAVE, DECIDERS.DISCARD]) {
      expect(screen.queryByRole("button", { name })).toBeNull();
    }
  });
});

describe("UnitDeciders: what the rules above leave", () => {
  it("names who decides when more than one unit has a rule for the kind", () => {
    const entry = ownEntry([FOUR[0]], {
      bounded_by: { egress_domain: ["p1", "root"] },
      effective: { egress_domain: { kinds: ["egress_domain"], who: ["group:eng-leads"] } },
    });
    mount({ deciders: [entry] });
    expect(screen.getByText(DECIDERS.MET(whoPhrase(["group:eng-leads"])))).toBeInTheDocument();
  });

  it("says only security decides when the rules name nobody in common", () => {
    const entry = ownEntry([FOUR[0]], {
      bounded_by: { egress_domain: ["p1", "root"] },
      effective: { egress_domain: { kinds: ["egress_domain"], who: [] } },
    });
    mount({ deciders: [entry] });
    expect(screen.getByText(DECIDERS.MET_NONE)).toBeInTheDocument();
  });

  it("says nothing when only this unit has a rule for the kind", () => {
    const entry = ownEntry([FOUR[0]], {
      bounded_by: { egress_domain: ["p1"] },
      effective: { egress_domain: { kinds: ["egress_domain"], who: ["owner"] } },
    });
    mount({ deciders: [entry] });
    expect(screen.queryByText(/with the rules above/i)).toBeNull();
  });
});

describe("UnitDeciders: the rule editor", () => {
  it("disables a kind another rule names, with the rule's number", async () => {
    mount();
    await userEvent.click(screen.getAllByRole("button", { name: DECIDERS.EDIT })[0]);
    const box = within(editorBox(0));
    expect(box.getByRole("checkbox", { name: new RegExp(DECIDER_KIND_LABEL.egress_domain) })).toBeChecked();
    expect(box.getByRole("checkbox", { name: new RegExp(DECIDER_KIND_LABEL.ado_escalation) })).toBeDisabled();
    expect(box.getAllByText(DECIDERS.IN_RULE(2)).length).toBeGreaterThan(0);
    expect(box.getByText(DECIDERS.ALWAYS_NEVER)).toBeInTheDocument();
    expect(box.getByText(DECIDERS.COUNTER(2))).toBeInTheDocument();
  });

  it("makes elevation its own rule, with its own limits and no owner or scope", async () => {
    mount({ deciders: [] });
    await userEvent.click(button(DECIDERS.ADD));
    const box = within(editorBox(0));
    await userEvent.click(box.getByRole("checkbox", { name: new RegExp(DECIDER_KIND_LABEL.elevation) }));
    expect(box.getByText(DECIDERS.ELEVATION_OWN_RULE)).toBeInTheDocument();
    expect(box.getByRole("checkbox", { name: new RegExp(DECIDERS.WHO_OWNER) })).toBeDisabled();
    expect(box.getByText(DECIDERS.OWNER_OFF_REQUEST)).toBeInTheDocument();
    expect(box.queryByText(DECIDERS.SCOPE_LABEL)).toBeNull();
    expect(box.queryByText(DECIDERS.SECOND)).toBeNull();
    expect(box.getByRole("checkbox", { name: new RegExp(DECIDER_KIND_LABEL.egress_domain) })).toBeDisabled();
    expect(box.getAllByText(DECIDERS.NOT_WITH).length).toBeGreaterThan(0);
    // Nothing is chosen yet, so the two required fields say so and Done is off.
    expect(box.getByText(DECIDERS.ELEV_MAX_ERROR)).toBeInTheDocument();
    expect(box.getByText(DECIDERS.ASK_REQUIRED)).toBeInTheDocument();
    expect(box.getByRole("button", { name: DECIDERS.DONE })).toBeDisabled();
    // Ten minutes is under the floor.
    await userEvent.type(box.getByLabelText(DECIDERS.ELEV_MAX_LABEL), "10");
    expect(box.getByText(DECIDERS.ELEV_MAX_ERROR)).toBeInTheDocument();
    await userEvent.clear(box.getByLabelText(DECIDERS.ELEV_MAX_LABEL));
    await userEvent.type(box.getByLabelText(DECIDERS.ELEV_MAX_LABEL), "120");
    expect(box.queryByText(DECIDERS.ELEV_MAX_ERROR)).toBeNull();
  });

  it("forces the second person on for a credential and keeps the owner out", async () => {
    mount({ deciders: [] });
    await userEvent.click(button(DECIDERS.ADD));
    const box = within(editorBox(0));
    await userEvent.click(box.getByRole("checkbox", { name: new RegExp(DECIDER_KIND_LABEL.credential) }));
    expect(box.getByRole("checkbox", { name: new RegExp(DECIDERS.SECOND) })).toBeChecked();
    expect(box.getByRole("checkbox", { name: new RegExp(DECIDERS.SECOND) })).toBeDisabled();
    expect(box.getByText(DECIDERS.SECOND_FORCED)).toBeInTheDocument();
    expect(box.getByRole("checkbox", { name: new RegExp(DECIDERS.WHO_OWNER) })).toBeDisabled();
    expect(box.getByText(DECIDERS.OWNER_OFF_CRED)).toBeInTheDocument();
  });

  it("stops the owner and a second person from being chosen together", async () => {
    mount({ deciders: [] });
    await userEvent.click(button(DECIDERS.ADD));
    const box = within(editorBox(0));
    await userEvent.click(box.getByRole("checkbox", { name: new RegExp(DECIDER_KIND_LABEL.egress_domain) }));
    await userEvent.click(box.getByRole("checkbox", { name: new RegExp(DECIDERS.WHO_OWNER) }));
    expect(box.getByRole("checkbox", { name: new RegExp(DECIDERS.SECOND) })).toBeDisabled();
    expect(box.getByText(DECIDERS.OWNER_HINT_EGRESS)).toBeInTheDocument();
  });

  it("counts every decider and stops adding groups at eight", async () => {
    const eight: DeciderRule = { kinds: ["mcp_call"], who: ["unit_admins", ...Array.from({ length: 7 }, (_, i) => `group:g${i}`)] };
    mount({ deciders: [ownEntry([eight])] });
    await userEvent.click(button(DECIDERS.EDIT));
    const box = within(editorBox(0));
    expect(box.getByText(DECIDERS.COUNTER(8))).toBeInTheDocument();
    expect(box.getByRole("button", { name: DECIDERS.ADD_GROUP })).toBeDisabled();
  });

  it("adds a group once and not when it is empty", async () => {
    mount({ deciders: [] });
    await userEvent.click(button(DECIDERS.ADD));
    const box = within(editorBox(0));
    await userEvent.click(box.getByRole("button", { name: DECIDERS.ADD_GROUP }));
    expect(box.getByText(DECIDERS.COUNTER(0))).toBeInTheDocument();
    await userEvent.type(box.getByPlaceholderText(DECIDERS.GROUP_PLACEHOLDER), " ops ");
    await userEvent.click(box.getByRole("button", { name: DECIDERS.ADD_GROUP }));
    await userEvent.type(box.getByPlaceholderText(DECIDERS.GROUP_PLACEHOLDER), "ops");
    await userEvent.click(box.getByRole("button", { name: DECIDERS.ADD_GROUP }));
    expect(box.getAllByText(DECIDERS.WHO_GROUP("ops"))).toHaveLength(1);
    expect(box.getByText(DECIDERS.COUNTER(1))).toBeInTheDocument();
  });
});

describe("UnitDeciders: Save", () => {
  async function addEgressRule() {
    await userEvent.click(button(DECIDERS.ADD));
    const box = within(editorBox(0));
    await userEvent.click(box.getByRole("checkbox", { name: new RegExp(DECIDER_KIND_LABEL.egress_domain) }));
    await userEvent.click(box.getByRole("checkbox", { name: new RegExp(DECIDERS.WHO_OWNER) }));
    await userEvent.type(box.getByPlaceholderText(DECIDERS.GROUP_PLACEHOLDER), "e2e-leads");
    await userEvent.click(box.getByRole("button", { name: DECIDERS.ADD_GROUP }));
    await userEvent.click(box.getByRole("radio", { name: DECIDERS.SCOPE_ONCE }));
    await userEvent.click(box.getByRole("button", { name: DECIDERS.DONE }));
  }

  it("sends the draft, toasts and reloads on a 200", async () => {
    putMock.mockResolvedValue({ unit_id: "p1", rules: [] });
    const { onChanged } = mount({ deciders: [] });
    await addEgressRule();
    await userEvent.click(button(DECIDERS.SAVE));
    expect(putMock).toHaveBeenCalledWith("p1", [{ kinds: ["egress_domain"], who: ["owner", "group:e2e-leads"], max_scope: "once" }]);
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith(DECIDERS.SAVED));
    expect(onChanged).toHaveBeenCalledTimes(1);
  });

  it("sends an elevation rule with who may ask and its limit as one duration", async () => {
    putMock.mockResolvedValue({ unit_id: "p1", rules: [] });
    mount({ deciders: [] });
    await userEvent.click(button(DECIDERS.ADD));
    const box = within(editorBox(0));
    await userEvent.click(box.getByRole("checkbox", { name: new RegExp(DECIDER_KIND_LABEL.elevation) }));
    await userEvent.click(box.getByRole("checkbox", { name: new RegExp(DECIDERS.WHO_ADMINS) }));
    await userEvent.type(box.getByLabelText(DECIDERS.ELEV_MAX_LABEL), "120");
    await userEvent.type(box.getAllByPlaceholderText(DECIDERS.GROUP_PLACEHOLDER)[1], "support");
    await userEvent.click(box.getAllByRole("button", { name: DECIDERS.ADD_GROUP })[1]);
    await userEvent.type(box.getByLabelText(DECIDERS.TICKET_PATTERN_LABEL), "[A-Z]+-[0-9]+");
    expect(box.getByRole("button", { name: DECIDERS.DONE })).toBeEnabled();
    await userEvent.click(box.getByRole("button", { name: DECIDERS.DONE }));
    await userEvent.click(button(DECIDERS.SAVE));
    expect(putMock).toHaveBeenCalledWith("p1", [
      { kinds: ["elevation"], who: ["unit_admins"], max_elevation: "2h", eligible_requesters: ["group:support"], ticket_pattern: "[A-Z]+-[0-9]+" },
    ]);
  });

  it("shows the submitted note, keeps the draft and reloads when the change is held", async () => {
    putMock.mockRejectedValue(new PendingChangeError({ id: "ch1" } as never));
    const { onChanged } = mount({ deciders: [] });
    await addEgressRule();
    await userEvent.click(button(DECIDERS.SAVE));
    expect(await screen.findByText(CHANGES.SUBMITTED_TITLE)).toBeInTheDocument();
    expect(onChanged).toHaveBeenCalledTimes(1);
    expect(toast.success).not.toHaveBeenCalled();
  });

  it.each([
    ["400 invalid_deciders", new HttpError(400, "invalid deciders: rules[1]: who lists owner twice", "invalid_deciders")],
    ["422 decider_rule_wider_than_parent", new HttpError(422, "rules[1]: egress_domain for group:other reaches further than Payments above this unit allows", "decider_rule_wider_than_parent")],
  ])("opens the rule a %s names, with the server's sentence", async (_n, err) => {
    putMock.mockRejectedValue(err);
    mount();
    await userEvent.click(screen.getAllByRole("button", { name: DECIDERS.REMOVE })[3]);
    await userEvent.click(button(DECIDERS.SAVE));
    const box = await screen.findByTestId("decider-rule-editor-1");
    expect(within(box).getByText(DECIDERS.REFUSED_TITLE)).toBeInTheDocument();
    expect(within(box).getByText(err.message)).toBeInTheDocument();
    expect(box).toHaveAttribute("aria-invalid", "true");
    expect(button(DECIDERS.SAVE)).toBeEnabled();
  });

  it("shows a lock refusal as a note only", async () => {
    const sentence = "deciders is locked by Payments; ask the platform team";
    putMock.mockRejectedValue(new HttpError(409, sentence, "unit_knob_locked"));
    mount();
    await userEvent.click(screen.getAllByRole("button", { name: DECIDERS.REMOVE })[3]);
    await userEvent.click(button(DECIDERS.SAVE));
    expect(await screen.findByText(sentence)).toBeInTheDocument();
    expect(screen.getByText(DECIDERS.REFUSED_TITLE)).toBeInTheDocument();
    expect(screen.queryByTestId("decider-rule-editor-0")).toBeNull();
  });

  it("says nothing was saved when the request never got an answer", async () => {
    putMock.mockRejectedValue(new TypeError("network down"));
    mount();
    await userEvent.click(screen.getAllByRole("button", { name: DECIDERS.REMOVE })[3]);
    await userEvent.click(button(DECIDERS.SAVE));
    expect(await screen.findByText(DECIDERS.TRANSIT_BODY)).toBeInTheDocument();
    expect(screen.getByText(DECIDERS.REFUSED_TITLE)).toBeInTheDocument();
    expect(button(DECIDERS.SAVE)).toBeEnabled();
  });
});

describe("ProfileEditor: the Deciders section", () => {
  it("has a jump link whose anchor exists, between Limits and Contact", () => {
    render(
      <ProfileEditor profile={P} profiles={[P]} disabled={false} onCancel={vi.fn()} onSaved={vi.fn()} onSubmitted={vi.fn()} deciders={[]} decidersSupported unitNames={{}} onDecidersChanged={vi.fn()} />,
    );
    const links = within(screen.getByTestId("section-anchors")).getAllByRole("link");
    const labels = links.map((l) => l.textContent);
    expect(labels.indexOf(DECIDERS.TITLE)).toBe(labels.indexOf(GOV.LIMITS_TITLE) + 1);
    const href = links[labels.indexOf(DECIDERS.TITLE)].getAttribute("href")!.slice(1);
    expect(href).toBe("governance-section-deciders");
    expect(document.getElementById(href)).not.toBeNull();
  });
});
