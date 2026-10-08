/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { act, renderHook, waitFor } from "@testing-library/react";
import { beforeEach, expect, it, vi } from "vitest";
import { MemoryRouter } from "react-router-dom";
import { OperatorProvider } from "../../wardyn/operator-context";
import { getAuthGeneration, notifyAuthChange } from "../../../lib/api/core";
import type { SetupStatus } from "../../../lib/types";
import { useNewRunController } from "./use-new-run-controller";

const api = vi.hoisted(() => ({ setup: vi.fn(), policies: vi.fn(), defaults: vi.fn(), workspaces: vi.fn(), me: vi.fn(), create: vi.fn(), preflight: vi.fn(), preview: vi.fn() }));
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: api.setup } }));
vi.mock("../../../lib/api/policies", () => ({ policies: { listPolicies: api.policies, getDefaultPolicy: api.defaults } }));
vi.mock("../../../lib/api/workspaces", () => ({ workspaces: { listWorkspaces: api.workspaces } }));
vi.mock("../../../lib/api/health", () => ({ health: { whoami: api.me } }));
vi.mock("../../../lib/api/runs", async (actual) => ({ ...await actual<typeof import("../../../lib/api/runs")>(), runs: { listRuns: async () => [], createRun: api.create, preflightRun: api.preflight } }));
vi.mock("../../../lib/api/policy-preview", () => ({ previewRunPolicy: api.preview }));
vi.mock("../../../lib/capabilities", () => ({ useMyCapabilities: () => null }));

const status = { platform: { os: "linux", kvm: true }, runner: { driver: "docker", confinement_classes: ["CC1", "CC2", "CC3"] }, harnesses: [] } as unknown as SetupStatus;
const policy = { min_confinement_class: "CC1", allowed_domains: [], first_use_approval: "deny_with_review" };
let owner = "alice", revision = 1, auth = 0;
function wrapper({ children }: { children: React.ReactNode }) {
  return <MemoryRouter><OperatorProvider operator principal={owner} identityRevision={revision} authGeneration={auth}>{children}</OperatorProvider></MemoryRouter>;
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}
beforeEach(() => {
  vi.clearAllMocks();
  owner = "alice"; revision = 1; auth = getAuthGeneration();
  api.setup.mockResolvedValue(status);
  api.policies.mockResolvedValue([{ id: "saved", name: "Stored", spec: { ...policy, credential_grants: [{ secret_ref: "<redacted>" }] } }]);
  api.defaults.mockResolvedValue(policy);
  api.workspaces.mockResolvedValue([]);
  api.me.mockResolvedValue({ principal: owner });
  api.create.mockResolvedValue({ id: "run" });
  api.preflight.mockResolvedValue({ enforced_confinement_class: "CC1", setup_items: [] });
  api.preview.mockResolvedValue({ spec: policy });
});

it("retains exact invalid custom bytes across saved/default mode changes", async () => {
  const { result } = renderHook(useNewRunController, { wrapper });
  await waitFor(() => expect(result.current.policiesLoaded).toBe(true));
  const source = '{ "allowed_domains": [\n';
  act(() => result.current.onSpecChange(source));
  expect(result.current.policy.merged).toBeNull();
  expect(result.current.dirty).toBe(true);
  act(() => { result.current.onPolicyModeChange("saved"); result.current.onPickPolicy("saved"); });
  expect(result.current.specText).toBe(source);
  act(() => result.current.onPolicyModeChange("default"));
  act(() => result.current.onPolicyModeChange("custom"));
  expect(result.current.specText).toBe(source);
  await act(() => result.current.launch());
  await act(() => result.current.preflight());
  expect(api.create).not.toHaveBeenCalled();
  expect(api.preflight).not.toHaveBeenCalled();
});

it("keeps an edited source and explicit class when setup finishes late", async () => {
  const late = deferred<SetupStatus>();
  api.setup.mockReturnValueOnce(late.promise);
  const { result } = renderHook(useNewRunController, { wrapper });
  const source = JSON.stringify({ ...policy, allowed_domains: ["authored.example"] }, null, 4);
  act(() => {
    result.current.onSpecChange(source);
    result.current.patch({ confinementClass: "CC2" });
    result.current.setCcTouched(true);
  });
  await act(async () => late.resolve(status));
  expect(result.current.specText).toBe(source);
  expect(result.current.cc).toBe("CC2");
});

it("machine seeding is clean, while wire-equivalent whitespace is dirty", async () => {
  const { result } = renderHook(useNewRunController, { wrapper });
  await waitFor(() => expect(result.current.probeSettled).toBe(true));
  expect(result.current.dirty).toBe(false);
  const body = result.current.currentBody;
  act(() => result.current.onSpecChange(`${result.current.specText}\n`));
  expect(result.current.currentBody).toBe(body);
  expect(result.current.dirty).toBe(true);
});

it("purges a changed principal's draft and refuses an old launch closure", async () => {
  const { result, rerender } = renderHook(useNewRunController, { wrapper });
  await waitFor(() => expect(result.current.policiesLoaded).toBe(true));
  act(() => result.current.patch({ task: "private alice task", title: "private" }));
  const oldLaunch = result.current.launch;
  owner = "bob"; revision++;
  rerender();
  expect(result.current.state.task).toBe("");
  expect(result.current.state.title).toBe("");
  await act(() => oldLaunch());
  expect(api.create).not.toHaveBeenCalled();
});

it("invalidates an unchanged draft after same-principal adoption and rejects old source reads", async () => {
  const late = deferred<SetupStatus>();
  api.setup.mockReturnValueOnce(late.promise);
  const { result, rerender } = renderHook(useNewRunController, { wrapper });
  act(() => result.current.patch({ task: "kept" }));
  act(() => notifyAuthChange());
  await act(async () => late.resolve(status));
  expect(result.current.availableClasses).toBeNull();
  expect(api.setup).toHaveBeenCalledTimes(1);
  auth = getAuthGeneration(); revision++;
  rerender();
  await waitFor(() => expect(result.current.availableClasses).toEqual(["CC1", "CC2", "CC3"]));
  expect(api.setup).toHaveBeenCalledTimes(2);
  expect(result.current.state.task).toBe("kept");
});

it("acknowledges only the submitted snapshot, so a later edit is dirty again", async () => {
  const { result } = renderHook(useNewRunController, { wrapper });
  await waitFor(() => expect(result.current.policiesLoaded).toBe(true));
  act(() => result.current.patch({ title: "authored" }));
  expect(result.current.dirty).toBe(true);
  await act(() => result.current.launch());
  expect(api.create).toHaveBeenCalledOnce();
  expect(result.current.dirty).toBe(false);
  act(() => result.current.onSpecChange(`${result.current.specText}\n`));
  expect(result.current.dirty).toBe(true);
});
