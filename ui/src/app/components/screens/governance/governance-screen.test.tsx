/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GovernanceScreen — the security admin's profiles/assignments surface (0.7
// Phase 4). Every assertion reads its expected string from a copy module rather
// than retyping it, the convention permissions.test.tsx and access-panel.test
// .tsx already follow: these tests fail the moment a rendered string stops
// coming from the canon.
//
// Four things are pinned here because they are DECISIONS, not rendering:
//   1. exactly one `default` (teal) button per state (CONSOLE-RULES §6),
//   2. the add form COLLAPSES while the editor is open — the mechanism that
//      keeps rule 1 true without inventing a modal,
//   3. delete is TWO refusals: a pre-filled, count-bearing one the client can
//      see, and a COUNT-FREE 409 for the race it cannot,
//   4. no component in this directory renders a product string of its own.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expectNoOwnCopy } from "../../../lib/test-fixtures";

// The SafetyMeter (one per profile row, plus the editor's) debounces a
// POST /policies/grade. Stub it so no test touches the network.
const gradePolicyMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: { gradePolicy: (...a: unknown[]) => gradePolicyMock(...a) },
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

const getGovernanceMock = vi.fn();
const createProfileMock = vi.fn();
const updateProfileMock = vi.fn();
const deleteProfileMock = vi.fn();
const upsertAssignmentMock = vi.fn();
const deleteAssignmentMock = vi.fn();
const previewGovernanceMock = vi.fn();
vi.mock("../../../lib/api/governance", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/governance")>("../../../lib/api/governance");
  return {
    ...actual,
    governance: {
      getGovernance: () => getGovernanceMock(),
      createProfile: (...a: unknown[]) => createProfileMock(...a),
      updateProfile: (...a: unknown[]) => updateProfileMock(...a),
      deleteProfile: (...a: unknown[]) => deleteProfileMock(...a),
      upsertAssignment: (...a: unknown[]) => upsertAssignmentMock(...a),
      deleteAssignment: (...a: unknown[]) => deleteAssignmentMock(...a),
      previewGovernance: (...a: unknown[]) => previewGovernanceMock(...a),
    },
  };
});

// The Who field is a DirectoryCombobox (§I). It defaults here to the
// deployment with NO directory configured (the search reports unconfigured →
// null → the plain text input it has always been, no chrome); the one test
// that wants suggestions overrides it. The combobox's own states are pinned in
// wardyn/directory-combobox.test.tsx.
const directorySearchMock = vi.fn();
vi.mock("../../../lib/api/directory", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/directory")>("../../../lib/api/directory");
  return { ...actual, directory: { search: (...a: unknown[]) => directorySearchMock(...a) } };
});

