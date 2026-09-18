/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// W25-1: SummaryHeader's "Interactive — attachable" chip must claim
// attachability under the SAME predicate AttachTerminal itself gates on
// (attach-terminal.tsx: `if (!operator) { ...requires the admin role }`)
// — otherwise a member sees the chip promise attachability and then gets a
// red "requires the admin role" error the instant they open the terminal
// below it (OverviewTab renders <AttachTerminal> whenever `attachable`).
import type { ReactElement } from "react";
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { SummaryHeader } from "./run-detail-summary-header";
import { OperatorProvider } from "../wardyn/operator-context";
import type { AgentRun, RunState } from "../../lib/types";
import { TERMINAL_RUN_STATES } from "../../lib/types";
import { RUN } from "../wardyn/copy";
import {
  CHIP_SETTING_UP,
  CHIP_WAITING_FOR_MACHINE,
  STARTING_CONTAINER_CREATING,
} from "./run-status-detail";

// SummaryHeader now renders a "Runs" breadcrumb <Link> (react-router-dom),
// which throws outside a Router context — wrap every render the same way
// run-detail-ssh.test.tsx does for its own router-dependent screen.
function renderHeader(ui: ReactElement) {
  return render(<MemoryRouter>{ui}</MemoryRouter>);
}

const runningInteractive: AgentRun = {
  id: "run-1",
  created_at: "now",
  updated_at: "now",
  created_by: "me",
  agent: "claude-code",
  repo: "acme/widgets",
  task: "",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://x",
  runner_target: "docker",
  interactive: true,
};

describe("SummaryHeader — attachable chip predicate (W25-1)", () => {
  it("shows the plain 'Interactive' chip (no attachable claim) for a member", () => {
    renderHeader(
      <OperatorProvider operator={false}>
        <SummaryHeader run={runningInteractive} terminal={false} onKill={() => {}} />
      </OperatorProvider>,
    );
    expect(screen.getByText("Interactive")).toBeInTheDocument();
    expect(screen.queryByText("Interactive — attachable")).toBeNull();
  });

  it("shows 'Interactive — attachable' for an operator on a RUNNING interactive run", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader run={runningInteractive} terminal={false} onKill={() => {}} />
      </OperatorProvider>,
    );
    expect(screen.getByText("Interactive — attachable")).toBeInTheDocument();
  });
});

// Command-bar reshape (design board seg2a): the fat identity card became a
// 52px single row at xl and up (wraps below — review R-16). These pin the
// shape that survived the squeeze — task as the page's h1, terminal-disabled
// Kill, and the pending-approvals chip.
describe("SummaryHeader — command bar", () => {
  const taskRun: AgentRun = { ...runningInteractive, task: "audit the egress proxy for missing hosts" };

  it("renders run.task as the page's h1", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader run={taskRun} terminal={false} onKill={() => {}} />
      </OperatorProvider>,
    );
    expect(
      screen.getByRole("heading", { name: "audit the egress proxy for missing hosts", level: 1 }),
    ).toBeInTheDocument();
  });

  it("disables Kill on a terminal run", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader run={{ ...taskRun, state: "COMPLETED" }} terminal={true} onKill={() => {}} />
      </OperatorProvider>,
    );
    expect(screen.getByRole("button", { name: /kill/i })).toBeDisabled();
  });

  it("keeps Kill enabled on a live run", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader run={taskRun} terminal={false} onKill={() => {}} />
      </OperatorProvider>,
    );
    expect(screen.getByRole("button", { name: /kill/i })).toBeEnabled();
  });

  it("shows the pending-approval count when non-zero", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader run={taskRun} terminal={false} pendingApprovalCount={2} onKill={() => {}} />
      </OperatorProvider>,
    );
    expect(screen.getByText(/2 waiting/i)).toBeInTheDocument();
  });

  it("renders no pending-approval chip when the count is zero", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader run={taskRun} terminal={false} pendingApprovalCount={0} onKill={() => {}} />
      </OperatorProvider>,
    );
    expect(screen.queryByText(/waiting/i)).toBeNull();
  });
});

