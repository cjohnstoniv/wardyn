/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, waitFor, fireEvent, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import type { ApprovalRequest, MeCapabilities } from "../../lib/types";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { REAUTH_ROW, REAUTH_TITLE } from "../wardyn/model-access-copy";
import { aheadByHours } from "../../lib/test-clock";

// HIGH fix (error handling): approve/deny were unguarded awaits. A rejected
// deny() must NOT leave the dialog's confirm button spinning forever, must
// surface a toast.error, and must keep the dialog open (so the operator can
// retry or cancel). These tests pin that behavior.

// Mock sonner so we can assert toast.error fires without rendering a real toaster.
const toastError = vi.fn();
const toastSuccess = vi.fn();
vi.mock("sonner", () => ({
  toast: { error: (...a: unknown[]) => toastError(...a), success: (...a: unknown[]) => toastSuccess(...a) },
}));

// Hand-rolled api fake. listApprovals returns one pending request of
// mockPendingKind (default "credential"; tests override it to prove the B3
// kind-aware decide gating), or none when mockPendingEmpty is set (the
// empty-state copy test); deny() rejects to simulate a 409 / network
// failure. `mock`-prefixed so vi.mock's hoisting can reference them.
let mockPendingKind: ApprovalRequest["kind"] = "credential";
let mockPendingEmpty = false;
let mockCancelledRow = false;
// F5-F4: every listApprovals call rejects while true — simulates a transient
// fetchAll() failure (Promise.all over the 5 states) without having to race
// individual calls within one Promise.all.
let mockFailAllLists = false;
// F5-F11: freezes every listApprovals call until resolved, so a test can
// inspect the DOM DURING an in-flight refresh (decide()'s silent fetchAll()
// vs. the old load()'s loading-skeleton flash).
let mockListDeferred: Promise<void> | null = null;
const denyMock = vi.fn();
const approveMock = vi.fn();
vi.mock("../../lib/api/approvals", () => {
  return {
    approvals: {
      listApprovals: async (state: string) => {
        if (mockListDeferred) await mockListDeferred;
        if (mockFailAllLists) return Promise.reject(new Error("503"));
        if (state === "PENDING" && !mockPendingEmpty) {
          return Promise.resolve([
            {
              id: "apr_1",
              run_id: "run_1",
              kind: mockPendingKind,
              // A credential_reauth row carries the lane and the SUBJECT whose
              // sign-in resolves it — the wire fields reauthAudience grades the
              // viewer against (approvals-reauth.test.tsx owns the four cells;
              // this case is the owner's).
              requested_scope:
                mockPendingKind === "credential_reauth"
                  ? { mechanism: "bedrock_sso", credential_source: "per_user", owner: "you@corp" }
                  : { host: "api.example.com" },
              state: "PENDING",
              requested_at: new Date().toISOString(),
            } satisfies ApprovalRequest,
          ]);
        }
        // B4: the archived row a terminal run's cascade writes — decided_by
        // "system", reason run_killed. Off by default so no existing case sees
        // a second row in the Decided tab.
        if (state === "CANCELLED" && mockCancelledRow) {
          return Promise.resolve([
            {
              id: "apr_2",
              run_id: "run_1",
              kind: "egress_domain",
              requested_scope: { host: "api.example.com" },
              state: "CANCELLED",
              requested_at: new Date().toISOString(),
              decided_at: new Date().toISOString(),
              decided_by: "system",
            } satisfies ApprovalRequest,
          ]);
        }
        return Promise.resolve([]);
      },
      deny: (...a: unknown[]) => denyMock(...a),
      approve: (...a: unknown[]) => approveMock(...a),
    },
  };
});
// GET /me/capabilities backs the member why-denied surface below. Default: an
// empty, unenforced set — which is 0.5's behaviour, so every pre-existing test
// in this file is unaffected by it.
let mockCaps: MeCapabilities = {
  grants: [],
  enforcement: {},
  session_groups: [],
  groups_snapshot_stale: false,
};
vi.mock("../../lib/api/permissions", () => ({
  permissions: { getMyCapabilities: () => Promise.resolve(mockCaps) },
}));