import { HttpError } from "../../../lib/api/core";
import type { DirectoryEntry } from "../../../lib/api/directory";
import type { GovernanceProfile, GovernanceSnapshot } from "../../../lib/api/governance";
import { DIRECTORY, GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { ACCESS_STATE, PEOPLE, PREVIEW } from "../../../lib/people-access-copy";
import { PERM } from "../../../lib/permissions-copy";
import type { RunPolicySpec } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";
import { question } from "./display";
import { GovernanceScreen } from "./governance-screen";

const SPEC: RunPolicySpec = {
  allowed_domains: ["api.anthropic.com"],
  first_use_approval: "deny_with_review",
  min_confinement_class: "CC2",
};

function profile(over: Partial<GovernanceProfile> = {}): GovernanceProfile {
  return {
    id: "p1",
    name: "Greenfield contractors",
    ceiling: SPEC,
    limits: {},
    created_at: "2026-08-28T00:00:00Z",
    updated_at: "2026-08-28T00:00:00Z",
    ...over,
  };
}

const GREENFIELD = profile();
const PLATFORM = profile({ id: "p2", name: "Platform" });

function snapshot(over: Partial<GovernanceSnapshot> = {}): GovernanceSnapshot {
  return {
    profiles: [GREENFIELD, PLATFORM],
    assignments: [
      {
        id: "a1",
        subject_type: "group",
        subject: "wardyn.platform",
        profile_id: PLATFORM.id,
        priority: 10,
        created_at: "2026-08-29T00:00:00Z",
      },
    ],
    ...over,
  };
}

// The screen's ONE `default` button. Radix's role="switch" limit toggles carry
// bg-primary too when on, and getAllByRole("button") correctly excludes them —
// they are switches, not buttons.
function tealButtons(): HTMLElement[] {
  return screen
    .getAllByRole("button")
    .filter((b) => b.className.split(/\s+/).includes("bg-primary"));
}

// Each test awaits its own first findBy*, so this only primes the fetch.
function renderScreen(snap: GovernanceSnapshot | null = snapshot()) {
  if (snap) getGovernanceMock.mockResolvedValue(snap);
  else getGovernanceMock.mockRejectedValue(new Error("boom"));
  render(<GovernanceScreen />);
}

beforeEach(() => {
  gradePolicyMock.mockReset();
  gradePolicyMock.mockResolvedValue({ overall_risk: "medium", risk_assessment: [] });
  getGovernanceMock.mockReset();
  createProfileMock.mockReset();
  updateProfileMock.mockReset();
  deleteProfileMock.mockReset();
  upsertAssignmentMock.mockReset();
  deleteAssignmentMock.mockReset();
  previewGovernanceMock.mockReset();
  previewGovernanceMock.mockResolvedValue({});
  directorySearchMock.mockReset();
  directorySearchMock.mockResolvedValue(null);
});

describe("GovernanceScreen — states", () => {
  it("renders the frozen page header", async () => {
    renderScreen();
    expect(await screen.findByRole("heading", { name: GOV.TITLE, level: 1 })).toBeInTheDocument();
    expect(screen.getByText(GOV.LEAD)).toBeInTheDocument();
  });

  it("fetch_failed is distinct from empty, says enforcement is unaffected, and offers Retry", async () => {
    renderScreen(null);
    expect(await screen.findByText(GOV.FETCH_FAILED_TITLE)).toBeInTheDocument();
    expect(screen.getByText(GOV.FETCH_FAILED_BODY)).toBeInTheDocument();
    expect(screen.queryByText(GOV.EMPTY_TITLE)).not.toBeInTheDocument();

    getGovernanceMock.mockResolvedValue(snapshot());
    await userEvent.click(screen.getByRole("button", { name: ACCESS_STATE.FETCH_FAILED_RETRY }));
    expect(await screen.findByText(GREENFIELD.name)).toBeInTheDocument();
  });

  it("no profiles: the empty state carries New profile, and it is the ONE teal", async () => {
    renderScreen({ profiles: [], assignments: [] });
    expect(await screen.findByText(GOV.EMPTY_TITLE)).toBeInTheDocument();
    // withMono splits this one across sibling text nodes (WARDYN_DEFAULT_POLICY
    // renders as a <Mono>), so no single node holds the whole sentence — the
    // access-panel.test.tsx precedent: textContent concatenates every
    // descendant in order, and String.split with a capturing group neither
    // drops nor adds a character, so the original reconstructs exactly.
    expect(document.body.textContent).toContain(GOV.EMPTY_BODY);
    // …and the env var really is mono, not merely present as text.
    expect(screen.getByText("WARDYN_DEFAULT_POLICY").className).toContain("font-mono");

    const teal = tealButtons();
    expect(teal).toHaveLength(1);
    expect(teal[0]).toHaveTextContent(GOV.NEW_CTA);
    // …and only one New profile button exists at all — the header's is dropped
    // rather than duplicating the empty state's.
    expect(screen.getAllByRole("button", { name: GOV.NEW_CTA })).toHaveLength(1);
  });

  it("profiles but no assignments is its own state — a profile with no assignment bounds nobody", async () => {
    renderScreen(snapshot({ assignments: [] }));
    expect(await screen.findByText(GOV.EMPTY_ASSIGN_TITLE)).toBeInTheDocument();
    expect(screen.getByText(GOV.EMPTY_ASSIGN_BODY)).toBeInTheDocument();
  });
});

describe("GovernanceScreen — the profiles table", () => {
  it("renders the assignment count, the limits and the ceiling grade per row", async () => {
    renderScreen(snapshot({ profiles: [profile({ limits: { deny_task_mode_exec: true } }), PLATFORM] }));
    await screen.findByText(GREENFIELD.name);

    // PLATFORM carries the one assignment in the fixture; Greenfield carries none.
    expect(screen.getByText(GOV.ASSIGNED_COUNT(1))).toBeInTheDocument();
    expect(screen.getByText(GOV.ASSIGNED_NONE)).toBeInTheDocument();
    expect(screen.getByText(GOV.LIMIT_EXEC_LABEL)).toBeInTheDocument();
    expect(screen.getByText(GOV.LIMITS_NONE)).toBeInTheDocument();
    // The shipped meter, not a second grade vocabulary.
    expect(screen.getAllByTestId("safety-meter")).toHaveLength(2);
  });

  it("at rest the ONE teal is Assign — New profile and Edit are not", async () => {
    renderScreen();
    await screen.findByText(GREENFIELD.name);
    const teal = tealButtons();
    expect(teal).toHaveLength(1);
    expect(teal[0]).toHaveTextContent(GOV.ADD_CTA);
  });
});

describe("GovernanceScreen — the editor collapses the add form (one teal at a time)", () => {
  it("opening the editor collapses the add form to a disabled summary row and moves the teal to Save profile", async () => {
    renderScreen();
    await screen.findByText(GREENFIELD.name);
    // Before: the add form is live and its Assign is the teal.
    expect(screen.queryByTestId("governance-add-assignment-collapsed")).not.toBeInTheDocument();
    expect(screen.getByLabelText(GOV.FIELD_PRIORITY)).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: `${GOV.EDIT} ${GREENFIELD.name}` }));

    expect(await screen.findByTestId("governance-profile-editor")).toBeInTheDocument();
    expect(screen.getByText(GOV.EDITOR_TITLE_EDIT(GREENFIELD.name))).toBeInTheDocument();

    // The form collapsed to its ADD_TITLE summary row — no new string, and the
    // fields are gone rather than merely disabled.
    const collapsed = screen.getByTestId("governance-add-assignment-collapsed");
    expect(within(collapsed).getByText(GOV.ADD_TITLE)).toBeInTheDocument();
    expect(within(collapsed).getByRole("button", { name: GOV.ADD_CTA })).toBeDisabled();
    expect(screen.queryByLabelText(GOV.FIELD_PRIORITY)).not.toBeInTheDocument();

    const teal = tealButtons();
    expect(teal).toHaveLength(1);
    expect(teal[0]).toHaveTextContent(GOV.SAVE);
  });

  it("Cancel re-expands the add form and hands the teal back to Assign", async () => {
    renderScreen();
    await screen.findByText(GREENFIELD.name);
    await userEvent.click(screen.getByRole("button", { name: `${GOV.EDIT} ${GREENFIELD.name}` }));
    await userEvent.click(screen.getByRole("button", { name: PEOPLE.CANCEL }));

    expect(screen.queryByTestId("governance-profile-editor")).not.toBeInTheDocument();
    expect(screen.queryByTestId("governance-add-assignment-collapsed")).not.toBeInTheDocument();
    const teal = tealButtons();
    expect(teal).toHaveLength(1);
    expect(teal[0]).toHaveTextContent(GOV.ADD_CTA);
  });
});

