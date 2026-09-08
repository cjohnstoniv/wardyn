/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Sentinel pins for the admin Permissions screen. Every assertion below reads
// its expected string from lib/permissions-copy.ts rather than retyping it, so
// these tests fail the moment a rendered string stops coming from the canon —
// which is the property the owner's mock approval actually bought.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const toastError = vi.fn();
vi.mock("sonner", () => ({
  toast: { error: (...a: unknown[]) => toastError(...a), success: vi.fn(), warning: vi.fn() },
}));

const getPermissionsMock = vi.fn();
const upsertGrantMock = vi.fn();
const deleteGrantMock = vi.fn();
const putEnforcementMock = vi.fn();
vi.mock("../../lib/api/permissions", () => ({
  permissions: {
    getPermissions: () => getPermissionsMock(),
    upsertGrant: (...a: unknown[]) => upsertGrantMock(...a),
    deleteGrant: (...a: unknown[]) => deleteGrantMock(...a),
    putEnforcement: (...a: unknown[]) => putEnforcementMock(...a),
  },
}));

const listRunsMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({ runs: { listRuns: () => listRunsMock() } }));

import { CAPABILITY_KINDS, KIND, PERM } from "../../lib/permissions-copy";
import type { CapabilityGrant } from "../../lib/types";
import { PermissionsScreen } from "./permissions";
import { OperatorProvider } from "../wardyn/operator-context";

function grant(over: Partial<CapabilityGrant> = {}): CapabilityGrant {
  return {
    id: "11111111-1111-1111-1111-111111111111",
    subject_type: "user",
    subject: "alice@corp.example",
    capability: "egress_host",
    value: "*.github.com",
    effect: "allow",
    created_at: "2026-08-01T00:00:00Z",
    created_by: "admin",
    ...over,
  };
}

function renderScreen() {
  return render(
    <MemoryRouter>
      <PermissionsScreen />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  toastError.mockClear();
  getPermissionsMock.mockReset().mockResolvedValue({ grants: [], enforcement: {} });
  upsertGrantMock.mockReset();
  deleteGrantMock.mockReset();
  putEnforcementMock.mockReset();
  listRunsMock.mockReset().mockResolvedValue([]);
});

describe("PermissionsScreen — fresh install (every kind off, zero grants)", () => {
  it("states the default posture, the doctrine and the exemption", async () => {
    renderScreen();
    await screen.findByText(PERM.DEFAULT_POSTURE);
    expect(screen.getByText(PERM.DOCTRINE)).toBeInTheDocument();
    expect(screen.getByText(PERM.EXEMPT)).toBeInTheDocument();
    expect(screen.getByText(PERM.LEAD)).toBeInTheDocument();
  });

  // DERIVED from CAPABILITY_KINDS, never a hand-listed set: the kind list is
  // the contract, and a spec that re-typed it kept passing while the screen
  // grew two kinds it never rendered (0.7's `agent`/`integration`).
  it("renders every kind off, each with its unenforced consequence", async () => {
    renderScreen();
    await screen.findByText(PERM.DEFAULT_POSTURE);
    for (const k of CAPABILITY_KINDS) {
      // The label also appears in the add form's capability picker, so this
      // asserts presence rather than uniqueness.
      expect(screen.getAllByText(KIND[k].label).length).toBeGreaterThan(0);
      expect(screen.getByText(KIND[k].unenforced)).toBeInTheDocument();
    }
    expect(screen.getAllByText(PERM.CHIP_OFF)).toHaveLength(CAPABILITY_KINDS.length);
    expect(screen.queryByText(PERM.CHIP_ON)).toBeNull();
  });

  it("shows the empty-grants copy, not a table", async () => {
    renderScreen();
    await screen.findByText(PERM.EMPTY_TITLE);
    expect(screen.getByText(PERM.EMPTY_BODY)).toBeInTheDocument();
    expect(screen.queryByRole("table")).toBeNull();
  });
});

