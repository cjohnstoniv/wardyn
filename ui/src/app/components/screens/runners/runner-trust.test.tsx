/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A runner's trust class on Admin -> Runners (0.9.3, owner mock item 3, Q5). The strings are written out,
// not read from the copy module, so a drift in them fails this suite. The organization spelling is the
// console's ("Organization"), per the owner's ruling.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { toast } from "sonner";

const setTrustMock = vi.fn();
vi.mock("../../../lib/api/runners", () => ({ runners: { setTrust: (...a: unknown[]) => setTrustMock(...a), revoke: vi.fn() } }));
const getChangeMock = vi.fn();
vi.mock("../../../lib/api/governance", async (orig) => ({
  ...(await orig<typeof import("../../../lib/api/governance")>()),
  governance: { getChange: (id: string) => getChangeMock(id) },
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { HttpError, PendingChangeError } from "../../../lib/api/core";
import { aheadByHours } from "../../../lib/test-clock";
import type { GovernanceChange } from "../../../lib/types";
import type { RunnerView } from "../../../lib/types/runners";
import { setField } from "../../../../test/set-field";
import { OperatorProvider } from "../../wardyn/operator-context";
import { RunnerCard } from "./runner-card";
import { RunnerDrawer } from "./runner-drawer";

function runner(over: Partial<RunnerView> = {}): RunnerView {
  const none = { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 };
  return {
    id: "r-laptop",
    name: "laptop-7",
    state: "claimed",
    owner: "alice@example.com",
    owner_kind: "person",
    trust_class: "personal",
    online: true,
    key_fingerprint: "SHA256:fp",
    key_fingerprint_abbreviated: false,
    created_at: aheadByHours(-72),
    claimed_at: aheadByHours(-71),
    kind: "self_hosted",
    limits: null,
    usage: { online: true, in_use: none, available: none, disk_capped: true },
    pools: [],
    ...over,
  };
}

const ORG = runner({ id: "r-eks", name: "eks-prod-runners", owner: undefined, owner_kind: "org", trust_class: "org_cluster", trust_set_by: "sam@example.com", trust_set_at: aheadByHours(-72) });

const onChanged = vi.fn();
function open(r: RunnerView, securityOperator = true) {
  return render(
    <OperatorProvider operator securityOperator={securityOperator} operatorResolved principal="sam@example.com">
      <RunnerDrawer runner={r} onClose={vi.fn()} onChanged={onChanged} onOpenPool={vi.fn()} />
    </OperatorProvider>,
  );
}
const trigger = () => screen.getByRole("combobox", { name: "Trust class" });

beforeEach(() => {
  setTrustMock.mockReset();
  getChangeMock.mockReset();
  onChanged.mockReset();
  vi.mocked(toast.success).mockReset();
});
afterEach(() => cleanup());

describe("Admin -> Runners trust class: a person's runner, a security admin", () => {
  it("shows the section with its class, description and hint, and Change is off until the class and a reason change", async () => {
    const user = userEvent.setup();
    open(runner({ trust_set_by: "sam@example.com", trust_set_at: aheadByHours(-72) }));
    const section = (await screen.findByRole("dialog")).querySelector("[data-trust]") as HTMLElement;
    expect(within(section).getByText("Trust class", { selector: "h3" })).toBeInTheDocument();
    expect(within(section).getByText("Security admin")).toBeInTheDocument();
    expect(trigger()).toHaveTextContent("Personal");
    expect(within(section).getByText("A person's own computer. Wardyn assumes nothing about how it is managed.")).toBeInTheDocument();
    expect(within(section).getByText("Credential delivery rules in Governance can differ by class. Reviving a run started under the old class is checked again and refused if the rules differ.")).toBeInTheDocument();
    expect(within(section).getByText(/^Set by sam@example\.com /)).toBeInTheDocument();
    // Personal on a person's runner is the default: no chip.
    expect(within(await screen.findByRole("dialog")).queryByText("Managed device")).toBeNull();
    const save = within(section).getByRole("button", { name: "Change trust class" });
    expect(save).toBeDisabled();
    setField(within(section).getByLabelText(/^Reason/), "Enrolled in device management, ticket SEC-2210");
    expect(save).toBeDisabled(); // a reason alone is not a change
    await user.click(trigger());
    expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual(["Personal", "Managed device"]);
    await user.click(screen.getByRole("option", { name: "Managed device" }));
    expect(within(section).getByText("A person's computer your organization manages. This is your assertion; Wardyn can't check it.")).toBeInTheDocument();
    expect(within(section).getByText("Required. Kept on the change and in the audit trail.")).toBeInTheDocument();
    expect(save).toBeEnabled();
  });

  it("a change needs its reason: a blank one keeps Change off", async () => {
    const user = userEvent.setup();
    open(runner());
    await user.click(await screen.findByRole("combobox", { name: "Trust class" }));
    await user.click(screen.getByRole("option", { name: "Managed device" }));
    setField(screen.getByLabelText(/^Reason/), "   ");
    expect(screen.getByRole("button", { name: "Change trust class" })).toBeDisabled();
  });

  it("sits between Resources and Pools, as the mock has it", async () => {
    open(runner());
    await screen.findByRole("combobox", { name: "Trust class" });
    const at = (sel: string) => document.querySelector(sel) as Element;
    const before = (a: Element, b: Element) => Boolean(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);
    expect(before(at("[data-resources]"), at("[data-trust]"))).toBe(true);
    expect(before(at("[data-trust]"), at("[data-pools]"))).toBe(true);
  });

  it("saves with the reason, says what the runner is now, and re-reads", async () => {
    const user = userEvent.setup();
    setTrustMock.mockResolvedValue(runner({ trust_class: "managed_device" }));
    open(runner());
    await user.click(await screen.findByRole("combobox", { name: "Trust class" }));
    await user.click(screen.getByRole("option", { name: "Managed device" }));
    setField(screen.getByLabelText(/^Reason/), " Enrolled, SEC-2210 ");
    await user.click(screen.getByRole("button", { name: "Change trust class" }));
    expect(setTrustMock).toHaveBeenCalledWith("r-laptop", "managed_device", "Enrolled, SEC-2210");
    expect(toast.success).toHaveBeenCalledWith("laptop-7 is now Managed device");
    expect(onChanged).toHaveBeenCalledTimes(1);
  });

  it("a change held for a second person is not a save: the held notice shows, nothing is announced as done", async () => {
    const user = userEvent.setup();
    setTrustMock.mockRejectedValue(new PendingChangeError({} as GovernanceChange));
    open(runner());
    await user.click(await screen.findByRole("combobox", { name: "Trust class" }));
    await user.click(screen.getByRole("option", { name: "Managed device" }));
    setField(screen.getByLabelText(/^Reason/), "Enrolled");
    await user.click(screen.getByRole("button", { name: "Change trust class" }));
    const held = await screen.findByRole("status");
    expect(held).toHaveTextContent("Held for a second person.");
    expect(held).toHaveTextContent("Nothing changes until they approve it in Governance.");
    expect(toast.success).not.toHaveBeenCalled();
    expect(onChanged).not.toHaveBeenCalled();
    expect(screen.queryByRole("alert")).toBeNull();
    // Another save would meet the 409 for the proposal already waiting, so Save stays off until it resolves.
    expect(screen.getByRole("button", { name: "Change trust class" })).toBeDisabled();
  });

  it("a held proposal that is rejected or withdrawn frees Save and reloads the runner, and one still pending does neither", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      setTrustMock.mockRejectedValue(new PendingChangeError({ id: "chg-1" } as GovernanceChange));
      getChangeMock.mockResolvedValue({ id: "chg-1", state: "pending" });
      open(runner());
      await user.click(await screen.findByRole("combobox", { name: "Trust class" }));
      await user.click(screen.getByRole("option", { name: "Managed device" }));
      setField(screen.getByLabelText(/^Reason/), "Enrolled");
      const save = () => screen.getByRole("button", { name: "Change trust class" });
      await user.click(save());
      await screen.findByText("Held for a second person.");
      await act(() => vi.advanceTimersByTimeAsync(16_000));
      expect(getChangeMock).toHaveBeenCalledWith("chg-1");
      expect(save()).toBeDisabled();
      expect(onChanged).not.toHaveBeenCalled();
      getChangeMock.mockResolvedValue({ id: "chg-1", state: "rejected" });
      await act(() => vi.advanceTimersByTimeAsync(16_000));
      expect(save()).toBeEnabled();
      expect(onChanged).toHaveBeenCalledTimes(1);
      expect(screen.queryByText("Held for a second person.")).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it("shows the server's own sentence when it refuses, and does not re-read", async () => {
    const user = userEvent.setup();
    setTrustMock.mockRejectedValue(new HttpError(409, "claim the runner first: its claim sets the initial trust class"));
    open(runner());
    await user.click(await screen.findByRole("combobox", { name: "Trust class" }));
    await user.click(screen.getByRole("option", { name: "Managed device" }));
    setField(screen.getByLabelText(/^Reason/), "Enrolled");
    await user.click(screen.getByRole("button", { name: "Change trust class" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("claim the runner first: its claim sets the initial trust class");
    expect(toast.success).not.toHaveBeenCalled();
    expect(onChanged).not.toHaveBeenCalled();
  });

  it("a managed device reads as one: the chip and the select say Managed device", async () => {
    open(runner({ trust_class: "managed_device" }));
    const drawer = await screen.findByRole("dialog");
    expect(trigger()).toHaveTextContent("Managed device");
    // The chip beside Self-hosted / Online, outside the select.
    expect(within(drawer).getAllByText("Managed device").some((el) => el.closest("[data-slot=select-trigger]") === null)).toBe(true);
  });
});

describe("Admin -> Runners trust class: read-only", () => {
  it("an organization's runner is always Organization cluster: no select, no Reason, no Change; the owner reads Organization", async () => {
    open(ORG);
    const drawer = await screen.findByRole("dialog");
    const section = drawer.querySelector("[data-trust]") as HTMLElement;
    expect(within(section).queryByRole("combobox")).toBeNull();
    expect(within(section).queryByLabelText(/^Reason/)).toBeNull();
    expect(within(section).queryByRole("button", { name: "Change trust class" })).toBeNull();
    expect(within(section).getByText("Organization cluster")).toBeInTheDocument();
    expect(within(section).getByText("A cluster your organization runs. An organization's runner is always this class.")).toBeInTheDocument();
    expect(within(section).getByText(/^Set by sam@example\.com /)).toBeInTheDocument();
    // The chip beside the state, and the owner in the header.
    expect(within(drawer).getAllByText("Organization cluster")).toHaveLength(2);
    expect(within(drawer).getByText(/^Organization$/)).toBeInTheDocument();
  });

  it("someone who is not a security admin sees a person's runner's class read-only", async () => {
    open(runner({ trust_class: "managed_device" }), false);
    const section = (await screen.findByRole("dialog")).querySelector("[data-trust]") as HTMLElement;
    expect(within(section).queryByRole("combobox")).toBeNull();
    expect(within(section).queryByRole("button", { name: "Change trust class" })).toBeNull();
    expect(within(section).getByText("Managed device")).toBeInTheDocument();
    expect(within(section).getByText("A person's computer your organization manages. This is your assertion; Wardyn can't check it.")).toBeInTheDocument();
  });

  it("an unclaimed runner has no class to change yet", async () => {
    open(runner({ state: "unclaimed", claimed_at: undefined }));
    const section = (await screen.findByRole("dialog")).querySelector("[data-trust]") as HTMLElement;
    expect(within(section).getByText("Available once its owner has claimed it.")).toBeInTheDocument();
    expect(within(section).queryByRole("combobox")).toBeNull();
  });

  it("a revoked runner's class is read-only", async () => {
    open(runner({ state: "revoked", revoked_at: aheadByHours(-1), revoked_by: "carol@example.com" }));
    const section = (await screen.findByRole("dialog")).querySelector("[data-trust]") as HTMLElement;
    expect(within(section).queryByRole("combobox")).toBeNull();
    expect(within(section).getByText("Personal")).toBeInTheDocument();
  });
});

describe("Admin -> Runners list: the trust class chip", () => {
  const chips = (r: RunnerView) => {
    render(<RunnerCard f={{ kind: "self_hosted", id: r.id, name: r.name, runner: r }} onOpen={vi.fn()} onOpenPool={vi.fn()} />);
    return screen.queryAllByText(/^(Personal|Managed device|Organization cluster)$/).map((e) => e.textContent);
  };
  it("an organization's runner always shows its class", () => {
    expect(chips(ORG)).toEqual(["Organization cluster"]);
    expect(screen.getByText("Organization", { selector: "dd" })).toBeInTheDocument();
  });
  it("a person's runner shows a chip only when it is a Managed device", () => {
    expect(chips(runner({ trust_class: "managed_device" }))).toEqual(["Managed device"]);
    cleanup();
    expect(chips(runner())).toEqual([]);
  });
});
