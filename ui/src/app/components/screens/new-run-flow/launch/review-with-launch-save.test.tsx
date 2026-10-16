/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The mounted Review route supplies the saver's own drives to Save as (DR-0221):
// a shared template leaves them, and a start folder on one, out.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { setField } from "../../../../../test/set-field";
import { SCENARIO_PREFLIGHT } from "../../../../lib/new-run-fixtures/scenarios/preflight";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { NR_SAVE } from "../../../wardyn/copy/new-run/save";
import { OperatorProvider } from "../../../wardyn/operator-context";
import type { RunIntent } from "../draft/run-intent";
import { NEW_RUN_SCREENS, NewRunRoute, type Seed, type SeedLoaders } from "../new-run-route";
import { ReviewWithLaunch } from "./review-with-launch";

const { wfetchMock } = vi.hoisted(() => ({ wfetchMock: vi.fn() }));
vi.mock("../draft/use-intent-checks", () => ({
  useDraftIdentity: () => ({ principal: "alice", resolved: true, revision: 1, authGeneration: 0 }),
  useIntentChecks: () => ({
    generation: 5, preflightRead: { generation: 5, at: Date.now() }, preflight: vi.fn(), preflightResult: SCENARIO_PREFLIGHT.member,
    preflightError: null, preflightReason: "", preview: { result: null, retry: vi.fn() },
  }),
}));
vi.mock("../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));

const MINE = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const TEAM = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb";
const INTENT = {
  shape: "environment", experience: "interactive", ends_after_sec: 3600,
  drives: [{ drive_id: MINE }, { drive_id: TEAM }],
  start_folder: { kind: "attachment", drive_id: MINE },
} as unknown as RunIntent;
const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } });
const drive = (id: string, owned: boolean) => ({ id, name: id.slice(0, 4), section: owned ? "user" : "shared", source: "personal", backend: "local", target: "/d", writable: true, enforcement: "enforced", legacy: false, owned });

function mount(catalogue: () => Promise<Response>) {
  const loaders: SeedLoaders = {
    template: async (): Promise<Seed> => ({ origin: { kind: "scratch" }, intent: INTENT }),
    again: async (): Promise<Seed> => ({ origin: { kind: "scratch" }, intent: INTENT }),
  };
  wfetchMock.mockImplementation(async (path: string, init?: { method?: string }) => {
    if (path === "/templates" && init?.method === "POST") return json({ id: "t9", kind: "template" });
    if (path === "/templates") return json({ templates: [], publish: { org: false, groups: ["g-1"], group_labels: { "g-1": "Platform" }, groups_unverified: false } });
    if (path.startsWith("/me/drives")) return catalogue();
    throw new Error(`unexpected ${path}`);
  });
  render(
    <MemoryRouter initialEntries={["/runs/new?again=11111111-1111-4111-8111-111111111111"]}>
      <OperatorProvider operator={false} principal="alice">
        <Routes>
          <Route path="/runs/new" element={<NewRunRoute loaders={loaders} screens={{ ...NEW_RUN_SCREENS, review: ReviewWithLaunch }} />} />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>,
  );
}

async function saveShared() {
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: NR_REVIEW.SAVE_TEMPLATE }));
  const dialog = await screen.findByRole("dialog");
  await user.selectOptions(await within(dialog).findByLabelText("Available to"), "Platform group");
  setField(within(dialog).getByLabelText(NR_SAVE.NAME_LABEL), "Team");
  await user.click(within(dialog).getByRole("button", { name: NR_SAVE.CONFIRM_TEMPLATE }));
}
const posted = () => {
  const call = wfetchMock.mock.calls.find(([p, i]) => p === "/templates" && (i as { method?: string } | undefined)?.method === "POST");
  return call ? (JSON.parse((call[1] as { body: string }).body).document.intent as RunIntent) : undefined;
};

beforeEach(() => {
  wfetchMock.mockReset();
});
afterEach(cleanup);

describe("Review route Save as and the saver's own drives (DR-0221)", () => {
  it("omits an own drive and the start folder on it from a shared template", async () => {
    mount(async () => json({ drives: [drive(MINE, true), drive(TEAM, false)], pool_checked: true }));
    await screen.findByRole("button", { name: NR_REVIEW.SAVE_TEMPLATE });
    await vi.waitFor(() => expect(wfetchMock.mock.calls.some(([p]) => String(p).startsWith("/me/drives"))).toBe(true));
    await saveShared();
    await vi.waitFor(() => expect(posted()).toBeDefined());
    const intent = posted() as RunIntent;
    expect(intent.drives).toEqual([{ drive_id: TEAM }]);
    expect(intent.start_folder).toBeUndefined();
  });
});
