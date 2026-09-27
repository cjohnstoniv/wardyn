/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The /approvals card's audience for a mid-run AWS sign-in request (W6-U
// BLOCKER-1 / BLOCKER-2).
//
// Its own file rather than more cases in approvals.test.tsx (668 lines, one
// shared api fake that every other describe depends on): this seam needs a
// different fixture — a reauth scope with a credential_source and an owner —
// and a different provider stack, one viewer per case.
//
// The rule under test is the same rule the cockpit row follows
// (live-approvals-reauth.test.tsx), because it is one function: the door
// renders only for the viewer whose own sign-in the server would accept for
// this row — the subject the row names on the per_user lane, an operator on
// the shared one (reauthResolvableBy, internal/api's injection_awssso.go).

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest, MeCapabilities } from "../../lib/types";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { MODEL_ACCESS_BANNER, REAUTH_ROW, REAUTH_TITLE } from "../wardyn/model-access-copy";
import { CONSOLE_VIEW, OPEN_IN_USER_VIEW } from "../wardyn/copy/console-view";
import { MODEL_PROVIDERS, providerStatus } from "../../lib/test-fixtures";
import { WithDoor } from "../../../test/door-harness";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));
vi.mock("./settings/harness-login-pane", () => ({
  HarnessLoginPane: (p: { modelProvider?: string }) => <div data-testid="fake-pane" data-model-provider={p.modelProvider ?? ""} />,
}));

let mockScope: Record<string, unknown> = {};
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: async (state: string) =>
      state === "PENDING"
        ? [
            {
              id: "apr_1",
              run_id: "run_1",
              kind: "credential_reauth",
              requested_scope: mockScope,
              state: "PENDING",
              requested_at: new Date().toISOString(),
            } satisfies ApprovalRequest,
          ]
        : [],
    approve: vi.fn(),
    deny: vi.fn(),
  },
}));
const caps: MeCapabilities = {
  grants: [],
  enforcement: {},
  session_groups: [],
  groups_snapshot_stale: false,
};
vi.mock("../../lib/api/permissions", () => ({
  permissions: { getMyCapabilities: () => Promise.resolve(caps) },
}));
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: () =>
      Promise.resolve({
        id: "run_1",
        agent: "claude-code",
        task: "t",
        confinement_class: "CC2",
        state: "RUNNING",
        created_by: "alice@corp",
      }),
  },
}));

import { ApprovalsScreen } from "./approvals";
import { OperatorProvider } from "../wardyn/operator-context";

const PER_USER = { mechanism: "bedrock_sso", credential_source: "per_user", owner: "alice@corp" };
const SHARED = { mechanism: "bedrock_sso", credential_source: "shared", owner: "" };

async function mount(scope: Record<string, unknown>, operator: boolean, principal: string, path = "/approvals") {
  mockScope = scope;
  render(
    <OperatorProvider operator={operator} securityOperator={operator} principal={principal}>
      <MemoryRouter initialEntries={[path]}>
        <ModelAccessProvider status={null} onRefresh={() => {}}>
          <ApprovalsScreen />
        </ModelAccessProvider>
      </MemoryRouter>
    </OperatorProvider>,
  );
  await screen.findByText(REAUTH_TITLE);
}

const door = () => screen.queryByRole("button", { name: REAUTH_ROW.ariaLabel });

describe("/approvals — who is offered the held run's sign-in door", () => {
  beforeEach(() => {
    mockScope = PER_USER;
  });

  it("the OWNER on the per_user lane: the door, and the both-branches hint", async () => {
    await mount(PER_USER, false, "alice@corp");
    expect(door()).toBeInTheDocument();
    expect(screen.getByText(REAUTH_ROW.hint)).toBeInTheDocument();
  });

  it("a SHARED-lane operator: the door — the shared credential is theirs", async () => {
    await mount(SHARED, true, "admin@corp");
    expect(door()).toBeInTheDocument();
  });

  it("a SHARED-lane member: the instruction, no door — the server refuses their sign-in", async () => {
    // harnessLoginNotPerUserRefusal: a shared-lane member's own sign-in is
    // never what the server would accept, so the cockpit row and this card
    // both withhold the button rather than call it "your sign-in".
    await mount(SHARED, false, "alice@corp");
    expect(door()).not.toBeInTheDocument();
    expect(screen.getByText(REAUTH_ROW.sharedMemberHint)).toBeInTheDocument();
    expect(screen.queryByText(REAUTH_ROW.hint)).not.toBeInTheDocument();
  });

  it("an ADMIN on a member's per_user row: no door, and the sentence names whose sign-in it is", async () => {
    await mount(PER_USER, true, "admin@corp");
    expect(door()).not.toBeInTheDocument();
    expect(screen.getByText(REAUTH_ROW.notYoursHint("alice@corp"))).toBeInTheDocument();
    expect(screen.queryByText(REAUTH_ROW.hint)).not.toBeInTheDocument();
  });

  it("no Approve/Deny pair appears in the withheld-door cells either", async () => {
    await mount(PER_USER, true, "admin@corp");
    // The kind is not decidable by any tier; withholding the door must not fall
    // back to the decision pair the card renders for every other kind.
    expect(screen.queryByRole("button", { name: /^Approve$/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Deny$/ })).not.toBeInTheDocument();
  });
});

