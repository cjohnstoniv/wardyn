/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The change review sections: what each verdict, risk and impact result reads as, and the raw diff last and closed.
import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { CHANGE_REVIEW } from "../../../lib/change-review-copy";
import type { ChangeImpact, ChangeReview, GovernanceChange, HeldBecause } from "../../../lib/types";
import { aheadByHours } from "../../../lib/test-clock";
import { ImpactSection, RawDiffSection, RiskChip, RiskSection, VerdictBlock, VerdictChip } from "./change-review-sections";

function change(review?: ChangeReview, over: Partial<GovernanceChange> = {}): GovernanceChange {
  return {
    id: "c1",
    target_kind: "governance_unit",
    op: "update",
    target_key: "u1",
    state: "pending",
    proposed_by: "ana@example.com",
    proposed_at: aheadByHours(-2),
    expires_at: aheadByHours(20),
    diff: {
      before: { name: "Payments", ceiling: { allowed_domains: ["a.example"] } },
      after: { name: "Payments", ceiling: { allowed_domains: ["a.example", "b.example"] } },
      changed: ["ceiling.allowed_domains"],
      review,
    },
    ...over,
  };
}

const item = (field: string, risk_level: "low" | "medium" | "high") => ({ field, value: "v", risk_level, rationale: `why ${field}` });

describe("VerdictChip", () => {
  it("reads each verdict", () => {
    const { rerender } = render(<VerdictChip review={{ verdict: "widens" }} />);
    expect(screen.getByText(CHANGE_REVIEW.VERDICT.widens)).toBeInTheDocument();
    rerender(<VerdictChip review={{ verdict: "not_checked" }} />);
    expect(screen.getByText(CHANGE_REVIEW.VERDICT.not_checked)).toBeInTheDocument();
    rerender(<VerdictChip review={undefined} />);
    expect(screen.getByText(CHANGE_REVIEW.VERDICT.not_checked)).toBeInTheDocument();
  });

  it.each(["contact", "lock_removed", "rename", "legacy_shift", "first_allowlist"] as HeldBecause[])(
    "names the held reason %s",
    (r) => {
      render(<VerdictChip review={{ verdict: "narrows_held", held_because: [r, "contact"] }} />);
      expect(screen.getByText(CHANGE_REVIEW.VERDICT.narrows_held(CHANGE_REVIEW.HELD_BECAUSE[r]))).toBeInTheDocument();
    },
  );
});

describe("RiskChip", () => {
  it("shows before and after", () => {
    render(<RiskChip risk={{ before: { overall: "low", items: [] }, after: { overall: "high", items: [] } }} />);
    expect(screen.getByText("Risk Low → High")).toBeInTheDocument();
  });
  it("shows only the grade after when there is no before", () => {
    render(<RiskChip risk={{ before: null, after: { overall: "medium", items: [] } }} />);
    expect(screen.getByText("Risk Medium")).toBeInTheDocument();
  });
});

describe("VerdictBlock", () => {
  it("shows the widened field with its path, the entries, the sentence and the rules note", () => {
    render(<VerdictBlock change={change({ verdict: "widens", widens: [{ field: "allowed_domains", entries: ["b.example"] }] })} />);
    expect(screen.getByText("Allowed hosts")).toBeInTheDocument();
    expect(screen.getByText("allowed_domains")).toBeInTheDocument();
    expect(screen.getByText("b.example")).toBeInTheDocument();
    expect(screen.getByText("This lets runs under Payments reach 1 new host.")).toBeInTheDocument();
    expect(screen.getByText(CHANGE_REVIEW.RULES_NOTE)).toBeInTheDocument();
  });

  it("names every reason of a held narrowing and writes no sentence", () => {
    render(<VerdictBlock change={change({ verdict: "narrows_held", held_because: ["rename", "first_allowlist"] })} />);
    expect(screen.getByText(CHANGE_REVIEW.VERDICT.narrows_held(CHANGE_REVIEW.HELD_BECAUSE.rename))).toBeInTheDocument();
    expect(screen.getByText(CHANGE_REVIEW.VERDICT.narrows_held(CHANGE_REVIEW.HELD_BECAUSE.first_allowlist))).toBeInTheDocument();
    expect(screen.queryByText(/This lets runs/)).toBeNull();
  });

  it("says a change with no narrowing result is treated as widening, never that its kind has no check", () => {
    // A legacy profile change: a kind that has a check, stored before the review was written.
    render(<VerdictBlock change={change(undefined)} />);
    const note = screen.getByText(CHANGE_REVIEW.NOT_CHECKED_NOTE);
    expect(note).toBeInTheDocument();
    expect(note.textContent).not.toMatch(/kind|Profile/);
  });

  // Each widening field with the entries the server sends for it, and the words that say which way they go.
  it.each([
    ["allowed_domains", ["settle.partner-a.example"], CHANGE_REVIEW.ADDS],
    ["allowed_methods", ["DELETE"], CHANGE_REVIEW.ADDS],
    ["eligible_grants", ["github"], CHANGE_REVIEW.ADDS],
    ["denied_domains", ["blocked.example"], CHANGE_REVIEW.REMOVES_FROM_BLOCKED],
    ["withheld_grants", ["github"], CHANGE_REVIEW.NO_LONGER_WITHHELD],
    ["members", ["kai@example.com"], CHANGE_REVIEW.NO_LONGER_BOUND],
    ["vcs", ["github"], CHANGE_REVIEW.LOOSENED],
    ["member_cap", ["cpu_millis"], CHANGE_REVIEW.RAISED],
    ["owning_unit", ["Payments"], CHANGE_REVIEW.MOVES],
    ["a_field_from_a_newer_server", ["x"], CHANGE_REVIEW.AFFECTED],
  ])("reads the entries of %s with the words for their direction", (field, entries, words) => {
    render(<VerdictBlock change={change({ verdict: "widens", widens: [{ field, entries }] })} />);
    const line = screen.getByText(entries[0]).parentElement as HTMLElement;
    expect(line.firstChild?.textContent?.trim()).toBe(words);
    // Only the allow-type fields add anything.
    if (words !== CHANGE_REVIEW.ADDS) expect(line.textContent).not.toMatch(/^Adds/);
  });
});

