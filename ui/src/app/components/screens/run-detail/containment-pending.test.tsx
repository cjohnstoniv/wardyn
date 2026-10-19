/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, renderHook, screen, waitFor } from "@testing-library/react";
import { PendingChangeError } from "../../../lib/api/core";
import { absoluteTime } from "../../../lib/format";
import { aheadByHours } from "../../../lib/test-clock";
import type { GovernanceChange, RunDetail } from "../../../lib/types";
import type { RunContainment, RunContainmentPending } from "../../../lib/types/run-containment";
import { OperatorProvider } from "../../wardyn/operator-context";
import { CONTAINMENT as C } from "../../wardyn/copy/run-containment";
import { runners } from "../../../lib/api/runners";
import { ContainmentPending, useRunContainment } from "./containment-pending";

const recordNode = vi.fn();
vi.mock("../../../lib/api/runners", () => ({ runners: { recordNode: (...a: unknown[]) => recordNode(...a), runContainment: vi.fn() } }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const SINCE = aheadByHours(-1);
const BOUND = aheadByHours(1);

const tuple = {
  run_id: "run-1",
  pods: [
    {
      component: "proxy",
      uid: "uid-proxy-1",
      node_name: "n1",
      causal_uid: "uid-n1",
      post_bind: { name: "n1", uid: "uid-n1", machine_id: "m1", system_uuid: "s1" },
    },
  ],
};

function pending(over: Partial<RunContainmentPending> = {}): RunContainment {
  return {
    pending: {
      kind: "pending_delete",
      since: SINCE,
      bound_until: BOUND,
      bound_passed: false,
      name_reused: false,
      no_causal_association: false,
      two_people: false,
      tuple,
      ...over,
    },
    records: [],
  };
}

function show(containment: RunContainment | null, securityOperator = true, onChanged = vi.fn(), failed = false) {
  render(
    <OperatorProvider operator={securityOperator} securityOperator={securityOperator}>
      <ContainmentPending runId="run-1" containment={containment} onChanged={onChanged} failed={failed} />
    </OperatorProvider>,
  );
  return onChanged;
}

afterEach(() => recordNode.mockReset());

describe("ContainmentPending", () => {
  it("says the egress bound while it holds, and termination unknown once it passed", () => {
    show(pending());
    expect(screen.getByTestId("run-containment-line")).toHaveTextContent(C.pendingTitle(absoluteTime(BOUND)));
    expect(screen.getByText(C.PENDING_BODY)).toBeInTheDocument();
  });

  it("says termination unknown once the bound passed", () => {
    show(pending({ bound_passed: true }));
    expect(screen.getByTestId("run-containment-line")).toHaveTextContent(C.PASSED_TITLE);
    expect(screen.getByText(C.PASSED_BODY)).toBeInTheDocument();
  });

  it("renders nothing for a run that is not held and has no records", () => {
    show({ records: [] });
    show(null);
    expect(screen.queryByTestId("run-containment")).toBeNull();
  });

  it("names a reused node name and offers only node powered off", () => {
    show(pending({ name_reused: true, no_causal_association: true, two_people: true, tuple: { ...tuple, reuses: [{ name: "n1", uid: "uid-n1-new", machine_id: "m7", system_uuid: "s7" }] } }));
    expect(screen.getByTestId("run-containment-name-reused")).toHaveTextContent(C.NAME_REUSED);
    expect(screen.getByTestId("run-node-record-reuses")).toHaveTextContent(C.reuse("n1", "uid-n1-new", "m7", "s7"));
    expect(screen.getByText(C.ATTEST_POWERED_OFF)).toBeInTheDocument();
    expect(screen.queryByText(C.ATTEST_NO_PRIOR)).toBeNull();
    expect(screen.getByText(C.TWO_PEOPLE)).toBeInTheDocument();
  });

  it("offers no prior holder only on the no-association path", () => {
    show(pending({ no_causal_association: true, two_people: true }));
    expect(screen.getByTestId("run-containment-no-causal")).toHaveTextContent(C.NO_CAUSAL);
    expect(screen.getByText(C.ATTEST_NO_PRIOR)).toBeInTheDocument();
  });

  it("shows the form, its warnings and the machines to security operators only", () => {
    show(pending(), false);
    expect(screen.getByTestId("run-containment-line")).toBeInTheDocument();
    expect(screen.queryByTestId("run-node-record-form")).toBeNull();
  });

  it("names the warnings, the holder UIDs and when the pods were marked", () => {
    show(pending());
    const form = screen.getByTestId("run-node-record-form");
    expect(form).toHaveTextContent(C.WARN_DELETED_NODE);
    expect(form).toHaveTextContent(C.WARN_PARTITIONED);
    const machines = screen.getByTestId("run-node-record-machines");
    expect(machines).toHaveTextContent(C.pod("proxy", "uid-proxy-1"));
    expect(machines).toHaveTextContent(C.holder(C.HOLDER_CAUSAL, "uid-n1"));
    expect(machines).toHaveTextContent(C.machine("m1", "s1"));
    expect(form).toHaveTextContent(C.markedAt(absoluteTime(SINCE)));
  });

  it("sends the record with the tuple it showed", async () => {
    recordNode.mockResolvedValue({ id: "rec-1", state: "sent" });
    const onChanged = show(pending());
    const send = screen.getByRole("button", { name: C.SUBMIT });
    expect(send).toBeDisabled();
    fireEvent.change(screen.getByLabelText(C.SOURCE_ID_LABEL, { exact: false }), { target: { value: " i-0abc123 " } });
    fireEvent.click(send);
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(recordNode).toHaveBeenCalledWith("run-1", { attestation: "node_powered_off", source: "provider_console", source_id: "i-0abc123", tuple });
  });

  it("says a held record waits for a second person", async () => {
    recordNode.mockRejectedValue(new PendingChangeError({ id: "ch-1" } as GovernanceChange));
    show(pending({ two_people: true, name_reused: true }));
    fireEvent.change(screen.getByLabelText(C.SOURCE_ID_LABEL, { exact: false }), { target: { value: "i-1" } });
    fireEvent.click(screen.getByRole("button", { name: C.SUBMIT }));
    expect(await screen.findByTestId("run-node-record-held")).toHaveTextContent(C.HELD);
  });

  it("lists the records and what the runner answered", () => {
    show({
      records: [
        { id: "r2", attestation: "node_powered_off", source: "other", source_id: "t-2", state: "refused", reason: "a Node still holds n1", created_at: aheadByHours(-0.5) },
        { id: "r1", attestation: "node_powered_off", source: "other", source_id: "t-1", state: "accepted", created_at: aheadByHours(-0.8) },
      ],
    });
    const rows = screen.getAllByTestId("run-node-record");
    expect(rows[0]).toHaveTextContent(C.stateRefused("a Node still holds n1"));
    expect(rows[1]).toHaveTextContent(C.STATE_ACCEPTED);
  });
});

describe("ContainmentPending — a stale or failed read (#K3b-L1)", () => {
  it("a failed first read says so and Retry reads again, instead of showing nothing", () => {
    const onChanged = show(null, true, vi.fn(), true);
    expect(screen.getByTestId("run-containment-stale")).toHaveTextContent(C.LOAD_FAILED);
    fireEvent.click(screen.getByRole("button", { name: C.RETRY }));
    expect(onChanged).toHaveBeenCalledTimes(1);
  });

  it("a failed refresh keeps the last good hold on screen, marked as possibly out of date", () => {
    show(pending(), true, vi.fn(), true);
    expect(screen.getByTestId("run-containment-stale")).toHaveTextContent(C.STALE);
    expect(screen.getByTestId("run-containment-line")).toHaveTextContent("Containment pending");
  });
});

describe("useRunContainment — keeping a held run's containment current", () => {
  const localRun = { id: "run-1", placement: "local", updated_at: "t0" } as unknown as RunDetail;
  const read = runners.runContainment as unknown as ReturnType<typeof vi.fn>;

  afterEach(() => {
    vi.useRealTimers();
    read.mockReset();
  });

  it("re-reads a held run on a timer, so the passed bound shows without the run's updated_at moving", async () => {
    vi.useFakeTimers();
    read.mockResolvedValueOnce(pending({ bound_passed: false })).mockResolvedValue(pending({ bound_passed: true }));
    const { result } = renderHook(() => useRunContainment(localRun));
    await act(async () => {});
    expect(result.current[0]?.pending?.bound_passed).toBe(false);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(result.current[0]?.pending?.bound_passed).toBe(true);
  });

  it("a failed read keeps the last good answer and reports failed; the next good read clears it", async () => {
    vi.useFakeTimers();
    read
      .mockResolvedValueOnce(pending())
      .mockRejectedValueOnce(new Error("boom"))
      .mockResolvedValue(pending());
    const { result } = renderHook(() => useRunContainment(localRun));
    await act(async () => {});
    expect(result.current[2]).toBe(false);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(result.current[0]?.pending).toBeTruthy();
    expect(result.current[2]).toBe(true);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(result.current[2]).toBe(false);
  });

  it("a run with no hold is read once and not polled", async () => {
    vi.useFakeTimers();
    read.mockResolvedValue({ records: [] });
    renderHook(() => useRunContainment(localRun));
    await act(async () => {});
    await act(async () => {
      await vi.advanceTimersByTimeAsync(20000);
    });
    expect(read).toHaveBeenCalledTimes(1);
  });
});