describe("GovernanceScreen — profile writes", () => {
  it("a successful save renders the server's OMISSION warnings verbatim, in order, and never blocks", async () => {
    const warnings = [
      'this profile omits 1 denied domain(s) the deployment default denies (*.corp.example) — members under it are NOT walled from them',
      'this profile sets allow_all_egress while the deployment default does not — members under it reach any non-denied public host',
    ];
    updateProfileMock.mockResolvedValue({ profile: GREENFIELD, warnings });
    renderScreen();
    await screen.findByText(GREENFIELD.name);

    await userEvent.click(screen.getByRole("button", { name: `${GOV.EDIT} ${GREENFIELD.name}` }));
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));

    expect(await screen.findByText(GOV.OMISSION_TITLE)).toBeInTheDocument();
    for (const w of warnings) expect(screen.getByText(w)).toBeInTheDocument();
    // Non-blocking: the write went through and the editor closed.
    expect(updateProfileMock).toHaveBeenCalledTimes(1);
    expect(screen.queryByTestId("governance-profile-editor")).not.toBeInTheDocument();
  });

  it("a grant-bound 400 renders the server's message under the frozen heading, editor still open", async () => {
    const body =
      'invalid ceiling: eligible grant "api_key" pairing secret "stripe-live" with host "api.stripe.com" is not in the deployment ceiling (a profile may narrow the deployment\'s eligible grants, never add one)';
    updateProfileMock.mockRejectedValue(new HttpError(400, body));
    renderScreen();
    await screen.findByText(GREENFIELD.name);

    await userEvent.click(screen.getByRole("button", { name: `${GOV.EDIT} ${GREENFIELD.name}` }));
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));

    expect(await screen.findByText(GOV.GRANT_BOUND_TITLE)).toBeInTheDocument();
    // Verbatim: the server names the failing leg, the grant, the secret and the
    // host — a frozen sentence could not.
    expect(screen.getByText(body)).toBeInTheDocument();
    expect(screen.getByTestId("governance-profile-editor")).toBeInTheDocument();
  });

  it("a transport failure (no answer at all) renders SAVE_ERROR, not a server message", async () => {
    updateProfileMock.mockRejectedValue(new TypeError("Failed to fetch"));
    renderScreen();
    await screen.findByText(GREENFIELD.name);

    await userEvent.click(screen.getByRole("button", { name: `${GOV.EDIT} ${GREENFIELD.name}` }));
    await userEvent.click(screen.getByRole("button", { name: GOV.SAVE }));

    expect(await screen.findByText(GOV.SAVE_ERROR)).toBeInTheDocument();
    expect(screen.queryByText(GOV.GRANT_BOUND_TITLE)).not.toBeInTheDocument();
  });
});

