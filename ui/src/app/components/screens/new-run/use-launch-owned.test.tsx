/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { act, renderHook } from "@testing-library/react";
import { beforeEach, expect, it, vi } from "vitest";
import { getAuthGeneration } from "../../../lib/api/core";
import { registerUnsaved, unsavedSnapshot } from "../../../lib/unsaved-registry";
import { initialWizardState } from "./wizard-types";
import { useLaunch, type UseLaunchParams } from "./use-launch";

const create = vi.hoisted(() => vi.fn());
const navigate = vi.hoisted(() => vi.fn());
vi.mock("react-router-dom", async (original) => ({ ...await original<typeof import("react-router-dom")>(), useNavigate: () => navigate }));
vi.mock("../../../lib/api/runs", async (original) => ({ ...await original<typeof import("../../../lib/api/runs")>(), runs: { createRun: create, preflightRun: vi.fn() } }));
vi.mock("../../../lib/api/policy-preview", () => ({ previewRunPolicy: vi.fn() }));
function params(onCreated: () => void): UseLaunchParams {
  return {
    state: initialWizardState("CC1", { task: "owner's task", selectedPolicyId: "saved" }),
    workspaces: [], policyMode: "saved", ccTouched: false, merged: null,
    identity: { principal: "alice", resolved: true, revision: 1, authGeneration: getAuthGeneration() },
    autoCheck: { local: false, backendArm: false, modelArm: false },
    doorOpen: false, adoDoorOpen: false, externalRevision: "1", sourceRefreshPending: false, onCreated,
  };
}
beforeEach(() => { create.mockReset(); navigate.mockReset(); });

it("only 2xx releases its own recovery text, before synchronous navigation", async () => {
  const release = registerUnsaved("run-owner", () => "exact authored text\nselections");
  const other = registerUnsaved("other-owner", () => "other draft");
  navigate.mockImplementation(() => expect(unsavedSnapshot()).toBe("other draft"));
  const { result, unmount } = renderHook(useLaunch, { initialProps: params(release) });
  create.mockRejectedValueOnce(new Error("refused"));
  await act(() => result.current.launch());
  expect(unsavedSnapshot()).toBe("exact authored text\nselections\n\nother draft");
  expect(navigate).not.toHaveBeenCalled();
  create.mockResolvedValueOnce({ id: "created" });
  await act(() => result.current.launch());
  expect(navigate).toHaveBeenCalledExactlyOnceWith("/runs/created", { state: { launchWarnings: [] } });
  unmount();
  release(); other();
});

it("an old owner's success cannot release or navigate the replacement draft", async () => {
  let resolve!: (value: { id: string }) => void;
  create.mockReturnValueOnce(new Promise((done) => { resolve = done; }));
  const onCreated = vi.fn();
  const p = params(onCreated);
  const { result, rerender } = renderHook(useLaunch, { initialProps: p });
  let pending!: Promise<string | void>;
  act(() => { pending = result.current.launch(); });
  rerender({ ...p, identity: { ...p.identity, principal: "bob", revision: 2 } });
  await act(async () => { resolve({ id: "old-run" }); await pending; });
  expect(onCreated).not.toHaveBeenCalled();
  expect(navigate).not.toHaveBeenCalled();
});