// review R-02: `truncate` on an `inline-flex` Chip clips mid-word with NO
// ellipsis — the anonymous flex child (the text node) gets min-content
// sizing regardless of the parent's own overflow-hidden. Red-first: before
// this fix the chip's className carried `truncate` directly and had no inner
// span, so this test's selector found nothing with that class inside the
// chip's text.
describe("SummaryHeader — failure_hint chip actually ellipsizes (review R-02)", () => {
  it("wraps the hint in a block span that carries truncate, not the inline-flex chip itself", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader
          run={{ ...runningInteractive, state: "FAILED", failure_hint: "a very long server-side reason" }}
          terminal={true}
          onKill={() => {}}
        />
      </OperatorProvider>,
    );
    const hint = screen.getByText("a very long server-side reason");
    expect(hint.tagName).toBe("SPAN");
    expect(hint.className).toContain("truncate");
    expect(hint.className).toContain("block");
    expect(hint.className).toContain("min-w-0");
  });
});

// 0.7.3 F7 — "Start a run like this one" on the header, for every terminal
// run (a strict superset of the failure block's 3 endings). Tab order clone
// -> kill: outline, never the bar's one danger slot.
describe("SummaryHeader — clone door (0.7.3 F7)", () => {
  // review C-14: the component itself gates on the `terminal` PROP, never on
  // `run.state` directly (run-detail-summary-header.tsx:233) — this loop pins
  // the CALLER's contract (every one of the 5 states in TERMINAL_RUN_STATES
  // is passed in as terminal={true} by run-detail.tsx), not a branch inside
  // SummaryHeader. That contract is worth 5 identical-looking cases: a state
  // dropped from the caller's terminal check would pass this test suite
  // silently if it were asserted only once.
  it.each(TERMINAL_RUN_STATES)("a terminal run's header offers the clone door (%s)", (state) => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader
          run={{ ...runningInteractive, state: state as RunState }}
          terminal={true}
          onKill={() => {}}
          onClone={() => {}}
        />
      </OperatorProvider>,
    );
    expect(screen.getByRole("button", { name: RUN.CLONE_CTA })).toBeInTheDocument();
  });

  it("a live run's header offers no clone door", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader run={runningInteractive} terminal={false} onKill={() => {}} onClone={() => {}} />
      </OperatorProvider>,
    );
    expect(screen.queryByRole("button", { name: RUN.CLONE_CTA })).toBeNull();
  });

  it("renders no clone door when the page passes no onClone", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader run={{ ...runningInteractive, state: "COMPLETED" }} terminal={true} onKill={() => {}} />
      </OperatorProvider>,
    );
    expect(screen.queryByRole("button", { name: RUN.CLONE_CTA })).toBeNull();
  });

  it("clone is outline — Kill keeps the bar's one danger slot", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader
          run={{ ...runningInteractive, state: "COMPLETED" }}
          terminal={true}
          onKill={() => {}}
          onClone={() => {}}
        />
      </OperatorProvider>,
    );
    const clone = screen.getByRole("button", { name: RUN.CLONE_CTA });
    expect(clone.className).not.toContain("text-danger");
    const kill = screen.getByRole("button", { name: /kill/i });
    expect(kill.className).toContain("text-danger");
  });
});