describe("GovernanceScreen — delete: the pre-fill and the race are different refusals", () => {
  it("a visibly-assigned profile opens PRE-FILLED with the count, confirm disabled, nothing attempted", async () => {
    renderScreen();
    // Twice over: the profiles row's name cell and the assignment row's
    // Profile cell — this fixture is precisely the assigned case.
    await screen.findAllByText(PLATFORM.name);

    await userEvent.click(screen.getByRole("button", { name: `${GOV.DELETE} ${PLATFORM.name}` }));
    const dialog = await screen.findByRole("alertdialog");

    expect(within(dialog).getByText(GOV.DELETE_RESTRICT_TITLE)).toBeInTheDocument();
    expect(within(dialog).getByText(GOV.DELETE_RESTRICT_BODY(PLATFORM.name, 1))).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: GOV.DELETE })).toBeDisabled();
    expect(deleteProfileMock).not.toHaveBeenCalled();
  });

  it("an unassigned profile confirms with the frozen consequence, and deletes", async () => {
    deleteProfileMock.mockResolvedValue(undefined);
    renderScreen();
    await screen.findByText(GREENFIELD.name);

    await userEvent.click(screen.getByRole("button", { name: `${GOV.DELETE} ${GREENFIELD.name}` }));
    const dialog = await screen.findByRole("alertdialog");
    // The confirm's two halves are ONE frozen string, split for display only —
    // the split itself is pinned by the `question` describe below.
    const [head, body] = question(GOV.DELETE_CONFIRM(GREENFIELD.name));
    expect(within(dialog).getByText(head)).toBeInTheDocument();
    expect(within(dialog).getByText(body)).toBeInTheDocument();
    expect(within(dialog).queryByText(GOV.DELETE_RESTRICT_TITLE)).not.toBeInTheDocument();

    await userEvent.click(within(dialog).getByRole("button", { name: GOV.DELETE }));
    expect(deleteProfileMock).toHaveBeenCalledWith(GREENFIELD.id);
  });

  it("the race: a 409 is rendered COUNT-FREE, post-attempt, with the confirm still enabled", async () => {
    const conflict =
      "this governance profile is still assigned — delete its assignments first (deleting it while assigned would silently widen everyone it bounds back to the deployment ceiling)";
    deleteProfileMock.mockRejectedValue(new HttpError(409, conflict));
    renderScreen();
    await screen.findByText(GREENFIELD.name);

    await userEvent.click(screen.getByRole("button", { name: `${GOV.DELETE} ${GREENFIELD.name}` }));
    const dialog = await screen.findByRole("alertdialog");
    const confirm = within(dialog).getByRole("button", { name: GOV.DELETE });
    await userEvent.click(confirm);

    expect(await within(dialog).findByText(GOV.DELETE_RESTRICT_TITLE)).toBeInTheDocument();
    expect(within(dialog).getByText(conflict)).toBeInTheDocument();
    // COUNT-FREE: the client believed the count was zero and the shipped 409
    // carries no n, so the count-bearing body must NOT appear on this path.
    expect(within(dialog).queryByText(GOV.DELETE_RESTRICT_BODY(GREENFIELD.name, 0))).not.toBeInTheDocument();
    expect(confirm).not.toBeDisabled();
  });
});

