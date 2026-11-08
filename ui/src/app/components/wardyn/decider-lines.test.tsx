/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest } from "../../lib/types";
import { DECIDERS_INBOX } from "./copy/deciders-inbox";
import { OperatorProvider } from "./operator-context";

const getRun = vi.fn();
vi.mock("../../lib/api/runs", () => ({ runs: { getRun: (...a: unknown[]) => getRun(...a) } }));

import {
  DecidedByLine,
  RequestedByRow,
  SecondPersonLine,
  basisSentence,
  deciderPhrase,
  joinOr,
} from "./decider-lines";

const row = (extra: Partial<ApprovalRequest>): ApprovalRequest => ({
  id: "ap_1",
  run_id: "run_1",
  kind: "egress_domain",
  requested_scope: { host: "registry.npmjs.org" },
  state: "PENDING",
  requested_at: new Date().toISOString(),
  ...extra,
});

describe("joinOr", () => {
  it("joins one, two and three entries with no Oxford comma", () => {
    expect(joinOr(["a"])).toBe("a");
    expect(joinOr(["a", "b"])).toBe("a or b");
    expect(joinOr(["a", "b", "c"])).toBe("a, b or c");
    expect(joinOr(["a", "b", "c", "d"])).toBe("a, b, c or d");
  });
});

describe("deciderPhrase", () => {
  it("says you on the owner's own card and the run's owner on a decider's", () => {
    expect(deciderPhrase("owner", true)).toBe("you");
    expect(deciderPhrase("owner", false)).toBe("the run's owner");
  });
  it("names the other entries the same on either card", () => {
    for (const own of [true, false]) {
      expect(deciderPhrase("unit_admins", own)).toBe("admins of this unit");
      expect(deciderPhrase("group:eng-leads", own)).toBe("the eng-leads group");
      expect(deciderPhrase("security", own)).toBe("a security admin");
    }
  });
  it("passes an entry the console does not know through as sent", () => {
    expect(deciderPhrase("something_new", true)).toBe("something_new");
  });
});

const asViewer = (item: ApprovalRequest, securityOperator: boolean) =>
  render(
    <OperatorProvider operator={false} securityOperator={securityOperator}>
      <DecidedByLine item={item} />
    </OperatorProvider>,
  );

describe("DecidedByLine", () => {
  it("reads the entries in the server's order on a decider's card", () => {
    asViewer(
      row({ requested_by: "dana@acme.example", deciders: ["owner", "unit_admins", "group:eng-leads", "security"] }),
      false,
    );
    const line = screen.getByText(DECIDERS_INBOX.DECIDED_BY_LABEL).closest("p");
    expect(line).toHaveTextContent(
      "Decided by: the run's owner, admins of this unit, the eng-leads group or a security admin",
    );
  });
  it("reads you for the owner entry on an own card", () => {
    asViewer(row({ deciders: ["owner", "security"] }), false);
    expect(screen.getByText(DECIDERS_INBOX.DECIDED_BY_LABEL).closest("p")).toHaveTextContent("Decided by: you or a security admin");
  });
  it("reads the run's owner for a security operator, whose Pending list carries other people's runs", () => {
    asViewer(row({ deciders: ["owner", "security"] }), true);
    expect(screen.getByText(DECIDERS_INBOX.DECIDED_BY_LABEL).closest("p")).toHaveTextContent(
      "Decided by: the run's owner or a security admin",
    );
  });
  it("draws nothing without deciders", () => {
    const { container } = asViewer(row({}), false);
    expect(container).toBeEmptyDOMElement();
  });
});

describe("SecondPersonLine", () => {
  it("draws the second-person sentence only for second_human", () => {
    const { rerender } = render(<SecondPersonLine item={row({ second_human: true })} />);
    expect(screen.getByText(DECIDERS_INBOX.SECOND_PERSON)).toBeInTheDocument();
    rerender(<SecondPersonLine item={row({})} />);
    expect(screen.queryByText(DECIDERS_INBOX.SECOND_PERSON)).not.toBeInTheDocument();
  });
});

describe("RequestedByRow", () => {
  it("names who asked, the first 8 characters of the run id, and says the run can't be opened; no link, no run read", () => {
    getRun.mockClear();
    render(
      <MemoryRouter>
        <RequestedByRow who="dana.reyes@acme.example" runId="4f2a91c0-1111-2222-3333-444455556666" />
      </MemoryRouter>,
    );
    expect(screen.getByText("dana.reyes@acme.example")).toBeInTheDocument();
    expect(screen.getByText("4f2a91c0")).toHaveClass("font-mono");
    expect(screen.queryByText(/4f2a91c0-1111/)).not.toBeInTheDocument();
    expect(screen.getByText(DECIDERS_INBOX.CANT_OPEN_RUN)).toBeInTheDocument();
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
    expect(getRun).not.toHaveBeenCalled();
  });
  it("leaves the run part out when no run id is given", () => {
    render(
      <MemoryRouter>
        <RequestedByRow who="sam@acme.example" />
      </MemoryRouter>,
    );
    expect(screen.getByText("sam@acme.example")).toBeInTheDocument();
    expect(screen.queryByText(DECIDERS_INBOX.CANT_OPEN_RUN)).not.toBeInTheDocument();
  });
});

describe("basisSentence", () => {
  it("has one sentence per basis", () => {
    expect(basisSentence("owner")).toBe("Decided as the run's owner");
    expect(basisSentence("group:eng-leads")).toBe("Decided as a member of the eng-leads group");
    expect(basisSentence("security")).toBe("Decided as a security admin");
    expect(basisSentence("policy")).toBe("Decided by an approval rule, not a person");
    expect(basisSentence("admin_token")).toBe("Decided with the admin token");
  });
  it("names the unit for unit_admin:<id> when the name is known, else falls back", () => {
    expect(basisSentence("unit_admin:u1", { u1: "Payments" })).toBe("Decided as an admin of Payments");
    expect(basisSentence("unit_admin:u1", { u2: "Other" })).toBe("Decided as a unit admin");
    expect(basisSentence("unit_admin:u1")).toBe("Decided as a unit admin");
  });
  it("says nothing for an absent or unknown basis", () => {
    expect(basisSentence(undefined)).toBeNull();
    expect(basisSentence("")).toBeNull();
    expect(basisSentence("mystery")).toBeNull();
  });
});