// ---------------------------------------------------------------------------
// M3 — WHO beside WHAT. The identity glyph used to sit alone at the left of the
// bar while the state chip lived four elements away past the repo and the
// workspace path, so the same run read as two different shapes on the board and
// in the cockpit. One pair, one attention vocabulary, no seam.
// ---------------------------------------------------------------------------
describe("SummaryHeader — the who + what glyph pair", () => {
  const glyph = () => document.querySelector("[data-attention]")!;

  it("puts the state glyph immediately after the agent badge, not across the bar", () => {
    renderHeader(
      <OperatorProvider operator>
        <SummaryHeader run={runningInteractive} terminal={false} onKill={() => {}} />
      </OperatorProvider>,
    );
    const g = glyph();
    expect(g).toBeTruthy();
    // Adjacent siblings inside one wrapper — the pair, never fused. "CC" is
    // Claude Code's monogram (AgentBadge), so this asserts WHO then WHAT.
    expect(g.previousElementSibling).toHaveTextContent("CC");
  });

  it("a RUNNING run with nothing pending reads as running", () => {
    renderHeader(
      <OperatorProvider operator>
        <SummaryHeader run={runningInteractive} terminal={false} onKill={() => {}} />
      </OperatorProvider>,
    );
    expect(glyph()).toHaveAttribute("data-attention", "active");
  });

  it("a HELD approval outranks a merely pending one — the same order attentionRank uses", () => {
    renderHeader(
      <OperatorProvider operator>
        <SummaryHeader
          run={runningInteractive}
          terminal={false}
          pendingApprovalCount={4}
          sandboxHeld
          onKill={() => {}}
        />
      </OperatorProvider>,
    );
    expect(glyph()).toHaveAttribute("data-attention", "permission");
  });

  it("a pending approval that is NOT holding the sandbox reads as monitoring, not as a demand", () => {
    renderHeader(
      <OperatorProvider operator>
        <SummaryHeader
          run={runningInteractive}
          terminal={false}
          pendingApprovalCount={1}
          onKill={() => {}}
        />
      </OperatorProvider>,
    );
    expect(glyph()).toHaveAttribute("data-attention", "monitoring");
  });

  // The kill cascade leaves the run's PENDING approvals alive, so a run killed
  // seconds after a wait_for_review raise still arrives here with sandboxHeld.
  it("a KILLED run reads as needing review even while a hold is still parked on it", () => {
    renderHeader(
      <OperatorProvider operator>
        <SummaryHeader
          run={{ ...runningInteractive, state: "KILLED" }}
          terminal
          pendingApprovalCount={1}
          sandboxHeld
          onKill={() => {}}
        />
      </OperatorProvider>,
    );
    expect(glyph()).toHaveAttribute("data-attention", "interrupted");
  });

  it("a FAILED run reads as needing review, whatever is pending on it", () => {
    renderHeader(
      <OperatorProvider operator>
        <SummaryHeader
          run={{ ...runningInteractive, state: "FAILED" }}
          terminal
          pendingApprovalCount={2}
          onKill={() => {}}
        />
      </OperatorProvider>,
    );
    expect(glyph()).toHaveAttribute("data-attention", "interrupted");
  });
});

// 0.7.6 finding 6: a STARTING run says what it is waiting ON, in the header's
// own short register. The 0.7.5 field report's estate watched an identical
// "Starting" badge for 131 seconds twice and had no way to tell a first pull
// from a hang.
describe("SummaryHeader — the startup reason (finding 6)", () => {
  const starting = (extra: Partial<AgentRun>): AgentRun => ({
    ...runningInteractive,
    state: "STARTING",
    interactive: false,
    ...extra,
  });

  it("carries the reason, in the SHORT register, with the sentence on the title", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader
          run={starting({ status_detail: "agent: ContainerCreating", status_reason: "ContainerCreating" })}
          terminal={false}
          onKill={() => {}}
        />
      </OperatorProvider>,
    );
    const chip = screen.getByText(CHIP_SETTING_UP);
    expect(chip).toBeInTheDocument();
    // The sentence is reachable, but never the chip's own text: at max-w-[160px]
    // it would truncate to a restatement of the STARTING badge beside it.
    expect(chip.closest("[title]")).toHaveAttribute("title", STARTING_CONTAINER_CREATING);
    expect(screen.queryByText(STARTING_CONTAINER_CREATING)).toBeNull();
  });

  // review R-01/F1-F4's rule for the failure_hint chip applies here for the same
  // reason: min-w-0 shrink lets it give up WIDTH, never existence. A `hidden …`
  // waterfall step would hide the one sentence a waiting person is waiting for.
  it("may never hide at any width", () => {
    const { container } = renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader
          run={starting({ status_detail: "pod: Unschedulable: no room", status_reason: "Unschedulable" })}
          terminal={false}
          onKill={() => {}}
        />
      </OperatorProvider>,
    );
    const chip = screen.getByText(CHIP_WAITING_FOR_MACHINE).closest("[title]");
    expect(chip).not.toBeNull();
    expect(container.innerHTML).toContain(CHIP_WAITING_FOR_MACHINE);
    expect(chip?.className ?? "").not.toMatch(/(^|\s)hidden(\s|$)/);
    expect(chip?.className ?? "").toContain("shrink");
  });

  it("says NOTHING on a run that is no longer starting, even carrying a stale detail", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader
          run={{
            ...runningInteractive,
            state: "COMPLETED",
            status_detail: "agent: ContainerCreating",
            status_reason: "ContainerCreating",
          }}
          terminal
          onKill={() => {}}
        />
      </OperatorProvider>,
    );
    expect(screen.queryByText(CHIP_SETTING_UP)).toBeNull();
    expect(screen.queryByText(STARTING_CONTAINER_CREATING)).toBeNull();
  });
});