describe("PermissionsScreen — advisory, enforced, and the dangerous cell", () => {
  it("an off kind with grants is advisory, and a deny row says it still bites", async () => {
    getPermissionsMock.mockResolvedValue({
      grants: [grant({ capability: "secret", value: "STRIPE_LIVE_KEY", effect: "deny" })],
      enforcement: {},
    });
    renderScreen();
    await screen.findByText(PERM.ADVISORY);
    expect(screen.getByText(PERM.DENY_BEFORE_ENFORCE)).toBeInTheDocument();
    // Never render enforcement that isn't happening.
    expect(screen.queryByText(PERM.CHIP_ON)).toBeNull();
  });

  it("an enforced kind swaps to its enforced consequence and the Enforced chip", async () => {
    getPermissionsMock.mockResolvedValue({ grants: [grant()], enforcement: { egress_host: true } });
    renderScreen();
    await screen.findByText(KIND.egress_host.enforced);
    expect(screen.getAllByText(PERM.CHIP_ON)).toHaveLength(1);
    expect(screen.queryByText(KIND.egress_host.unenforced)).toBeNull();
    expect(screen.queryByText(PERM.ADVISORY)).toBeNull();
  });

  it("enforced with no allow grant carries the lockout warning", async () => {
    getPermissionsMock.mockResolvedValue({
      grants: [grant({ capability: "workspace", value: "ws-1", effect: "deny" })],
      enforcement: { workspace: true },
    });
    renderScreen();
    await screen.findByText(PERM.ENFORCE_ON_ZERO);
  });
});

describe("PermissionsScreen — enforcement confirms", () => {
  it("turning a kind on names the affected member count and writes the FULL map", async () => {
    getPermissionsMock.mockResolvedValue({ grants: [grant()], enforcement: {} });
    // Two distinct human run creators, plus the admin-token machine lane which
    // must not be counted as a member.
    listRunsMock.mockResolvedValue([
      { created_by: "bob@corp.example" },
      { created_by: "dana@corp.example" },
      { created_by: "bob@corp.example" },
      { created_by: "admin-token" },
    ]);
    putEnforcementMock.mockResolvedValue({ egress_host: true });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();

    await screen.findByText(KIND.egress_host.unenforced);
    await user.click(screen.getByRole("switch", { name: `${PERM.ENFORCEMENT_TITLE} ${KIND.egress_host.label}` }));

    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(PERM.ENFORCE_ON_TITLE(KIND.egress_host.label))).toBeInTheDocument();
    expect(within(dialog).getByText(PERM.ENFORCE_ON_BODY(2))).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: PERM.ENFORCE_CONFIRM }));

    // The PUT carries EVERY kind, not just the one that flipped (the endpoint
    // takes the whole map). Derived from CAPABILITY_KINDS so adding a kind
    // cannot leave this asserting a stale, short body.
    await waitFor(() =>
      expect(putEnforcementMock).toHaveBeenCalledWith(
        Object.fromEntries(CAPABILITY_KINDS.map((k) => [k, k === "egress_host"])),
      ),
    );
  });

  it("turning a kind on with nothing granted repeats the lockout warning in the dialog", async () => {
    getPermissionsMock.mockResolvedValue({ grants: [], enforcement: {} });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();

    await screen.findByText(KIND.workspace.unenforced);
    await user.click(screen.getByRole("switch", { name: `${PERM.ENFORCEMENT_TITLE} ${KIND.workspace.label}` }));

    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(PERM.ENFORCE_ON_ZERO)).toBeInTheDocument();
  });

  it("turning a kind off says denies still apply, and offers Stop enforcing", async () => {
    getPermissionsMock.mockResolvedValue({ grants: [grant()], enforcement: { egress_host: true } });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();

    await screen.findByText(KIND.egress_host.enforced);
    await user.click(screen.getByRole("switch", { name: `${PERM.ENFORCEMENT_TITLE} ${KIND.egress_host.label}` }));

    const dialog = await screen.findByRole("alertdialog");
    // The TITLE has to ask the same question as the body and the button — it
    // used to ask the opposite one ("Enforce Egress hosts?").
    expect(within(dialog).getByText(PERM.ENFORCE_OFF_TITLE(KIND.egress_host.label))).toBeInTheDocument();
    expect(within(dialog).queryByText(PERM.ENFORCE_ON_TITLE(KIND.egress_host.label))).toBeNull();
    expect(within(dialog).getByText(PERM.ENFORCE_OFF_BODY)).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: PERM.ENFORCE_STOP })).toBeInTheDocument();
  });
});