// RunContextRow (redesign) fetches the gated run to inline its context — and
// since B4 the card above reads its STATE off that same fetch, so the tests
// drive it from here.
let mockRunState = "RUNNING";
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: () =>
      Promise.resolve({
        id: "run_1",
        agent: "claude-code",
        repo: "acme/widgets",
        task: "Fix flaky auth tests",
        confinement_class: "CC2",
        state: mockRunState,
      }),
  },
}));

import { ApprovalsScreen } from "./approvals";
import { OperatorProvider, RoleProvider } from "../wardyn/operator-context";
import { DENIED } from "../../lib/permissions-copy";
import { APPROVAL, OPERATOR_ONLY_REASON, SECURITY_ONLY_REASON } from "../wardyn/copy";

// Every describe below assumes a LIVE run unless it says otherwise (B4 gates
// the decision pair on the run's state), and no archived CANCELLED row.
beforeEach(() => {
  mockRunState = "RUNNING";
  mockCancelledRow = false;
  mockFailAllLists = false;
  mockListDeferred = null;
});

describe("ApprovalsScreen — deny error handling", () => {
  beforeEach(() => {
    mockPendingKind = "credential";
    toastError.mockClear();
    toastSuccess.mockClear();
    denyMock.mockReset();
    approveMock.mockReset();
  });

  // Finding 4 — a mid-run AWS sign-in request is a DOOR, not a decision. The
  // Approve/Deny pair is REMOVED for the kind (a disabled pair would name a
  // role that could decide it, and none can: the server answers 409 to either
  // verb), the card is titled for what it asks rather than "Mint a scoped
  // credential", and it carries NO blast-radius claim — nothing is granted.
  it("renders the re-auth request as a door with its own title and no blast radius", async () => {
    mockPendingKind = "credential_reauth";
    render(
      // The row's OWNER — the one viewer whose own sign-in clears it.
      <OperatorProvider operator={false} securityOperator={false} principal="you@corp">
        <MemoryRouter>
          <ModelAccessProvider status={null} onRefresh={() => {}}>
            <ApprovalsScreen />
          </ModelAccessProvider>
        </MemoryRouter>
      </OperatorProvider>,
    );
    expect(await screen.findByText(REAUTH_TITLE)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Approve$/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Deny$/ })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: REAUTH_ROW.ariaLabel })).toBeInTheDocument();
    expect(screen.queryByText(/Mint a scoped credential/i)).not.toBeInTheDocument();
    // The hint renders; the "Blast radius:" LABEL over it does not (UX ruling
    // B3). The row grants nothing — it asks its owner to sign in
    // again to a credential the deployment already configured — so a
    // blast-radius label claimed a capability that does not exist.
    expect(screen.getByText(REAUTH_ROW.hint)).toBeInTheDocument();
    expect(screen.queryByText(/Blast radius/i)).not.toBeInTheDocument();
    mockPendingKind = "credential";
  });

  it("surfaces a toast and re-enables the confirm button when deny() rejects", async () => {
    denyMock.mockRejectedValue(new Error("HTTP 409: already decided"));
    render(
      <MemoryRouter>
        <ApprovalsScreen />
      </MemoryRouter>,
    );

    // Wait for the pending card to render, then open the deny dialog.
    const denyBtn = await screen.findByRole("button", { name: /deny/i });
    fireEvent.click(denyBtn);

    // The dialog requires a reason for a deny; fill it and confirm.
    const reason = await screen.findByPlaceholderText(/not on allowlist/i);
    fireEvent.change(reason, { target: { value: "bad scope" } });
    const confirm = await screen.findByRole("button", { name: /confirm deny/i });
    fireEvent.click(confirm);

    // toast.error must fire after the rejection.
    await waitFor(() => expect(toastError).toHaveBeenCalledTimes(1));
    // The dialog must stay open (confirm button still present) and re-enabled —
    // not stuck spinning forever.
    const confirmAfter = await screen.findByRole("button", { name: /confirm deny/i });
    await waitFor(() => expect(confirmAfter).not.toBeDisabled());
    expect(denyMock).toHaveBeenCalledWith("apr_1", "bad scope");
  });
});

