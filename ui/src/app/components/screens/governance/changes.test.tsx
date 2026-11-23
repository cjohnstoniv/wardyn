/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Changes tab (0.8.6 four-eyes, mock packet M3). Expected strings come from CHANGES, so these fail the moment a
// rendered string stops coming from the approved canon. Pinned here because they are decisions: a row opens the
// change on its own page, and a 202 at a write site is "submitted", never "saved". The page itself is pinned in
// change-review-page.test.tsx.
import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";

const poolGetMock = vi.fn();
const poolMembersMock = vi.fn();
vi.mock("../../../lib/api/runner-pools", () => ({
  runnerPools: { get: (...a: unknown[]) => poolGetMock(...a), members: (...a: unknown[]) => poolMembersMock(...a) },
}));

import { CHANGE_REVIEW } from "../../../lib/change-review-copy";
import { CHANGES, DELIVERY } from "../../../lib/governance-copy";
import type { GovernanceChange } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";
import { aheadByHours } from "../../../lib/test-clock";
import { ChangesTab } from "./changes";
import { SubmittedNote } from "./submitted-note";

function change(over: Partial<GovernanceChange> = {}): GovernanceChange {
  return {
    id: "c1",
    target_kind: "governance_unit",
    op: "update",
    target_key: "team-a",
    state: "pending",
    proposed_by: "ana@example.com",
    proposed_at: aheadByHours(-2),
    expires_at: aheadByHours(20),
    diff: {
      before: { ceiling: { allowed_domains: ["*.corp", "git.x"] }, name: "team-a" },
      after: { ceiling: { allowed_domains: ["*.corp"] }, name: "team-a" },
      changed: ["ceiling.allowed_domains"],
    },
    ...over,
  };
}

function Where() {
  const l = useLocation();
  return <p data-testid="where">{l.pathname + l.search}</p>;
}

function renderTab(changes: GovernanceChange[], principal = "bo@example.com", onChanged = vi.fn()) {
  render(
    <MemoryRouter>
      <OperatorProvider operator={false} principal={principal}>
        <ChangesTab changes={changes} loadError={null} onChanged={onChanged} />
        <Where />
      </OperatorProvider>
    </MemoryRouter>,
  );
  return { onChanged };
}