describe("PermissionsScreen — a snapshot that never arrived (R4/F015)", () => {
  // The enforcement block states what the daemon is refusing RIGHT NOW. Its
  // "Not enforced" chip and the member-powers prose beneath it are claims, so
  // they may only come from a snapshot that actually arrived — the seed object
  // `{ grants: [], enforcement: {} }` is indistinguishable from a real
  // zero-config answer, which is why there is no seed any more.
  it("paints no kind state at all when GET /permissions fails — an ErrorState, not six 'Not enforced' chips", async () => {
    getPermissionsMock.mockRejectedValue(new Error("boom"));
    renderScreen();

    await screen.findAllByRole("button", { name: /retry/i });
    expect(screen.queryAllByText(PERM.CHIP_OFF)).toHaveLength(0);
    expect(screen.queryByText(PERM.CHIP_ON)).toBeNull();
    for (const k of CAPABILITY_KINDS) {
      expect(screen.queryByText(KIND[k].unenforced)).toBeNull();
      expect(screen.queryByText(KIND[k].enforced)).toBeNull();
    }
    // ...and it must not claim the zero-config posture either.
    expect(screen.queryByText(PERM.DEFAULT_POSTURE)).toBeNull();
  });

  it("retrying after a failure renders the kinds from the snapshot that arrives", async () => {
    getPermissionsMock.mockRejectedValueOnce(new Error("boom")).mockResolvedValue({
      grants: [grant()],
      enforcement: { egress_host: true },
    });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();

    const retry = (await screen.findAllByRole("button", { name: /retry/i }))[0];
    await user.click(retry);
    await screen.findByText(KIND.egress_host.enforced);
    expect(screen.getAllByText(PERM.CHIP_OFF)).toHaveLength(CAPABILITY_KINDS.length - 1);
  });
});

describe("PermissionsScreen — an affected-member count that could not be read (R4/F133)", () => {
  it("says members are bounded WITHOUT a number when GET /runs is refused — never '0 members'", async () => {
    getPermissionsMock.mockResolvedValue({ grants: [grant()], enforcement: {} });
    listRunsMock.mockRejectedValue(new Error("403"));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();

    await screen.findByText(KIND.egress_host.unenforced);
    await user.click(screen.getByRole("switch", { name: `${PERM.ENFORCEMENT_TITLE} ${KIND.egress_host.label}` }));

    const dialog = await screen.findByRole("alertdialog");
    // The counted form said "0 members are bounded" — i.e. "this affects
    // nobody", the opposite of the lockout risk the dialog exists to state.
    expect(within(dialog).queryByText(/\b0 members\b/)).toBeNull();
    expect(within(dialog).queryByText(PERM.ENFORCE_ON_BODY(0))).toBeNull();
    expect(within(dialog).getByText(PERM.ENFORCE_ON_BODY_UNKNOWN)).toBeInTheDocument();
  });
});

describe("PermissionsScreen — the grant table", () => {
  it("renders who/capability/value/effect, allow amber and deny red — never a success tone", async () => {
    getPermissionsMock.mockResolvedValue({
      grants: [
        grant(),
        grant({ id: "22222222-2222-2222-2222-222222222222", subject_type: "all", subject: "", capability: "secret", value: "GITHUB_TOKEN" }),
        grant({ id: "33333333-3333-3333-3333-333333333333", subject_type: "group", subject: "wardyn.contractors", effect: "deny", value: "registry.npmjs.org" }),
      ],
      enforcement: {},
    });
    renderScreen();

    await screen.findByText("*.github.com");
    expect(screen.getByText(PERM.PRECEDENCE)).toBeInTheDocument();
    expect(screen.getByText(PERM.GRANT_IS_NOT_SUCCESS)).toBeInTheDocument();
    for (const col of [PERM.COL_WHO, PERM.COL_CAPABILITY, PERM.COL_VALUE, PERM.COL_EFFECT, PERM.COL_ADDED]) {
      expect(screen.getByRole("columnheader", { name: col })).toBeInTheDocument();
    }
    // Scoped to the table: the add form's Who/Effect segments carry the same
    // words, and it is the ROW's tone that the honesty rule is about.
    const table = screen.getByRole("table");
    expect(within(table).getByText(PERM.SUBJECT_ALL)).toBeInTheDocument();
    expect(within(table).getByText(PERM.SUBJECT_GROUP)).toBeInTheDocument();
    const allow = within(table).getAllByText(PERM.EFFECT_ALLOW)[0];
    expect(allow.className).toContain("warning");
    expect(allow.className).not.toContain("success");
    expect(within(table).getByText(PERM.EFFECT_DENY).className).toContain("danger");
  });

  it("removing a grant confirms with the subject named, then deletes it", async () => {
    getPermissionsMock.mockResolvedValue({ grants: [grant()], enforcement: {} });
    deleteGrantMock.mockResolvedValue(undefined);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();

    await screen.findByText("*.github.com");
    await user.click(screen.getByRole("button", { name: `${PERM.REMOVE} ${KIND.egress_host.label} *.github.com` }));

    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(PERM.REMOVE_CONFIRM("alice@corp.example"))).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: PERM.REMOVE }));

    await waitFor(() => expect(deleteGrantMock).toHaveBeenCalledWith(grant().id));
  });
});