describe("GovernanceScreen — assignments and the resolved preview", () => {
  it("renders both halves of the effect fact, and the precedence rule", async () => {
    renderScreen();
    await screen.findByText(GOV.ASSIGN_TITLE);
    expect(screen.getByText(GOV.PRECEDENCE)).toBeInTheDocument();
    expect(screen.getByText(GOV.EFFECT_NOTE)).toBeInTheDocument();
    expect(screen.getByText(GOV.SIGNIN_NOTE)).toBeInTheDocument();
  });

  it("priority is shown only inside the group tier — a user row reads PRIORITY_NA", async () => {
    renderScreen(
      snapshot({
        assignments: [
          {
            id: "a2",
            subject_type: "user",
            subject: "alice@corp.example",
            profile_id: GREENFIELD.id,
            priority: 7,
            created_at: "2026-08-30T00:00:00Z",
          },
        ],
      }),
    );
    await screen.findByText("alice@corp.example");
    expect(screen.getByText(GOV.PRIORITY_NA)).toBeInTheDocument();
  });

  it("unassign confirms with the frozen sentence and removes", async () => {
    deleteAssignmentMock.mockResolvedValue(undefined);
    renderScreen();
    await screen.findByText("wardyn.platform");

    await userEvent.click(screen.getByRole("button", { name: `${PERM.REMOVE} wardyn.platform` }));
    const dialog = await screen.findByRole("alertdialog");
    const [head, body] = question(GOV.UNASSIGN_CONFIRM("wardyn.platform", PLATFORM.name));
    expect(within(dialog).getByText(head)).toBeInTheDocument();
    expect(within(dialog).getByText(body)).toBeInTheDocument();

    await userEvent.click(within(dialog).getByRole("button", { name: PERM.REMOVE }));
    expect(deleteAssignmentMock).toHaveBeenCalledWith("a1");
  });

  // THE §I swap, end to end: the Who field is the same free-text box it was,
  // and what a picked row puts in it — and therefore on the wire — is the
  // CLAIM VALUE, never the display name. A group's `groups` claim carries the
  // object id, so assigning "Platform Engineering" would bind nobody.
  it("a picked suggestion assigns the claim value, not the name shown", async () => {
    const GROUP: DirectoryEntry = {
      display_name: "Platform Engineering",
      claim_value: "8f3c1a2b-0000-4d1e-9f00-abcdef012345",
      kind: "group",
      detail: "group · 8f3c1a2b",
    };
    directorySearchMock.mockResolvedValue([GROUP]);
    upsertAssignmentMock.mockResolvedValue(undefined);
    renderScreen();
    await screen.findByText(GOV.ASSIGN_TITLE);

    const who = screen.getByRole("textbox", { name: PERM.FIELD_WHO });
    await userEvent.type(who, "plat");
    await userEvent.click(
      await screen.findByRole("button", { name: DIRECTORY.SUGGEST_ROW(GROUP.display_name, GROUP.detail!) }),
    );
    expect(who).toHaveValue(GROUP.claim_value);
    // The kind the segmented control names is the kind the search asked for.
    expect(directorySearchMock).toHaveBeenCalledWith("plat", "group");

    await userEvent.click(screen.getByRole("combobox")); // the profile Select
    await userEvent.click(await screen.findByRole("option", { name: PLATFORM.name }));
    await userEvent.click(screen.getByRole("button", { name: GOV.ADD_CTA }));

    expect(upsertAssignmentMock).toHaveBeenCalledWith({
      subject_type: "group",
      subject: GROUP.claim_value,
      profile_id: PLATFORM.id,
      priority: 0,
    });
  });

  it("the preview takes CLAIMS (the People step's own field) and names the matching row's tier", async () => {
    previewGovernanceMock.mockResolvedValue({
      profile_id: PLATFORM.id,
      profile_name: PLATFORM.name,
      matched_tier: "group",
    });
    renderScreen();
    await screen.findByText(GOV.PREVIEW_TITLE);
    expect(screen.getByText(PREVIEW.FIELD_CLAIMS_HINT)).toBeInTheDocument();

    await userEvent.type(screen.getByLabelText(PREVIEW.FIELD_CLAIMS), "wardyn.platform");
    await userEvent.click(screen.getByRole("button", { name: GOV.PREVIEW_RUN_CTA }));

    expect(
      await screen.findByText(GOV.PREVIEW_RESULT(PLATFORM.name, GOV.MATCHED_GROUP)),
    ).toBeInTheDocument();
    expect(screen.getByText(GOV.PREVIEW_NOT_SAVED)).toBeInTheDocument();
  });

  // THE thing that replaced a client-side ORDER BY: this screen ships claims
  // and renders an answer. The precedence itself is pinned server-side, against
  // a real Postgres, in internal/api/governance_preview_test.go — where every
  // case is asserted equal to a direct ResolveGovernanceProfile call.
  //
  // What is still THIS file's to pin is the one mapping the client owns: a
  // kind-less textarea onto two typed wire lists. Every line reaches BOTH (the
  // server offers each to both tiers and its answer names the one that
  // matched), and user_subjects arrives sign-in-subject-first so the resolver's
  // array_position tie-break agrees with capabilitySubjects.
  it("offers every claim to both tiers, sign-in subject before email", async () => {
    renderScreen();
    await screen.findByText(GOV.PREVIEW_TITLE);
    await userEvent.type(
      screen.getByLabelText(PREVIEW.FIELD_CLAIMS),
      "alice@corp.example{enter}8f2c-sub-id{enter}eng",
    );
    await userEvent.click(screen.getByRole("button", { name: GOV.PREVIEW_RUN_CTA }));

    expect(previewGovernanceMock).toHaveBeenCalledWith({
      user_subjects: ["8f2c-sub-id", "eng", "alice@corp.example"],
      groups: ["alice@corp.example", "8f2c-sub-id", "eng"],
    });
  });

  it("claims that match nothing resolve to the deployment ceiling", async () => {
    renderScreen();
    await screen.findByText(GOV.PREVIEW_TITLE);
    await userEvent.type(screen.getByLabelText(PREVIEW.FIELD_CLAIMS), "nobody");
    await userEvent.click(screen.getByRole("button", { name: GOV.PREVIEW_RUN_CTA }));
    expect(await screen.findByText(GOV.PREVIEW_RESULT_DEFAULT)).toBeInTheDocument();
  });

  it("a failed preview is the one honest cause of PREVIEW_RESULT_UNKNOWN", async () => {
    renderScreen();
    await screen.findByText(GOV.PREVIEW_TITLE);
    previewGovernanceMock.mockRejectedValueOnce(new Error("boom"));
    await userEvent.type(screen.getByLabelText(PREVIEW.FIELD_CLAIMS), "wardyn.platform");
    await userEvent.click(screen.getByRole("button", { name: GOV.PREVIEW_RUN_CTA }));
    expect(await screen.findByText(GOV.PREVIEW_RESULT_UNKNOWN)).toBeInTheDocument();
  });
});