// Role-aware console: the queue itself stays visible to a viewer — only
// deciding is out of reach (see http.go's requireOperator comment).
describe("ApprovalsScreen — role-aware decide buttons", () => {
  beforeEach(() => {
    mockPendingKind = "credential";
    mockPendingEmpty = false;
    denyMock.mockReset();
    approveMock.mockReset();
  });

  it("operator (today's default): Approve and Deny are enabled, no reason shown", async () => {
    render(
      <MemoryRouter>
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    expect(await screen.findByRole("button", { name: /^approve$/i })).not.toBeDisabled();
    expect(screen.getByRole("button", { name: /^deny$/i })).not.toBeDisabled();
    expect(screen.queryByText(/requires the admin role/i)).not.toBeInTheDocument();
  });

  it("viewer: Approve and Deny are disabled and the reason is visible — the queue itself still renders", async () => {
    render(
      <OperatorProvider operator={false} securityOperator={false}>
        <MemoryRouter>
          <ApprovalsScreen />
        </MemoryRouter>
      </OperatorProvider>,
    );
    // The request is still readable — a viewer isn't blinded. (scope has no
    // host/repos/key discriminator, so deriveTitle's generic-credential title
    // is the deterministic, exact text to assert on.)
    expect(await screen.findByText("Mint a scoped credential")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^approve$/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /^deny$/i })).toBeDisabled();
    // The decide-gate is useSecurityOperator (admin OR security admin), not
    // useOperator — OPERATOR_ONLY_REASON ("Requires the admin role.") is a
    // false statement here, since a security admin could also decide this.
    expect(screen.getByText(SECURITY_ONLY_REASON)).toBeInTheDocument();
    expect(screen.queryByText(OPERATOR_ONLY_REASON)).not.toBeInTheDocument();
    // Clicking a disabled button must never reach the API.
    expect(approveMock).not.toHaveBeenCalled();
    expect(denyMock).not.toHaveBeenCalled();
  });

  // B3: decide() (approvals.go) makes egress_domain owner-or-admin — a member
  // (operator:false) may decide one raised by a run they own; this list is
  // already scoped to the caller's own runs (handleListApprovals), so every
  // row rendered here is, by construction, one the member owns.
  it("member, egress_domain kind: Approve and Deny are LIVE (decide buttons live — prompt-v2)", async () => {
    mockPendingKind = "egress_domain";
    render(
      <OperatorProvider operator={false} securityOperator={false}>
        <MemoryRouter>
          <ApprovalsScreen />
        </MemoryRouter>
      </OperatorProvider>,
    );
    expect(await screen.findByRole("button", { name: /^approve$/i })).not.toBeDisabled();
    expect(screen.getByRole("button", { name: /^deny$/i })).not.toBeDisabled();
    expect(screen.queryByText(/requires the admin role/i)).not.toBeInTheDocument();
  });

  // Same member, but the row is credential/tool_call: stays admin-only
  // REGARDLESS of ownership (a self-approved credential mint / a re-opened
  // tool_call would self-authorize under the operator's own ceiling).
  it("member, tool_call kind: still disabled — kind, not just ownership, gates the decision", async () => {
    mockPendingKind = "tool_call";
    render(
      <OperatorProvider operator={false} securityOperator={false}>
        <MemoryRouter>
          <ApprovalsScreen />
        </MemoryRouter>
      </OperatorProvider>,
    );
    expect(await screen.findByRole("button", { name: /^approve$/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /^deny$/i })).toBeDisabled();
    expect(screen.getByText(SECURITY_ONLY_REASON)).toBeInTheDocument();
    expect(screen.queryByText(OPERATOR_ONLY_REASON)).not.toBeInTheDocument();
  });

  // 0.7 §B: a SECURITY ADMIN (operator:false, security_operator:true) decides
  // ANY kind on ANY run — authorizeMemberDecision early-returns for
  // isSecurityOperator (approvals.go:392) BEFORE both the kind check and the
  // egress_host capability leg. Gating this card on useOperator would refuse
  // them a decision the server would have honoured.
  it("security admin, tool_call kind: LIVE — the tier decides any kind, on any run", async () => {
    mockPendingKind = "tool_call";
    render(
      <OperatorProvider operator={false} securityOperator={true}>
        <MemoryRouter>
          <ApprovalsScreen />
        </MemoryRouter>
      </OperatorProvider>,
    );
    expect(await screen.findByRole("button", { name: /^approve$/i })).not.toBeDisabled();
    expect(screen.getByRole("button", { name: /^deny$/i })).not.toBeDisabled();
    expect(screen.queryByText(/requires the admin role/i)).not.toBeInTheDocument();
  });

  // The decide-gate chip's fallback text must not be hardcoded to
  // OPERATOR_ONLY_REASON: the predicate feeding it (kindDecidable =
  // canDecideApproval(useSecurityOperator(), kind)) is security-tier, not
  // operator-tier — "Requires the admin role." is false for a caller a
  // security admin could also satisfy. A security admin never reaches this
  // fallback at all (kindDecidable is unconditionally true for them); a
  // plain member/viewer denied a credential/tool_call decision reads the
  // honest tier sentence instead.
  it("a security admin never sees OPERATOR_ONLY_REASON; a member denied by kind sees the security-tier sentence", async () => {
    mockPendingKind = "tool_call";
    const { unmount } = render(
      <OperatorProvider operator={false} securityOperator={true}>
        <MemoryRouter>
          <ApprovalsScreen />
        </MemoryRouter>
      </OperatorProvider>,
    );
    await screen.findByRole("button", { name: /^approve$/i });
    expect(screen.queryByText(OPERATOR_ONLY_REASON)).not.toBeInTheDocument();
    expect(screen.queryByText(SECURITY_ONLY_REASON)).not.toBeInTheDocument();
    unmount();

    // The title's second clause: a plain member (not a security admin)
    // denied by kind sees the honest SECURITY_ONLY_REASON sentence, not the
    // false OPERATOR_ONLY_REASON fallback — an arm otherwise unexercised by
    // a test that only ever mounted securityOperator.
    render(
      <OperatorProvider operator={false} securityOperator={false}>
        <MemoryRouter>
          <ApprovalsScreen />
        </MemoryRouter>
      </OperatorProvider>,
    );
    await screen.findByRole("button", { name: /^approve$/i });
    expect(screen.getByText(SECURITY_ONLY_REASON)).toBeInTheDocument();
    expect(screen.queryByText(OPERATOR_ONLY_REASON)).not.toBeInTheDocument();
  });
});

