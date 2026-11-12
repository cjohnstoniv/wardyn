/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the provider says for each way the read can go: only an answer that proves the caller has no list reads as
// absent; a failure is its own state with a retry; the setup status not yet known is loading.

import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { HttpError } from "../../../../../lib/api/core";
import type { MyMachines } from "../../../../../lib/types";
import type { SetupStatus } from "../../../../../lib/types/setup";
import { ModelAccessProvider } from "../../../../wardyn/model-access-context";
import { KeptMachinesProvider, useKeptMachines } from "./kept-machines-provider";

afterEach(cleanup);

const ON = { features: { macos_vms: true } } as SetupStatus;
const LIST: MyMachines = { machines: [], allowed: true, count: 0, limit: 1, worst_bytes: 0 };

function Show() {
  const read = useKeptMachines();
  return (
    <>
      <p data-testid="status">{read.status}</p>
      {read.status === "failed" && <button onClick={read.retry}>retry</button>}
    </>
  );
}
const mount = (status: SetupStatus | null, load: () => Promise<MyMachines>) =>
  render(
    <ModelAccessProvider status={status} onRefresh={() => {}}>
      <KeptMachinesProvider load={load}>
        <Show />
      </KeptMachinesProvider>
    </ModelAccessProvider>,
  );
const status = () => screen.getByTestId("status").textContent;

describe("kept machines provider", () => {
  it("is loading while the setup status is unknown, and reads nothing", () => {
    const load = vi.fn();
    mount(null, load);
    expect(status()).toBe("loading");
    expect(load).not.toHaveBeenCalled();
  });

  it("is absent with the macOS switch off, ready with it on", async () => {
    mount({ features: {} } as SetupStatus, vi.fn());
    expect(status()).toBe("absent");
    cleanup();
    mount(ON, async () => LIST);
    await waitFor(() => expect(status()).toBe("ready"));
  });

  it("a caller who is no person (403 kept_machine_not_your_mac) is absent", async () => {
    mount(ON, async () => { throw new HttpError(403, "no", "kept_machine_not_your_mac"); });
    await waitFor(() => expect(status()).toBe("absent"));
  });

  it("any other refusal is failed, never absent: another 403, a 401, a 404", async () => {
    for (const e of [new HttpError(403, "no", "forbidden"), new HttpError(401, "no"), new HttpError(404, "no")]) {
      mount(ON, async () => { throw e; });
      await waitFor(() => expect(status()).toBe("failed"));
      cleanup();
    }
  });

  it("any other failure is failed, never absent, and its retry reads again", async () => {
    const load = vi.fn<() => Promise<MyMachines>>().mockRejectedValueOnce(new HttpError(500, "boom")).mockResolvedValue(LIST);
    mount(ON, load);
    await waitFor(() => expect(status()).toBe("failed"));
    await act(async () => screen.getByRole("button", { name: "retry" }).click());
    await waitFor(() => expect(status()).toBe("ready"));
    expect(load).toHaveBeenCalledTimes(2);
  });

  it("a network failure is failed too", async () => {
    mount(ON, async () => { throw new TypeError("network"); });
    await waitFor(() => expect(status()).toBe("failed"));
  });
});