describe("PermissionsScreen — add a grant", () => {
  it("the value label and hint follow the chosen capability", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();

    await screen.findByText(PERM.ADD_TITLE);
    expect(screen.getByLabelText(KIND.egress_host.valueLabel)).toBeInTheDocument();
    expect(screen.getByText(KIND.egress_host.valueHint)).toBeInTheDocument();

    await user.click(screen.getByRole("combobox", { name: PERM.FIELD_CAPABILITY }));
    await user.click(await screen.findByRole("option", { name: KIND.image.label }));

    expect(screen.getByLabelText(KIND.image.valueLabel)).toBeInTheDocument();
    expect(screen.getByText(KIND.image.valueHint)).toBeInTheDocument();
  });

  it("the Who hint follows the subject type, and 'all' drops the subject field", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();

    await screen.findByText(PERM.ADD_TITLE);
    expect(screen.getByText(PERM.HINT_USER)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: PERM.SUBJECT_GROUP }));
    expect(screen.getByText(PERM.HINT_GROUP)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: PERM.SUBJECT_ALL }));
    expect(screen.getByText(PERM.HINT_ALL)).toBeInTheDocument();
    expect(screen.queryByLabelText(PERM.FIELD_WHO)).toBeNull();
  });

  it("submits the natural key + effect, and says so when the upsert only updated one", async () => {
    upsertGrantMock.mockResolvedValue({ grant: grant(), updated: true });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();

    await screen.findByText(PERM.ADD_TITLE);
    await user.type(screen.getByLabelText(PERM.FIELD_WHO), "alice@corp.example");
    await user.type(screen.getByLabelText(KIND.egress_host.valueLabel), "*.github.com");
    await user.click(screen.getByRole("button", { name: PERM.EFFECT_DENY }));
    await user.click(screen.getByRole("button", { name: PERM.ADD_CTA }));

    await waitFor(() =>
      expect(upsertGrantMock).toHaveBeenCalledWith({
        subject_type: "user",
        subject: "alice@corp.example",
        capability: "egress_host",
        value: "*.github.com",
        effect: "deny",
      }),
    );
    expect(await screen.findByText(PERM.DUPLICATE)).toBeInTheDocument();
  });
});

// 0.7 §B: all four /permissions routes register on securityOps
// (routes.go's mountPermissionRoutes) — this IS the org allow/denylist
// primitive, and it is safe to hand over because capAllowed/capGranted stay on
// isOperator (capabilities.go), so no capability kind can widen the admin tier.
describe("PermissionsScreen — the security tier writes grants", () => {
  it("leaves Add grant live for a security admin, and dead for a plain member", async () => {
    const asTier = (operator: boolean, securityOperator: boolean) =>
      render(
        <MemoryRouter>
          <OperatorProvider operator={operator} securityOperator={securityOperator}>
            <PermissionsScreen />
          </OperatorProvider>
        </MemoryRouter>,
      );

    // The Who field, not the Add button: the button also disables on an empty
    // form (`!ready`), so it would pass for the wrong reason.
    const sec = asTier(false, true);
    expect(await sec.findByLabelText(PERM.FIELD_WHO)).not.toBeDisabled();
    sec.unmount();

    const member = asTier(false, false);
    expect(await member.findByLabelText(PERM.FIELD_WHO)).toBeDisabled();
  });
});

describe("PermissionsScreen — the group-snapshot ceiling", () => {
  it("states that groups are read at sign-in and grants resolve per request", async () => {
    renderScreen();
    await screen.findByText(PERM.SNAPSHOT_TITLE);
    expect(screen.getByText(PERM.SNAPSHOT_BODY)).toBeInTheDocument();
  });
});