// prompt-v2 point 5's exact empty-state string, member-only (admin keeps
// "You're all caught up" — the two roles see a genuinely different list, so
// the copy should say so rather than share admin's self-congratulatory tone).
describe("ApprovalsScreen — empty-state copy by role", () => {
  beforeEach(() => {
    mockPendingEmpty = true;
  });

  it("member: \"Approvals raised by your runs appear here.\"", async () => {
    render(
      <RoleProvider role="user">
        <MemoryRouter>
          <ApprovalsScreen />
        </MemoryRouter>
      </RoleProvider>,
    );
    expect(await screen.findByText("Approvals raised by your runs appear here.")).toBeInTheDocument();
    expect(screen.queryByText("You're all caught up")).not.toBeInTheDocument();
  });

  it("admin (and the fail-open default): unchanged", async () => {
    render(
      <MemoryRouter>
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    expect(await screen.findByText("You're all caught up")).toBeInTheDocument();
  });
});


// 0.6 pillar 2: the ONE member why-denied moment on this screen. A member may
// decide an egress_domain approval on their own run — unless `egress_host` is
// enforced and the host isn't granted to them (authorizeMemberDecision). The
// copy is read from the canon, never retyped.
describe("ApprovalsScreen — egress host not granted (member)", () => {
  beforeEach(() => {
    mockPendingKind = "egress_domain";
    mockPendingEmpty = false;
    denyMock.mockReset();
    approveMock.mockReset();
    mockCaps = { grants: [], enforcement: {}, session_groups: [], groups_snapshot_stale: false };
  });

  function renderMember() {
    return render(
      <OperatorProvider operator={false} securityOperator={false}>
        <RoleProvider role="user">
          <MemoryRouter>
            <ApprovalsScreen />
          </MemoryRouter>
        </RoleProvider>
      </OperatorProvider>,
    );
  }

  it("unenforced: the member decides their own egress approval, exactly as in 0.5", async () => {
    renderMember();
    expect(await screen.findByRole("button", { name: /^approve$/i })).not.toBeDisabled();
    expect(screen.queryByText(DENIED.APPROVE_CHIP)).not.toBeInTheDocument();
  });

  it("enforced + ungranted: both decisions disable, with the reason and the Always caveat", async () => {
    mockCaps = { ...mockCaps, enforcement: { egress_host: true } };
    renderMember();
    expect(await screen.findByRole("button", { name: /^approve$/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /^deny$/i })).toBeDisabled();
    expect(screen.getByText(DENIED.APPROVE_CHIP)).toBeInTheDocument();
    expect(screen.getByText(DENIED.APPROVE_BODY("api.example.com"))).toBeInTheDocument();
    expect(screen.getByText(DENIED.ALWAYS_STILL_ADMIN)).toBeInTheDocument();
  });

  it("enforced + granted by a *.suffix row: the decision is offered again", async () => {
    mockCaps = {
      ...mockCaps,
      enforcement: { egress_host: true },
      grants: [
        {
          id: "g1",
          subject_type: "user",
          subject: "bob@corp.example",
          capability: "egress_host",
          value: "*.example.com",
          effect: "allow",
          created_at: aheadByHours(-1),
        },
      ],
    };
    renderMember();
    expect(await screen.findByRole("button", { name: /^approve$/i })).not.toBeDisabled();
    expect(screen.queryByText(DENIED.APPROVE_CHIP)).not.toBeInTheDocument();
  });

  it("a stale group snapshot is named — but only once something is being enforced", async () => {
    mockCaps = { ...mockCaps, groups_snapshot_stale: true };
    const { unmount } = renderMember();
    await screen.findByRole("button", { name: /^approve$/i });
    expect(screen.queryByText(DENIED.STALE_GROUPS)).not.toBeInTheDocument();
    unmount();

    mockCaps = { ...mockCaps, enforcement: { egress_host: true }, groups_snapshot_stale: true };
    renderMember();
    expect(await screen.findByText(DENIED.STALE_GROUPS)).toBeInTheDocument();
  });
});

describe("ApprovalsScreen ?tab=", () => {
  it("?tab=decided opens the Decided tab (the audit chip's target); the default stays Pending", async () => {
    render(
      <MemoryRouter initialEntries={["/approvals?tab=decided"]}>
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    const decided = await screen.findByRole("tab", { name: /decided/i });
    expect(decided).toHaveAttribute("aria-selected", "true");
  });

  it("negative control: without ?tab= the Pending tab is selected", async () => {
    render(
      <MemoryRouter initialEntries={["/approvals"]}>
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    const decided = await screen.findByRole("tab", { name: /decided/i });
    expect(decided).toHaveAttribute("aria-selected", "false");
  });

  // X3-F14: ?tab=decided is read at mount but was never written back
  // (setSearchParams never called) — the Decided view couldn't be
  // reloaded/shared/reached by Back. audit.tsx's run_id filter does this
  // correctly (setSearchParams on every change); mirror it here.
  function LocationProbe() {
    const location = useLocation();
    return <div data-testid="location-search">{location.search}</div>;
  }

  it("clicking the Decided tab writes ?tab=decided back into the URL", async () => {
    const user = userEvent.setup();
    render(
      <MemoryRouter initialEntries={["/approvals"]}>
        <LocationProbe />
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    await screen.findByRole("button", { name: /^approve$/i });
    // Radix's Tabs.Trigger activates off the full pointer-event sequence —
    // a bare fireEvent.click never fires onValueChange (run-detail.test.tsx
    // precedent uses userEvent for the same reason).
    await user.click(screen.getByRole("tab", { name: /decided/i }));
    await waitFor(() => expect(screen.getByTestId("location-search")).toHaveTextContent("tab=decided"));
  });

  it("switching back to Pending clears the tab param", async () => {
    const user = userEvent.setup();
    render(
      <MemoryRouter initialEntries={["/approvals?tab=decided"]}>
        <LocationProbe />
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    await screen.findByRole("tab", { name: /decided/i, selected: true });
    await user.click(screen.getByRole("tab", { name: /pending/i }));
    await waitFor(() => expect(screen.getByTestId("location-search")).toHaveTextContent(""));
  });
});

// F5-F4 + F5-F11: without this, one transient failure on mount would leave
// status="error" forever while the 10s poll keeps silently filling
// pendingItems and the nav badge — the queue would look stuck on "Something
// went wrong" while the badge claims "1 pending". The tick heals status back
// to "ready", paused only while the FOREGROUND load is in flight (audit.tsx
// precedent) so a poll tick during the error state can still recover it.
describe("ApprovalsScreen — a poll tick heals a stuck error state", () => {
  // ticket: F5-F4
  beforeEach(() => {
    mockPendingKind = "credential";
  });

  it("mount fails, then a poll tick succeeds — the queue recovers from the error view", async () => {
    // Fake timers BEFORE render: usePoll's setInterval has to be the fake
    // one, or advancing time never reaches a tick (audit.test.tsx precedent).
    vi.useFakeTimers();
    try {
      mockFailAllLists = true;
      render(
        <MemoryRouter>
          <ApprovalsScreen />
        </MemoryRouter>,
      );
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();

      mockFailAllLists = false;
      await act(async () => {
        await vi.advanceTimersByTimeAsync(10_000);
      });
      expect(screen.getByRole("button", { name: /^approve$/i })).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: /retry/i })).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it("neg: the loading skeleton still shows on first mount, not the error view", async () => {
    render(
      <MemoryRouter>
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    // Before the initial fetch settles, neither the error view nor the queue
    // has rendered yet — the skeleton is up (this control would still pass
    // if `status` were initialised to "ready" without asserting the
    // skeleton is actually THERE, not just that the error view isn't).
    expect(screen.queryByRole("button", { name: /retry/i })).not.toBeInTheDocument();
    expect(document.querySelector(".animate-pulse")).not.toBeNull();
    await screen.findByRole("button", { name: /^approve$/i });
  });
});

describe("ApprovalsScreen — deciding refreshes silently, no skeleton flash", () => {
  // ticket: F5-F11
  beforeEach(() => {
    mockPendingKind = "credential";
  });

  it("decide() does not flip back to the loading skeleton mid-refresh", async () => {
    approveMock.mockResolvedValue(undefined);
    render(
      <MemoryRouter>
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    const approveBtn = await screen.findByRole("button", { name: /^approve$/i });
    fireEvent.click(approveBtn);
    // The trigger ("Approve") and the dialog's confirm button share the same
    // label — scope to the open dialog.
    const dialog = await screen.findByRole("dialog");
    // Freeze the refresh fetchAll() decide() kicks off, so the DOM mid-flight
    // is inspectable.
    let release: () => void = () => {};
    mockListDeferred = new Promise<void>((res) => {
      release = res;
    });
    fireEvent.click(within(dialog).getByRole("button", { name: /^approve$/i }));
    await waitFor(() => expect(approveMock).toHaveBeenCalled());
    // decide() itself has resolved; its silent refresh is frozen in flight.
    // load() would have flipped status to "loading" BEFORE even calling
    // fetchAll — the skeleton would already be showing. fetchAll() never
    // touches status on the way in, so it must not be.
    expect(document.querySelector(".animate-pulse")).toBeNull();
    release();
    await waitFor(() => expect(toastSuccess).toHaveBeenCalled());
    expect(document.querySelector(".animate-pulse")).toBeNull();
  });
});

// B4: a terminal run's PENDING approvals.
//
// Killing a run must not strand its approvals: left PENDING, they would sit
// for up to 24h (WARDYN_APPROVAL_EXPIRY_AFTER) while the console keeps
// offering Approve and Deny on them. Both buttons would be dead — the
// sandbox is torn down, the identity revoked, the server refuses — and a
// dead control on a governance surface reads as "this is still yours to
// answer". 0.7.2 cancels them server-side (types.ApprovalCancelled) and the
// screen stops asking.
describe("ApprovalsScreen — the run has ended", () => {
  // ticket: B4
  it("offers no decision on a KILLED run, and says what happened instead", async () => {
    mockRunState = "KILLED";
    render(
      <MemoryRouter>
        <ApprovalsScreen />
      </MemoryRouter>,
    );

    // The sentence arrives with the run fetch the context row already makes.
    expect(await screen.findByText(APPROVAL.CANCELLED_BODY)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^approve$/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /^deny$/i })).toBeNull();
  });

  // The negative control, and the reason the gate reads the RUN rather than
  // just the approval: a live run's queue is untouched.
  it("leaves the decision pair alone while the run is still going", async () => {
    render(
      <MemoryRouter>
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    expect(await screen.findByRole("button", { name: /^approve$/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^deny$/i })).toBeInTheDocument();
    expect(screen.queryByText(APPROVAL.CANCELLED_BODY)).toBeNull();
  });

  // The archived side. "Cancelled · by system · 3h ago" could mean anyone
  // withdrew it; the row has to say that NOTHING was decided — which is a
  // different fact from a denial, and the only one true here.
  it("a CANCELLED row in the Decided tab says nothing was approved and nothing denied", async () => {
    mockCancelledRow = true;
    render(
      <MemoryRouter initialEntries={["/approvals?tab=decided"]}>
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    expect(await screen.findByText(APPROVAL.CANCELLED_BODY)).toBeInTheDocument();
    expect(screen.getByText("by system", { exact: false })).toBeInTheDocument();
  });
});

// P0.3 (R3-F001/F108/F145).
//
// An egress_domain approval has always been HOST-WIDE — the proxy strips any
// port before keying the decision (approvalHostKey). Three surfaces relied on
// that quietly while "Reach api.example.com" read, to a human, like the one
// connection in front of them. 0.7.2 says it out loud; the port-scoped
// semantic is a 0.8 change at three places at once.
describe("ApprovalsScreen — an egress approval says it is host-wide", () => {
  // ticket: P0.3
  it("states the host-wide scope on an egress_domain card", async () => {
    mockPendingKind = "egress_domain";
    render(
      <MemoryRouter>
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    expect(await screen.findByText(APPROVAL.HOST_WIDE_NOTE)).toBeInTheDocument();
  });

  it("says nothing of the sort on a credential card, where it would be false", async () => {
    mockPendingKind = "credential";
    render(
      <MemoryRouter>
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    await screen.findByRole("button", { name: /^approve$/i });
    expect(screen.queryByText(APPROVAL.HOST_WIDE_NOTE)).toBeNull();
  });
});

// #638 — a run opened FROM /admin/approvals stays in the Admin view: the plain
// /runs/:id path is the User view's (the owner cockpit on a "url"-access
// install, a refusal for an admin-only token). Both run links on the screen —
// the pending card's "Open run" and the decided row's run id — go through
// runPath; the User-view mount keeps /runs/:id.
describe("ApprovalsScreen — run links stay in the view they are opened from (#638)", () => {
  it("/admin/approvals: the pending card's Open run goes to /admin/runs/:id", async () => {
    render(
      <MemoryRouter initialEntries={["/admin/approvals"]}>
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    const open = await screen.findByRole("link", { name: /open run/i });
    expect(open).toHaveAttribute("href", "/admin/runs/run_1");
  });

  it("/admin/approvals: the decided row's run link goes to /admin/runs/:id", async () => {
    mockCancelledRow = true;
    render(
      <MemoryRouter initialEntries={["/admin/approvals?tab=decided"]}>
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    const link = await screen.findByRole("link", { name: "run_1" });
    expect(link).toHaveAttribute("href", "/admin/runs/run_1");
  });

  it("negative control: /approvals keeps both links on /runs/:id", async () => {
    mockCancelledRow = true;
    const user = userEvent.setup();
    render(
      <MemoryRouter initialEntries={["/approvals"]}>
        <ApprovalsScreen />
      </MemoryRouter>,
    );
    const open = await screen.findByRole("link", { name: /open run/i });
    expect(open).toHaveAttribute("href", "/runs/run_1");
    await user.click(screen.getByRole("tab", { name: /decided/i }));
    const link = await screen.findByRole("link", { name: "run_1" });
    expect(link).toHaveAttribute("href", "/runs/run_1");
  });
});
