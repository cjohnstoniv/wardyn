/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";

import type { RemoteRunnerView } from "../../../lib/types/runner-pools";
import { OperatorProvider } from "../../wardyn/operator-context";
import { RemoteRunnerDrawer } from "./remote-runner-drawer";

const RUNNER: RemoteRunnerView = {
  id: "shared-docker",
  kind: "remote_hosted",
  name: "shared-docker",
  people_pools: "off",
  revision: 3,
  created_at: "2026-10-01T00:00:00Z",
  updated_at: "2026-10-02T00:00:00Z",
  configured: true,
  online: true,
  limits: { runs_at_once: 6, total: { cpu_millis: 24000, memory_mib: 98304, disk_mib: 1024000 }, per_run: { cpu_millis: 4000, memory_mib: 16384, disk_mib: 102400 } },
  pools: [],
  usage: { online: true, in_use: { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 }, available: { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 }, disk_capped: true },
};

function renderDrawer(operator: boolean, securityOperator: boolean) {
  return render(
    <OperatorProvider operator={operator} securityOperator={securityOperator}>
      <RemoteRunnerDrawer runner={RUNNER} onClose={vi.fn()} onChanged={vi.fn()} onOpenPool={vi.fn()} onSaveLimits={vi.fn()} />
    </OperatorProvider>,
  );
}

describe("RemoteRunnerDrawer: who may make it available to people", () => {
  // The server holds the people policy as a security policy, so a security admin who is not a super admin
  // can switch it; it names that tier, and editing limits stays the super admin's.
  it("is the security tier's: enabled for a security admin who is not a super admin, and says so", () => {
    renderDrawer(false, true);
    const section = document.querySelector("[data-available]") as HTMLElement;
    expect(within(section).getByText("Security admin or admin")).toBeInTheDocument();
    expect(within(section).getByRole("checkbox")).toBeEnabled();
    expect(screen.queryByRole("button", { name: "Edit limits" })).toBeNull();
  });

  it("is disabled for someone outside the security tier", () => {
    renderDrawer(false, false);
    const section = document.querySelector("[data-available]") as HTMLElement;
    expect(within(section).getByRole("checkbox")).toBeDisabled();
  });
});