describe("the Changes tab", () => {
  it("lists a pending change as Kind · Op over its target, proposer and times", () => {
    renderTab([change()]);
    expect(screen.getByRole("columnheader", { name: CHANGES.COL_TARGET })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Profile · Edit" })).toBeInTheDocument();
    expect(screen.getByText("team-a")).toBeInTheDocument();
    expect(screen.getByText("ana@example.com")).toBeInTheDocument();
    expect(screen.getByText("2h ago")).toBeInTheDocument();
    expect(screen.getByText("in 20h")).toBeInTheDocument();
  });

  it("adds an Effect column after Change with the verdict and risk, and nothing on a request", () => {
    const widening = change({
      id: "w1",
      diff: {
        ...change().diff,
        review: {
          verdict: "widens",
          widens: [{ field: "allowed_domains", entries: ["settle.partner-a.example"] }],
          risk: { before: { overall: "low", items: [] }, after: { overall: "medium", items: [] } },
        },
      },
    });
    const held = change({
      id: "h1",
      target_key: "team-h",
      diff: { ...change().diff, review: { verdict: "narrows_held", held_because: ["lock_removed", "contact"] } },
    });
    const request = change({
      id: "r1",
      target_kind: "governance_elevation",
      op: "request",
      target_key: "el1",
      diff: { after: { unit_name: "Payments", subject: "kai", duration: "1h0m0s" }, changed: [] },
    });
    renderTab([widening, held, change({ id: "u1" }), request]);
    const heads = screen.getAllByRole("columnheader").map((h) => h.textContent);
    expect(heads.indexOf(CHANGE_REVIEW.COL_EFFECT)).toBe(heads.indexOf(CHANGES.COL_CHANGE) + 1);
    const rows = screen.getAllByRole("row").slice(1);
    const effect = (i: number) => within(rows[i]).getAllByRole("cell")[1];
    expect(within(effect(0)).getByText(CHANGE_REVIEW.VERDICT.widens)).toBeInTheDocument();
    expect(within(effect(0)).getByText("Risk Low → Medium")).toBeInTheDocument();
    expect(within(effect(1)).getByText(CHANGE_REVIEW.VERDICT.narrows_held(CHANGE_REVIEW.HELD_BECAUSE.lock_removed))).toBeInTheDocument();
    expect(within(effect(2)).getByText(CHANGE_REVIEW.VERDICT.not_checked)).toBeInTheDocument();
    expect(effect(3).textContent).toBe("");
  });

  it("reads a held organization runner removal as a sentence, with who decides it", async () => {
    poolGetMock.mockResolvedValue({ id: "p1", name: "Platform services" });
    poolMembersMock.mockResolvedValue([{ kind: "self_hosted", id: "r-east", name: "k8s-east" }]);
    const state = (ids?: string[]) => ({ policy_present: true, subjects: [], remote_runner_ids: [], ...(ids ? { runner_ids: ids } : {}) });
    renderTab([change({ target_kind: "runner_pool_use_policy", op: "set", target_key: "p1", diff: { before: state(["r-east"]), after: state(), changed: ["runner_ids"] } })]);
    expect(await screen.findByRole("button", { name: "Remove organization runner k8s-east from Platform services" })).toBeInTheDocument();
    expect(screen.getByText("A super admin decides this.")).toBeInTheDocument();
  });

  it("keeps a use policy change under its kind and op", () => {
    const state = { policy_present: true, subjects: [], remote_runner_ids: [] };
    renderTab([change({ target_kind: "runner_pool_use_policy", op: "set", target_key: "p1", diff: { before: state, after: state, changed: [] } })]);
    expect(screen.getByRole("button", { name: "Runner pool use policy · Set" })).toBeInTheDocument();
    expect(screen.queryByText("A super admin decides this.")).not.toBeInTheDocument();
  });

  it("lists a credential delivery write as Runner credentials, under its natural key", () => {
    renderTab([change({ target_kind: "local_credential_delivery", op: "replace", target_key: "local_credential_delivery", diff: { changed: [] } })]);
    expect(screen.getByRole("button", { name: `${DELIVERY.CHANGE_KIND} · ${CHANGES.OP.replace}` })).toBeInTheDocument();
    expect(screen.getByText("local_credential_delivery")).toBeInTheDocument();
  });

  it("names a kind and an op this console predates by their raw names", () => {
    renderTab([change({ target_kind: "future_kind", op: "merge" })]);
    expect(screen.getByRole("button", { name: "future_kind · merge" })).toBeInTheDocument();
  });

  it("names a profile by the name in the server's diff, since its key is an id", () => {
    renderTab([change({ target_key: "7b1f0c52-0000-4000-8000-000000000001" })]);
    expect(screen.getByText("team-a")).toBeInTheDocument();
    expect(screen.queryByText("7b1f0c52-0000-4000-8000-000000000001")).toBeNull();
  });

  // Q6: a runner trust change's key is the runner's id; the server's diff carries its name on both sides.
  it("lists a runner trust change as Runner trust class · Edit over the runner's name, not its id", () => {
    const id = "3f9c1c2e-7a41-4d0b-9e6a-5b2f0d8c1a77";
    renderTab([
      change({
        target_kind: "runner_trust",
        target_key: id,
        diff: { before: { name: "dana-laptop", trust_class: "personal" }, after: { name: "dana-laptop", trust_class: "managed_device" }, changed: ["trust_class"] },
      }),
    ]);
    expect(screen.getByRole("button", { name: "Runner trust class · Edit" })).toBeInTheDocument();
    expect(screen.getByText("dana-laptop")).toBeInTheDocument();
    expect(screen.queryByText(id)).toBeNull();
  });

  it("falls back to the runner trust change's key when the diff carries no name", () => {
    renderTab([change({ target_kind: "runner_trust", target_key: "r-old", diff: { after: { trust_class: "managed_device" }, changed: ["trust_class"] } })]);
    expect(screen.getByText("r-old")).toBeInTheDocument();
  });

  it("keeps the key as the target for a kind whose key is already natural", () => {
    renderTab([change({ target_kind: "governance_membership", op: "upsert", target_key: "group:eng", diff: { changed: [] } })]);
    expect(screen.getByText("group:eng")).toBeInTheDocument();
  });

  // #2907: the lead and the empty text were one sentence said twice; the empty block says it once, and its
  // one door is the audit trail, where the decided changes are.
  it("with nothing pending shows one empty block, and its button opens the audit trail", async () => {
    renderTab([]);
    const block = screen.getByTestId("changes-empty");
    expect(within(block).getByText(CHANGES.EMPTY_TITLE)).toBeInTheDocument();
    expect(within(block).getByText(CHANGES.EMPTY_BODY)).toBeInTheDocument();
    expect(screen.queryByText(CHANGES.LEAD)).toBeNull();
    expect(screen.queryByRole("heading", { name: CHANGES.TAB })).toBeNull();
    await userEvent.click(within(block).getByTestId("changes-empty-audit"));
    expect(screen.getByTestId("where")).toHaveTextContent("/admin/audit");
  });

  it("with changes waiting the list replaces the empty block and keeps the lead", () => {
    renderTab([change()]);
    expect(screen.queryByTestId("changes-empty")).toBeNull();
    expect(screen.getByText(CHANGES.LEAD)).toBeInTheDocument();
  });

  // The server cuts the queue at 500 changes the viewer may see and says so in its header; exactly 500 rows
  // without it is the whole queue.
  it("says the list was cut only when the server says so, never from 500 rows alone", () => {
    const many = Array.from({ length: 500 }, (_, i) => change({ id: `c${i}`, target_key: `team-${i}` }));
    const { unmount } = render(
      <MemoryRouter>
        <OperatorProvider operator={false} principal="bo@example.com">
          <ChangesTab changes={many} loadError={null} onChanged={vi.fn()} />
        </OperatorProvider>
      </MemoryRouter>,
    );
    expect(screen.queryByText(CHANGES.TRUNCATED)).toBeNull();
    unmount();
    renderTab(Object.assign([...many], { truncated: true }));
    expect(screen.getByText(CHANGES.TRUNCATED)).toBeInTheDocument();
  });
});

describe("opening a change", () => {
  it("a click on a row opens the change on its own page, with no dialog", async () => {
    renderTab([change({ id: "c1" }), change({ id: "c2", target_key: "team-b" })]);
    await userEvent.click(screen.getAllByRole("row")[2]);
    expect(screen.getByTestId("where")).toHaveTextContent("/admin/governance/changes/c2");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("a click on the label opens the change too", async () => {
    renderTab([change()]);
    await userEvent.click(screen.getByRole("button", { name: "Profile · Edit" }));
    expect(screen.getByTestId("where")).toHaveTextContent("/admin/governance/changes/c1");
  });

  it("Enter on a focused row opens it", async () => {
    renderTab([change()]);
    const row = screen.getAllByRole("row")[1];
    row.focus();
    await userEvent.keyboard("{Enter}");
    expect(screen.getByTestId("where")).toHaveTextContent("/admin/governance/changes/c1");
  });
});

describe("the submitted-for-approval note", () => {
  it("says nothing has changed yet, and links to the Changes tab", () => {
    render(
      <MemoryRouter>
        <SubmittedNote />
      </MemoryRouter>,
    );
    const note = screen.getByRole("status");
    expect(within(note).getByText(CHANGES.SUBMITTED_TITLE)).toBeInTheDocument();
    expect(within(note).getByText(CHANGES.SUBMITTED_BODY)).toBeInTheDocument();
    expect(within(note).getByRole("link", { name: CHANGES.SUBMITTED_LINK })).toHaveAttribute(
      "href",
      "/admin/governance?tab=changes",
    );
  });

  it("renders without a router, minus the link", () => {
    render(<SubmittedNote />);
    expect(screen.getByText(CHANGES.SUBMITTED_TITLE)).toBeInTheDocument();
    expect(screen.queryByRole("link")).toBeNull();
  });
});
