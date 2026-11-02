/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin -> Runners reads the organisation's maximum macOS base age once and hands it to every Mac's card and to
// the drawer, so the badge and the drawer tell the same story (boards J7). Sentences come from the copy modules.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { MacVMCaps } from "../../../lib/types/runner-macvm";
import type { RunnerView } from "../../../lib/types/runners";

const listMock = vi.fn();
vi.mock("../../../lib/api/runners", () => ({
  RUNNERS_DISABLED: "runners_disabled",
  runners: { list: () => listMock(), settings: async () => ({ enabled: true }), tokens: async () => [], get: async () => ({}) },
}));
vi.mock("../../../lib/api/runner-pools", () => ({ remoteRunners: { list: async () => [] } }));
const siteConfig = vi.fn();
vi.mock("../../../lib/api/health", async (orig) => {
  const actual = await orig<typeof import("../../../lib/api/health")>();
  return { ...actual, health: { ...actual.health, getSiteConfig: () => siteConfig() } };
});
vi.mock("./workload-bindings-tab", () => ({ WorkloadBindingsTab: () => null }));
vi.mock("./pools/pools-tab", () => ({ PoolsTab: () => null }));
vi.mock("../../../lib/api/directory", () => ({ MIN_QUERY_LEN: 2, directory: { search: () => Promise.resolve(null) } }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { aheadByHours } from "../../../lib/test-clock";
import { MACVM_K, MACVM_P } from "../../wardyn/copy/runner-macvm";
import { OperatorProvider } from "../../wardyn/operator-context";
import { RunnersScreen } from "./runners-screen";

const none = { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 };
const mac = (name: string, macos_vm?: MacVMCaps): RunnerView => ({
  id: name, name, state: "claimed", owner: "dana@example.com", owner_kind: "person", trust_class: "managed_device", online: true,
  key_fingerprint: "SHA256:fp", key_fingerprint_abbreviated: false, created_at: aheadByHours(-72), claimed_at: aheadByHours(-71), kind: "self_hosted",
  posture: { mdm_managed: true, disk_encrypted: true, os: "darwin", os_version: "26.7", evidence: "mdm_file" },
  limits: { runs_at_once: 2, total: { cpu_millis: 8000, memory_mib: 20480, disk_mib: 0 }, per_run: { cpu_millis: 4000, memory_mib: 8192, disk_mib: 0 } },
  usage: { online: true, in_use: { ...none, macos_vm_runs: 0 }, available: none, disk_capped: true }, pools: [], macos_vm,
});
const caps = (daysOld: number): MacVMCaps => ({
  max: 2, min_cpus: 4, min_memory_mib: 8192, harnesses: ["none"], desktop: "control",
  base: { state: "ready", guest_os: "macOS 26.6.2 (25G83)", created_at: aheadByHours(-daysOld * 24), guest: { os_version: "26.6.2", sip: "on" } },
});

const show = () =>
  render(
    <MemoryRouter initialEntries={["/admin/runners"]}>
      <OperatorProvider operator securityOperator operatorResolved principal="admin@corp.example">
        <RunnersScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );

beforeEach(() => {
  listMock.mockResolvedValue([mac("old-mac", caps(41)), mac("new-mac", caps(6))]);
  siteConfig.mockResolvedValue({ macos_vm: { max_base_age_days: 30 } });
});
afterEach(() => {
  cleanup();
  listMock.mockReset();
  siteConfig.mockReset();
});

describe("Runners and the organisation's maximum base age", () => {
  it("reads the site config once; the old base's card says not offered, the new one's the count", async () => {
    show();
    const old = await screen.findByRole("listitem", { name: "old-mac" });
    await waitFor(() => expect(within(old).getByText(MACVM_K.K03)).toBeInTheDocument());
    expect(within(screen.getByRole("listitem", { name: "new-mac" })).getByText(MACVM_K.K01("0", "2"))).toBeInTheDocument();
    expect(siteConfig).toHaveBeenCalledTimes(1);
  });
  it("the drawer of the old base says Base too old, with the Mac's own macOS beside the age", async () => {
    show();
    const old = await screen.findByRole("listitem", { name: "old-mac" });
    await waitFor(() => expect(within(old).getByText(MACVM_K.K03)).toBeInTheDocument());
    await userEvent.click(within(old).getByRole("button", { name: "old-mac" }));
    const drawer = await screen.findByRole("dialog");
    expect(within(drawer).getByText(MACVM_P.P12)).toBeInTheDocument();
    expect(within(drawer).getByText(MACVM_P.P11("26.7", "26.6.2"))).toBeInTheDocument();
  });
  it("a site config that cannot be read sets no maximum: the old base reads as offered", async () => {
    siteConfig.mockRejectedValue(new Error("403"));
    show();
    const old = await screen.findByRole("listitem", { name: "old-mac" });
    await waitFor(() => expect(siteConfig).toHaveBeenCalled());
    expect(within(old).getByText(MACVM_K.K01("0", "2"))).toBeInTheDocument();
  });
  it("asks nothing while no runner sends macOS VM caps", async () => {
    listMock.mockResolvedValue([mac("plain")]);
    show();
    await screen.findByRole("listitem", { name: "plain" });
    expect(siteConfig).not.toHaveBeenCalled();
  });
});
