/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A held Continue (starts_here_required) focuses #nr-starts-here, so exactly one
// element on the Files step may carry it: the first included repository's radio,
// else the drive's when it is the only thing included, else the repositories section.

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { filesPreflight, MAIN_DRIVE_FACTS, SCENARIO_FILES_WORKSPACES } from "../../../../../lib/new-run-fixtures/scenarios/files";
import { LEGACY_USER_DRIVE } from "../../../../../lib/new-run-fixtures/scenarios/drives";
import { OperatorProvider } from "../../../../wardyn/operator-context";
import type { RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch } from "../../new-run-route";
import { FilesStep } from "./files-step";

const wfetchMock = vi.fn();
vi.mock("../../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));
vi.mock("../../draft/use-intent-checks", () => ({
  useDraftIdentity: () => ({ principal: "alice", resolved: true, revision: 0, authGeneration: 0 }),
  useIntentChecks: () => ({ preflightResult: filesPreflight(MAIN_DRIVE_FACTS.filesystem) }),
}));

const json = (body: unknown) => new Response(JSON.stringify(body), { headers: { "content-type": "application/json" } });
const BACKGROUND: RunIntent = { shape: "application", experience: "background", workload: { kind: "command", command: "make" } };
const REPO = { workspace_id: "ws-payments", repos: [{ repo: "github.com/acme/payments" }] };

beforeEach(() => {
  wfetchMock.mockImplementation(async (path: string) => {
    if (path.includes("/branches")) return json({ listing: "listed", branches: ["main"], default: "main", truncated: false });
    if (path.startsWith("/workspaces")) return json(SCENARIO_FILES_WORKSPACES.member);
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

function show(intent: RunIntent) {
  render(
    <MemoryRouter>
      <OperatorProvider operator={false} userDrive={LEGACY_USER_DRIVE}>
        <NewRunDraftProvider owner="alice">
          <Seed intent={intent} />
          <FilesStep location={{ screen: "step", step: "files" }} go={() => {}} startFromScratch={() => {}} />
        </NewRunDraftProvider>
      </OperatorProvider>
    </MemoryRouter>,
  );
}

const carriers = () => Array.from(document.querySelectorAll("#nr-starts-here"));

describe("the failed-Continue focus target", () => {
  it("is the repositories section when nothing is included", async () => {
    show(BACKGROUND);
    await screen.findByRole("checkbox", { name: "Include payments" });
    expect(carriers()).toHaveLength(1);
    expect(carriers()[0].tagName).toBe("SECTION");
  });

  it("is the drive's radio when the drive is the only thing included", async () => {
    show({ ...BACKGROUND, drive: { enabled: true } });
    await waitFor(() => expect(carriers()).toHaveLength(1));
    expect(carriers()[0]).toBe(screen.getByRole("radio", { name: /Mount my drive/ }));
  });

  it("is the repository's radio when a repository is included, with or without the drive", async () => {
    show({ ...BACKGROUND, workspaces: [REPO] });
    await waitFor(() => expect(carriers()).toHaveLength(1));
    expect(carriers()[0]).toBe(screen.getByRole("radio", { name: "payments Starts here" }));
    cleanup();
    show({ ...BACKGROUND, workspaces: [REPO], drive: { enabled: true } });
    await waitFor(() => expect(carriers()).toHaveLength(1));
    expect(carriers()[0]).toBe(screen.getByRole("radio", { name: "payments Starts here" }));
  });

  it("stays on the section when the drive is mounted in the draft but cannot be shown", async () => {
    cleanup();
    render(
      <MemoryRouter>
        <OperatorProvider operator={false} userDrive={null}>
          <NewRunDraftProvider owner="alice">
            <Seed intent={{ ...BACKGROUND, drive: { enabled: true } }} />
            <FilesStep location={{ screen: "step", step: "files" }} go={() => {}} startFromScratch={() => {}} />
          </NewRunDraftProvider>
        </OperatorProvider>
      </MemoryRouter>,
    );
    await screen.findByRole("checkbox", { name: "Include payments" });
    expect(carriers()).toHaveLength(1);
    expect(carriers()[0].tagName).toBe("SECTION");
  });
});