// The precedence table that used to live here is GONE, deliberately. It tested
// a TypeScript mirror of ResolveGovernanceProfile's ORDER BY, and the mirror is
// what Phase 4's finish lane deleted: the preview now POSTs /governance/preview
// and the server runs the real resolver. The four ranks — tier, sub-over-email,
// priority DESC, name ASC — are pinned in
// internal/api/governance_preview_test.go against a real Postgres, every case
// asserted equal to a direct ResolveGovernanceProfile call, plus the store's
// own governance_pg_test.go. Re-adding a client-side table here would re-create
// exactly the second matcher that was removed.

// THE canon pin: a component in this directory may render copy, never author
// it. Anything a reader sees comes from governance-copy.ts (frozen §7) or the
// modules §7.1 defers to — so a quoted or bare prose string in this source is a
// canon break, not a style question.
describe("Governance components render no copy of their own", () => {
  // The vitest project root (ui/) is the resolve base: vite rewrites
  // import.meta.url to a root-relative URL.
  expectNoOwnCopy("src/app/components/screens/governance", [
    "governance-screen.tsx",
    "profile-editor.tsx",
    "assignments.tsx",
    "display.tsx",
  ]);
});

describe("GovernanceScreen — the write gate", () => {
  it("a caller without the security tier gets every write control disabled", async () => {
    // OperatorProvider defaults securityOperator TRUE (fail-open), so the
    // restricted case is the one worth pinning: it must come from the provider,
    // never from an unwrapped default.
    getGovernanceMock.mockResolvedValue(snapshot());
    render(
      <OperatorProvider operator={false} securityOperator={false}>
        <GovernanceScreen />
      </OperatorProvider>,
    );
    await screen.findByText(GREENFIELD.name);

    expect(screen.getByRole("button", { name: GOV.NEW_CTA })).toBeDisabled();
    expect(screen.getByRole("button", { name: `${GOV.EDIT} ${GREENFIELD.name}` })).toBeDisabled();
    expect(screen.getByRole("button", { name: `${GOV.DELETE} ${GREENFIELD.name}` })).toBeDisabled();
    expect(screen.getByRole("button", { name: GOV.ADD_CTA })).toBeDisabled();
  });
});

