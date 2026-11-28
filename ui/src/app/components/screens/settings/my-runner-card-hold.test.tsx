/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// My runner keeps today's states through TanStack Query: a refusal that hides the card calls onHidden once, a silent
// failure keeps the rows, and a Retry leaves the error sentence on screen until the read answers.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { RunnerView } from "../../../lib/types/runners";

const listMineMock = vi.fn();
vi.mock("../../../lib/api/runners", () => ({
  RUNNERS_DISABLED: "runners_disabled",
  runners: { listMine: (...a: unknown[]) => listMineMock(...a) },
}));
vi.mock("../../../lib/api/runner-pools", () => ({
  runnerPools: { list: () => Promise.resolve({ pools: [] }), get: vi.fn(), members: vi.fn() },
  remoteRunners: { listMine: () => Promise.resolve([]) },
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { HttpError } from "../../../lib/api/core";
import { OperatorProvider } from "../../wardyn/operator-context";
import { MyRunnerCard } from "./my-runner-card";

const WAITING = {
  id: "r-mbp",
  name: "mbp-ben",
  state: "unclaimed",
  online: false,
  owner_kind: "person",
  kind: "self_hosted",
  key_fingerprint: "SHA256:abc",
  created_at: new Date().toISOString(),
  usage: { online: false, in_use: { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 }, available: { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 }, disk_capped: true },
  pools: [],
} as unknown as RunnerView;

const onHidden = vi.fn();
const renderCard = () =>
  render(
    <MemoryRouter initialEntries={["/account"]}>
      <OperatorProvider operator={false} securityOperator={false} operatorResolved principal="alice">
        <MyRunnerCard onHidden={onHidden} />
      </OperatorProvider>
    </MemoryRouter>,
  );

const settle = () => act(async () => void (await new Promise((resolve) => setTimeout(resolve, 20))));
const tick = async () => {
  await act(async () => {
    document.dispatchEvent(new Event("visibilitychange"));
  });
  await settle();
};
const expand = async (user: ReturnType<typeof userEvent.setup>) => {
  await user.click(await screen.findByRole("button", { name: /^My runner/ }));
};

beforeEach(() => {
  listMineMock.mockReset();
  onHidden.mockReset();
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ status: "ok", version: "0.10.0" }), { status: 200 })));
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("My runner failure hold", () => {
  it("a refusal that hides the card calls onHidden once, also when it answers a poll with rows shown", async () => {
    listMineMock.mockResolvedValueOnce([WAITING]).mockRejectedValueOnce(new HttpError(403, "runners are off", "runners_disabled"));
    const { container } = renderCard();
    await settle();
    expect(await screen.findByText("1 waiting for you to claim")).toBeInTheDocument();
    expect(onHidden).not.toHaveBeenCalled();
    await tick();
    expect(listMineMock).toHaveBeenCalledTimes(2);
    expect(container).toBeEmptyDOMElement();
    expect(onHidden).toHaveBeenCalledTimes(1);
    expect(onHidden).toHaveBeenCalledWith("runners_disabled");
    await settle();
    expect(onHidden).toHaveBeenCalledTimes(1);
  });

  it("a silent failure keeps the rows", async () => {
    listMineMock.mockResolvedValueOnce([WAITING]).mockRejectedValueOnce(new HttpError(500, "boom"));
    renderCard();
    await settle();
    expect(screen.getByText("1 waiting for you to claim")).toBeInTheDocument();
    await tick();
    expect(listMineMock).toHaveBeenCalledTimes(2);
    expect(screen.getByText("1 waiting for you to claim")).toBeInTheDocument();
    expect(screen.queryByText("Couldn't load your runners.")).not.toBeInTheDocument();
  });

  it("after a failed first read, Retry keeps the error sentence on screen until the answer", async () => {
    listMineMock.mockRejectedValueOnce(new HttpError(500, "the runner service is down", "runner_down"));
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    expect(await screen.findByText("the runner service is down")).toBeInTheDocument();
    let answer: (rows: RunnerView[]) => void = () => {};
    listMineMock.mockReturnValueOnce(new Promise<RunnerView[]>((resolve) => (answer = resolve)));
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await settle();
    expect(listMineMock).toHaveBeenCalledTimes(2);
    expect(screen.getByText("the runner service is down")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
    await act(async () => answer([WAITING]));
    await settle();
    expect(screen.queryByText("the runner service is down")).not.toBeInTheDocument();
    expect(screen.getByText("mbp-ben")).toBeInTheDocument();
  });
});