const person = (i: number) => ({
  membership_id: `m${i}`,
  subject_type: "user" as const,
  subject: `person${i}@example.com`,
  from_unit_id: "u0",
  from_unit_name: "Old",
  unit_id: i < 3 ? "u1" : "u2",
  unit_name: i < 3 ? "Payments" : "Data",
  changes: [{ knob: "allowed_domains", before: ["a"], after: ["a", "b"] }],
});

function impact(over: Partial<ChangeImpact>): ChangeImpact {
  return { computed: "tree", as_of: aheadByHours(0), ...over };
}

describe("ImpactSection", () => {
  it("counts people, units and live runs, then lists the first three people and counts the rest", () => {
    render(
      <ImpactSection
        change={change()}
        impact={impact({
          people: [1, 2, 3, 4, 5].map(person),
          live_runs: [
            { id: "11111111-aaaa", owner: "ana", started_at: aheadByHours(-1), title: "Nightly build" },
            { id: "22222222-bbbb", owner: "bo", started_at: aheadByHours(-3) },
          ],
          live_runs_total: 7,
        })}
      />,
    );
    const tree = screen.getByTestId("change-impact-tree");
    expect(within(tree).getByText(CHANGE_REVIEW.PEOPLE).previousSibling).toHaveTextContent("5");
    expect(within(tree).getByText(CHANGE_REVIEW.UNITS).previousSibling).toHaveTextContent("2");
    expect(within(tree).getByText(CHANGE_REVIEW.LIVE_RUNS_WALLS).previousSibling).toHaveTextContent("7");
    expect(tree).toHaveTextContent(CHANGE_REVIEW.NOT_IN_REPORT);
    const people = screen.getByTestId("change-impact-people");
    expect(within(people).getByText("person3@example.com")).toBeInTheDocument();
    expect(within(people).queryByText("person4@example.com")).toBeNull();
    expect(within(people).getAllByText(/Allowed hosts: a → a, b/)).toHaveLength(3);
    expect(within(people).getByText(CHANGE_REVIEW.MORE(2))).toBeInTheDocument();
    const runs = screen.getByTestId("change-impact-runs");
    expect(within(runs).getByText("Nightly build")).toBeInTheDocument();
    expect(within(runs).getByText("22222222")).toBeInTheDocument();
    expect(within(runs).getByText(CHANGE_REVIEW.MORE(5))).toBeInTheDocument();
  });

  it("shows the boundary report's counts and rows", () => {
    render(
      <ImpactSection
        change={change(undefined, { target_kind: "governance_boundary" })}
        impact={impact({
          computed: "boundary",
          boundary: {
            target: "boundary",
            deployment: [],
            units: [{ id: "u1", name: "Payments", changes: [{ knob: "allowed_domains", before: ["a"], after: [] }] }],
            stored_policies: [],
            memberships: [],
            provider_rows: [],
          },
        })}
      />,
    );
    const boundary = screen.getByTestId("change-impact-boundary");
    expect(within(boundary).getByText("Payments")).toBeInTheDocument();
    expect(within(boundary).getByText("Allowed hosts: a → Not set")).toBeInTheDocument();
  });

  it("an empty boundary report says no existing policies would change", () => {
    render(
      <ImpactSection
        change={change(undefined, { target_kind: "governance_boundary" })}
        impact={impact({
          computed: "boundary",
          boundary: { target: "boundary", deployment: [], units: [], stored_policies: [], memberships: [], provider_rows: [] },
        })}
      />,
    );
    const boundary = screen.getByTestId("change-impact-boundary");
    expect(within(boundary).getByText("No existing policies would change")).toBeInTheDocument();
    expect(within(boundary).queryByText("No change")).toBeNull();
  });

  it.each([
    ["kind", CHANGE_REVIEW.IMPACT_NOT_COMPUTED],
    ["stale", CHANGE_REVIEW.IMPACT_STALE("u1")],
    ["refused", CHANGE_REVIEW.IMPACT_REFUSED],
  ] as const)("says why nothing is computed: %s", (why, text) => {
    render(<ImpactSection change={change(undefined, { target_kind: "role_mapping" })} impact={impact({ computed: "none", not_computed: why })} />);
    expect(screen.getByText(text)).toBeInTheDocument();
  });

  // The server computes some operations of a kind and not others, so the text speaks of this change, never of the kind.
  it.each([
    ["a membership add", { target_kind: "governance_membership", op: "upsert" }],
    ["a profile deletion", { target_kind: "governance_unit", op: "delete" }],
  ])("says impact is not available for %s, without ruling out its kind", (_name, over) => {
    render(<ImpactSection change={change(undefined, over)} impact={impact({ computed: "none", not_computed: "kind" })} />);
    const note = screen.getByText(CHANGE_REVIEW.IMPACT_NOT_COMPUTED);
    expect(note.textContent).not.toMatch(/kind|Assignment|Profile|yet/);
  });

  it("says the same when a computed answer carries nothing to show", () => {
    render(<ImpactSection change={change(undefined, { target_kind: "governance_boundary" })} impact={impact({ computed: "boundary" })} />);
    expect(screen.getByText(CHANGE_REVIEW.IMPACT_NOT_COMPUTED)).toBeInTheDocument();
  });

  it("still shows the counts of a membership move, which the server computes", () => {
    render(
      <ImpactSection
        change={change(undefined, { target_kind: "governance_membership", op: "move" })}
        impact={impact({ people: [person(1), person(2)], live_runs_total: 4 })}
      />,
    );
    const tree = screen.getByTestId("change-impact-tree");
    expect(within(tree).getByText(CHANGE_REVIEW.PEOPLE).previousSibling).toHaveTextContent("2");
    expect(within(tree).getByText(CHANGE_REVIEW.LIVE_RUNS_WALLS).previousSibling).toHaveTextContent("4");
    expect(screen.queryByText(CHANGE_REVIEW.IMPACT_NOT_COMPUTED)).toBeNull();
  });

  it("shows a failed fetch for this section only, and nothing while it loads", () => {
    const { rerender, container } = render(<ImpactSection change={change()} impact={undefined} />);
    expect(container).toBeEmptyDOMElement();
    rerender(<ImpactSection change={change()} impact={undefined} error="boom" />);
    expect(screen.getByRole("alert")).toHaveTextContent("boom");
    expect(screen.getByText(CHANGE_REVIEW.IMPACT)).toBeInTheDocument();
  });
});

