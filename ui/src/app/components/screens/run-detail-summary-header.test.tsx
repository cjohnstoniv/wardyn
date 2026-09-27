/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SummaryHeader's "Interactive — attachable" chip must claim
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
import { waitingReauth } from "../wardyn/model-access-copy";
import { waitingAdoConsent } from "../../lib/reauth-waiting-copy";
import type { AgentRun, RunState } from "../../lib/types";
import { TERMINAL_RUN_STATES } from "../../lib/types";
import { RUN } from "../wardyn/copy";
import { RUN_FACTS } from "../wardyn/copy/door";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { MODEL_PROVIDERS, providerStatus } from "../../lib/test-fixtures";
import type { SetupStatus } from "../../lib/types";
import { AUTONOMY_META } from "../wardyn/autonomy-meta";
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

describe("SummaryHeader — attachable chip predicate", () => {
  // ticket: W25-1
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
// 52px single row at xl and up (wraps below). These pin the
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

// `truncate` on an `inline-flex` Chip clips mid-word with NO
// ellipsis — the anonymous flex child (the text node) gets min-content
// sizing regardless of the parent's own overflow-hidden. The chip's text
// must sit in an inner block span carrying `truncate`, not on the chip's
// own `inline-flex` className, or this test's selector finds nothing with
// that class inside the chip's text.
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

// "Start a run like this one" on the header, for every terminal
// run (a strict superset of the failure block's 3 endings). Tab order clone
// -> kill: outline, never the bar's one danger slot.
describe("SummaryHeader — clone door", () => {
  // ticket: 0.7.3 F7
  // The component itself gates on the `terminal` PROP, never on
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

// WHO beside WHAT: the identity glyph and the state chip must sit adjacent,
// or the same run reads as two different shapes on the board and in the
// cockpit. One pair, one attention vocabulary, no seam.
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

  // "sandbox held" would send the person looking for an Approve
  // button that does not exist for this kind. The chip names what they can do,
  // and KEEPS THE COUNT (round-2 UX S8): a count-free string would hide a
  // co-pending egress approval, so the person signs in and the run still sits.
  it("a held run waiting on an AWS sign-in says so, and still counts the others", () => {
    renderHeader(
      // The run's OWNER (created_by "me") — "your sign-in" is true of them, and
      // only of them.
      <OperatorProvider operator principal="me">
        <SummaryHeader
          run={runningInteractive}
          terminal={false}
          pendingApprovalCount={2}
          sandboxHeld
          awaitingReauth
          onKill={() => {}}
        />
      </OperatorProvider>,
    );
    expect(screen.getByText(waitingReauth(2))).toBeInTheDocument();
    expect(screen.queryByText(/sandbox held/i)).not.toBeInTheDocument();
    expect(waitingReauth(2)).toMatch(/1 more waiting/);
    expect(waitingReauth(1)).toBe("Waiting for your AWS sign-in");
  });

  // W6-U SHOULD-1 — the same chip, read by somebody who is not the owner: an
  // admin opening a member's held run, or a member under a shared-lane row
  // whose own cockpit row says "ask your admin". Only the owner's sign-in
  // clears the hold, so "your" was false on both.
  it("…and says whose sign-in it is when the reader is NOT the owner", () => {
    renderHeader(
      <OperatorProvider operator principal="admin@corp">
        <SummaryHeader
          run={runningInteractive}
          terminal={false}
          pendingApprovalCount={1}
          sandboxHeld
          awaitingReauth
          onKill={() => {}}
        />
      </OperatorProvider>,
    );
    expect(screen.getByText(waitingReauth(1, false))).toBeInTheDocument();
    expect(waitingReauth(1, false)).toBe("Waiting for the owner's AWS sign-in");
    expect(screen.queryByText(waitingReauth(1))).not.toBeInTheDocument();
    // The count clause is unchanged by the audience (round-2 UX S8).
    expect(waitingReauth(3, false)).toMatch(/2 more waiting/);
  });

  // S10 round 2 (F13) — the Azure DevOps twin: same shape, a DIFFERENT
  // provider, so the cockpit chip must never say "AWS" for this one.
  it("a held run waiting on an Azure DevOps sign-in names Azure DevOps, never AWS", () => {
    renderHeader(
      <OperatorProvider operator principal="me">
        <SummaryHeader
          run={runningInteractive}
          terminal={false}
          pendingApprovalCount={2}
          sandboxHeld
          awaitingAdoConsent
          onKill={() => {}}
        />
      </OperatorProvider>,
    );
    expect(screen.getByText(waitingAdoConsent(2))).toBeInTheDocument();
    expect(waitingAdoConsent(2)).toMatch(/1 more waiting/);
    expect(waitingAdoConsent(1)).toBe("Waiting for your Azure DevOps sign-in");
    expect(screen.queryByText(/AWS/)).not.toBeInTheDocument();
  });

  it("…and says whose Azure DevOps sign-in it is when the reader is NOT the owner", () => {
    renderHeader(
      <OperatorProvider operator principal="admin@corp">
        <SummaryHeader
          run={runningInteractive}
          terminal={false}
          pendingApprovalCount={1}
          sandboxHeld
          awaitingAdoConsent
          onKill={() => {}}
        />
      </OperatorProvider>,
    );
    expect(screen.getByText(waitingAdoConsent(1, false))).toBeInTheDocument();
    expect(waitingAdoConsent(1, false)).toBe("Waiting for the owner's Azure DevOps sign-in");
    expect(screen.queryByText(waitingAdoConsent(1))).not.toBeInTheDocument();
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

// A STARTING run says what it is waiting ON, in the header's own short
// register — an identical "Starting" badge alone gives no way to tell a
// first pull from a hang.
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

  // The same rule as the failure_hint chip applies here for the same
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

// #93/#96 — the run header's autonomy chip, beside ConfinementChip.
// run.autonomy_level freezes the level resolveRunAutonomy capped this run at,
// at create time (0.8 #97).
describe("SummaryHeader — the autonomy chip beside the barrier chip", () => {
  it("renders the level's friendly label when the run carries one", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader run={{ ...runningInteractive, autonomy_level: "L1" }} terminal={false} onKill={() => {}} />
      </OperatorProvider>,
    );
    expect(screen.getByText(AUTONOMY_META.L1.label)).toBeInTheDocument();
    // The wire code stays out of accessible content — same D4 rule
    // ConfinementChip follows.
    expect(screen.queryByText("L1")).toBeNull();
  });

  it("renders no autonomy chip at all for a run with an empty autonomy_level", () => {
    renderHeader(
      <OperatorProvider operator={true}>
        <SummaryHeader run={runningInteractive} terminal={false} onKill={() => {}} />
      </OperatorProvider>,
    );
    for (const meta of Object.values(AUTONOMY_META)) {
      expect(screen.queryByText(meta.label)).toBeNull();
    }
  });
});

// #543 (design §5.7, decision 5): the run's model provider, a neutral chip in
// the header's chip bar — named from the shell's /setup/status, and marked
// removed once the provider is gone from it.
describe("SummaryHeader — the run's model provider chip", () => {
  const { gateway } = MODEL_PROVIDERS;
  function withStatus(status: SetupStatus | null, run: AgentRun) {
    renderHeader(
      <ModelAccessProvider status={status} onRefresh={() => {}}>
        <OperatorProvider operator={false}>
          <SummaryHeader run={run} terminal={false} onKill={() => {}} />
        </OperatorProvider>
      </ModelAccessProvider>,
    );
  }

  it("names the provider the run chose", () => {
    withStatus(providerStatus([{ provider: gateway }]), { ...runningInteractive, model_provider_id: gateway.id });
    expect(screen.getByText(RUN_FACTS.PROVIDER(gateway.name, false))).toBeInTheDocument();
  });

  it("a provider deleted since: (removed), by the id the run recorded", () => {
    withStatus(providerStatus([{ provider: gateway }]), { ...runningInteractive, model_provider_id: "old-gateway" });
    expect(screen.getByText(RUN_FACTS.PROVIDER("old-gateway", true))).toBeInTheDocument();
  });

  it("claims nothing removed before /setup/status answers", () => {
    withStatus(null, { ...runningInteractive, model_provider_id: gateway.id });
    expect(screen.getByText(RUN_FACTS.PROVIDER(gateway.id, false))).toBeInTheDocument();
  });

  it("a run under no provider block has no chip", () => {
    withStatus(providerStatus([{ provider: gateway }]), runningInteractive);
    expect(screen.queryByText(/^Model provider · /)).toBeNull();
  });
});