// #543 (design §5.10, packet 1 door-cards.html state 5): the card names which
// AWS provider the hold is for, and its door is THAT provider's — with two AWS
// providers, the claude-code default's sign-in would leave the hold unresolved.
describe("/approvals — a provider run's hold (#543)", () => {
  const { bedrock } = MODEL_PROVIDERS;
  const bedrockDev = { ...bedrock, id: "bedrock-dev", name: "Bedrock (dev)" };
  const HOLD = { mechanism: "bedrock_sso", credential_source: "per_user", owner: "bob@acme.example", provider: bedrock.id };

  async function mountAt(path: string, principal: string, scope: Record<string, unknown> = HOLD, resolved = true) {
    mockScope = scope;
    window.history.pushState({}, "", path);
    render(
      <WithDoor
        path={path}
        principal={principal}
        operator={path.startsWith("/admin")}
        operatorResolved={resolved}
        status={providerStatus([{ provider: bedrockDev, defaultFor: ["claude-code"] }, { provider: bedrock }])}
      >
        <ApprovalsScreen />
      </WithDoor>,
    );
    await screen.findByText(REAUTH_TITLE);
  }
  afterEach(() => window.history.pushState({}, "", "/"));

  it("the owner: the title, the provider line, and Sign in to AWS opens the hold's OWN provider's door", async () => {
    await mountAt("/approvals", "bob@acme.example");
    expect(screen.getByText(REAUTH_ROW.PROVIDER(bedrock.name))).toBeInTheDocument();
    expect(screen.getByText(REAUTH_ROW.hint)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: REAUTH_ROW.ariaLabel }));
    const dialog = await screen.findByRole("dialog", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE });
    expect(dialog).toHaveTextContent(`For ${bedrock.name}`);
    expect(await screen.findByTestId("fake-pane")).toHaveAttribute("data-model-provider", bedrock.id);
  });

  it("Admin view, another user's hold: whose sign-in it waits on, and no door", async () => {
    await mountAt("/admin/approvals", "ann@acme.example");
    expect(screen.getByText(REAUTH_ROW.PROVIDER(bedrock.name))).toBeInTheDocument();
    expect(screen.getByText(REAUTH_ROW.notYoursHint("bob@acme.example"))).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: REAUTH_ROW.ariaLabel })).toBeNull();
    expect(screen.queryByRole("button", { name: CONSOLE_VIEW.OPEN_IN_USER })).toBeNull();
  });

  it("Admin view, the admin's own hold: no door, and Open in user view", async () => {
    await mountAt("/admin/approvals", "ann@acme.example", { ...HOLD, owner: "ann@acme.example" });
    expect(screen.getByText(REAUTH_ROW.notYoursHint("ann@acme.example"))).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: REAUTH_ROW.ariaLabel })).toBeNull();
    expect(screen.getByRole("button", { name: CONSOLE_VIEW.OPEN_IN_USER })).toBeInTheDocument();
  });

  it("a hold whose provider is gone: the hint alone, no button that opens nothing", async () => {
    await mountAt("/approvals", "bob@acme.example", { ...HOLD, provider: "removed-provider" });
    expect(screen.getByText(REAUTH_ROW.hint)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: REAUTH_ROW.ariaLabel })).toBeNull();
    expect(screen.queryByRole("button", { name: CONSOLE_VIEW.OPEN_IN_USER })).toBeNull();
  });

  it("while /me is still resolving: no door and no Open in user view", async () => {
    await mountAt("/admin/approvals", "bob@acme.example", HOLD, false);
    expect(screen.queryByRole("button", { name: REAUTH_ROW.ariaLabel })).toBeNull();
    expect(screen.queryByRole("button", { name: CONSOLE_VIEW.OPEN_IN_USER })).toBeNull();
  });

  it("the title names the need, not a pause (#146 defect 3)", () => {
    expect(REAUTH_TITLE).toBe("AWS sign-in needed for this run");
  });
});

// M-7 (admin-member-modes-design.md §4.6, §6) — /admin/approvals carries no
// personal reauth door, even on the admin's own row: the same not-yours
// sentence a member's row gets, plus a switch link back to the door there.
// The shared lane is untouched.
describe("/admin/approvals — the reauth door in the admin view (M-7)", () => {
  const OWN = { mechanism: "bedrock_sso", credential_source: "per_user", owner: "admin@corp" };
  const switchLink = () => screen.queryByRole("button", { name: OPEN_IN_USER_VIEW });

  it("gives the admin's OWN row the not-yours sentence, no door, and the switch link", async () => {
    await mount(OWN, true, "admin@corp", "/admin/approvals");
    expect(door()).not.toBeInTheDocument();
    expect(screen.getByText(REAUTH_ROW.notYoursHint("admin@corp"))).toBeInTheDocument();
    expect(switchLink()).toBeInTheDocument();
  });

  it("gives a member's row no door and no switch link — it is not the admin's own", async () => {
    await mount(PER_USER, true, "admin@corp", "/admin/approvals");
    expect(door()).not.toBeInTheDocument();
    expect(screen.getByText(REAUTH_ROW.notYoursHint("alice@corp"))).toBeInTheDocument();
    expect(switchLink()).not.toBeInTheDocument();
  });

  it("leaves the shared lane's door alone — it stays an admin-mode control", async () => {
    await mount(SHARED, true, "admin@corp", "/admin/approvals");
    expect(door()).toBeInTheDocument();
    expect(switchLink()).not.toBeInTheDocument();
  });
});
