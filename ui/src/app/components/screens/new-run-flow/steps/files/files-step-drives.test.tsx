/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, Files: where a shared drive mounts from (M10). The server says where
// each shared drive mounts from; in a pool holding the runner a share is bound
// to, it says that runner and the path there. A drive that cannot mount in this
// pool is closed with the server's reason as its sentence.
import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { filesPreflight, MAIN_DRIVE_FACTS, SCENARIO_FILES_WORKSPACES } from "../../../../../lib/new-run-fixtures/scenarios/files";
import type { MeDrive, MeDriveCatalogue } from "../../../../../lib/api/me-drives";
import { DRIVE_MEMBER } from "../../../../../lib/user-drives-copy";
import type { RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch } from "../../new-run-route";
import { FilesStep } from "./files-step";

const wfetchMock = vi.fn();
vi.mock("../../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));
const checks = vi.hoisted(() => ({ result: null as unknown }));
vi.mock("../../draft/use-intent-checks", () => ({
  useDraftIdentity: () => ({ principal: "alice", resolved: true, revision: 0, authGeneration: 0 }),
  useIntentChecks: (params: { body: Record<string, unknown> | null }) => ({ preflightResult: params.body === null ? null : checks.result }),
}));

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
const TEAM_SHARE: MeDrive = {
  id: "22222222-2222-4222-8222-222222222222", name: "Data team share", section: "shared", source: "team", backend: "host_path",
  target: "/home/agent/shared/data-team-share", writable: false, enforcement: "none", legacy: false, owned: false, team: false,
};
const FINANCE: MeDrive = { ...TEAM_SHARE, id: "33333333-3333-4333-8333-333333333333", name: "Finance archive", target: "/home/agent/shared/finance-archive", unavailable_reason: "not_in_pool" };
const BACKGROUND: RunIntent = { shape: "application", experience: "background", workload: { kind: "command", command: "make" } };

let catalogue: MeDriveCatalogue;
beforeEach(() => {
  catalogue = { drives: [TEAM_SHARE, FINANCE], pool_checked: true };
  checks.result = filesPreflight(MAIN_DRIVE_FACTS.filesystem);
  wfetchMock.mockImplementation(async (path: string) => {
    if (path.startsWith("/workspaces")) return json(SCENARIO_FILES_WORKSPACES.member);
    if (path.startsWith("/me/drives")) return json(catalogue);
    if (path.startsWith("/setup/status")) return json({ runner: { driver: "", confinement_classes: [], kubernetes: false, computer_folders: false } });
    if (path.startsWith("/me/runners")) return json([]);
    throw new Error(`unexpected ${path}`);
  });
});
afterEach(() => {
  cleanup();
  wfetchMock.mockReset();
});

function Seed({ intent }: { intent: RunIntent }) {
  const dispatch = useNewRunDispatch();
  React.useEffect(() => dispatch({ type: "seed", origin: { kind: "scratch" }, intent }), [dispatch, intent]);
  return null;
}
function show(intent: RunIntent = BACKGROUND) {
  render(
    <MemoryRouter>
      <NewRunDraftProvider owner="alice">
        <Seed intent={intent} />
        <FilesStep location={{ screen: "step", step: "files" }} go={() => {}} startFromScratch={() => {}} />
      </NewRunDraftProvider>
    </MemoryRouter>,
  );
}
const dialog = () => screen.getByRole("dialog");
async function openShared() {
  await userEvent.click(await screen.findByRole("button", { name: NR_FILES.ADD_ARIA }));
  await userEvent.click(within(dialog()).getByRole("button", { name: new RegExp(`^${NR_FILES.KIND_SHARED_DRIVE}`) }));
}

describe("where a shared drive mounts from (M10)", () => {
  it("says where a shared drive mounts from, the bound runner and path on it, and closes one bound elsewhere", async () => {
    catalogue = { pool_checked: true, drives: [
      { ...TEAM_SHARE, name: "team-share", mounts_from: "Organisation runners and mbp-dev", mounted_from: { runner: "mbp-dev", path: "/Volumes/team-share" } },
      { ...FINANCE, mounts_from: "Organisation runners", unavailable_reason: "bound_elsewhere" },
      { ...TEAM_SHARE, id: "44444444-4444-4444-8444-444444444444", name: "Docs share", target: "/home/agent/shared/docs", mounts_from: "Organisation runners and mbp-dev" },
    ] };
    show();
    await openShared();
    const lines = await within(dialog()).findAllByTestId("nr-drive-mounts");
    expect(lines.map((l) => l.textContent)).toEqual(["Mounted from mbp-dev, at /Volumes/team-share.", "Mounts from organisation runners and mbp-dev."]);
    expect(within(dialog()).getByRole("radio", { name: /^Finance archive/ })).toBeDisabled();
    expect(within(dialog()).getByText(DRIVE_MEMBER.NR_BOUND_ELSEWHERE("Finance archive"))).toBeInTheDocument();
  });

  it("closes an organisation drive that is not in this pool with the organisation-runners sentence", async () => {
    show();
    await openShared();
    expect(await within(dialog()).findByRole("radio", { name: /^Finance archive/ })).toBeDisabled();
    expect(within(dialog()).getByText(DRIVE_MEMBER.NR_ORG_RUNNERS_ONLY("Finance archive"))).toBeInTheDocument();
  });
});