describe("RiskSection", () => {
  it("counts the levels today and after, and lists what is new or changed with its reason", () => {
    render(
      <RiskSection
        change={change({
          verdict: "widens",
          risk: {
            before: { overall: "low", items: [item("allowed_domains", "low")] },
            after: { overall: "high", items: [item("allowed_domains", "high"), item("desktop", "medium"), item("tool_rules", "low")] },
          },
        })}
      />,
    );
    expect(screen.getByText("Low 1, Medium 0, High 0")).toBeInTheDocument();
    expect(screen.getByText("Low 1, Medium 1, High 1")).toBeInTheDocument();
    expect(screen.getByText("why allowed_domains")).toBeInTheDocument();
    expect(screen.getByText("why desktop")).toBeInTheDocument();
    expect(screen.getByText(CHANGE_REVIEW.RISK_RULES_NOTE)).toBeInTheDocument();
  });

  it("omits the Today row on a create", () => {
    render(<RiskSection change={change({ verdict: "widens", risk: { before: null, after: { overall: "low", items: [] } } })} />);
    expect(screen.queryByText(CHANGE_REVIEW.RISK_TODAY)).toBeNull();
    expect(screen.getByText(CHANGE_REVIEW.RISK_AFTER)).toBeInTheDocument();
  });

  it("says a profile change with no grade has none available, not that its kind is never graded", () => {
    render(<RiskSection change={change({ verdict: "widens" })} />);
    const note = screen.getByText(CHANGE_REVIEW.RISK_NOT_GRADED);
    expect(note).toBeInTheDocument();
    expect(note.textContent).not.toMatch(/kind|Profile/);
  });
});

describe("RawDiffSection", () => {
  it("is closed by default and shows a plain name beside each path", () => {
    render(<RawDiffSection change={change()} />);
    const raw = screen.getByTestId("change-raw-diff");
    expect(raw).not.toHaveAttribute("open");
    expect(within(raw).getByText(CHANGE_REVIEW.RAW_DIFF)).toBeInTheDocument();
    expect(within(raw).getByText("Allowed hosts")).toBeInTheDocument();
    expect(within(raw).getByText("ceiling.allowed_domains")).toBeInTheDocument();
    expect(within(raw).getByText(CHANGE_REVIEW.SHOW_FULL).closest("details")).not.toHaveAttribute("open");
  });

  it("shows the path alone when it has no plain name", () => {
    const c = change();
    c.diff.changed = ["ceiling.nothing_here"];
    render(<RawDiffSection change={c} />);
    expect(screen.getByText("ceiling.nothing_here")).toBeInTheDocument();
  });
});