// A DISPLAY split, never a rewrite: the dialog titles itself with the question
// and puts the consequence below it, and the two halves rejoin byte for byte.
describe("question — the confirm split", () => {
  it.each([GOV.DELETE_CONFIRM("X"), GOV.UNASSIGN_CONFIRM("who", "X")])("rejoins %s", (s) => {
    const [head, rest] = question(s);
    expect(`${head} ${rest}`).toBe(s);
    expect(head.endsWith("?")).toBe(true);
  });
});

// The user-drive DOOR (0.7 user drives, mock state 6). Its two strings are
// governance canon — GOV.LIMIT_DRIVE_LABEL / _HINT, appended to
// governance-prompt.md §7.2 — and the drives module never carries a copy: the
// security admin's whole authority over drives is this switch, /drives itself
// being SUPER.
describe("GovernanceScreen — the third limit is the user-drive door", () => {
  it("the editor's Limits section has three rows, and the third writes deny_user_drive", async () => {
    updateProfileMock.mockResolvedValue({ profile: GREENFIELD, warnings: [] });
    renderScreen();
    await screen.findByText(GREENFIELD.name);
    await userEvent.click(screen.getByRole("button", { name: `${GOV.EDIT} ${GREENFIELD.name}` }));

    const editor = await screen.findByTestId("governance-profile-editor");
    expect(within(editor).getAllByRole("switch")).toHaveLength(3);
    const door = within(editor).getByRole("switch", { name: GOV.LIMIT_DRIVE_LABEL });
    expect(door).toHaveAttribute("aria-checked", "false");
    expect(within(editor).getByText(GOV.LIMIT_DRIVE_HINT)).toBeInTheDocument();

    await userEvent.click(door);
    await userEvent.click(within(editor).getByRole("button", { name: GOV.SAVE }));

    expect(updateProfileMock).toHaveBeenCalledWith(
      GREENFIELD.id,
      expect.objectContaining({ limits: { deny_user_drive: true } }),
    );
  });

  it("a profile with the door shut carries its chip in the Limits column, beside the other two", async () => {
    renderScreen(
      snapshot({
        profiles: [profile({ limits: { deny_task_mode_exec: true, deny_user_drive: true } }), PLATFORM],
      }),
    );
    await screen.findByText(GREENFIELD.name);
    const limits = within(screen.getAllByRole("table")[0]);
    expect(limits.getByText(GOV.LIMIT_EXEC_LABEL)).toBeInTheDocument();
    expect(limits.getByText(GOV.LIMIT_DRIVE_LABEL)).toBeInTheDocument();
    // …and a profile with no limits still reads LIMITS_NONE, which the third
    // flag must not have quietly turned into "one limit set".
    expect(limits.getByText(GOV.LIMITS_NONE)).toBeInTheDocument();
  });
});
