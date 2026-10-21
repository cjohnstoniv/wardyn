/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Reuse it across runs" on the Files step (#1991): the row as the catalogue's create door draws it, the
// request field it sets, the sentences it changes, and the kept drive in the Add dialog's Choose step.

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { CREATE_ALLOWED, CREATE_DOORS, DRIVE_LEGACY, DRIVE_OWNED } from "../../../../../lib/new-run-fixtures/scenarios/drives";
import { filesPreflight, MAIN_DRIVE_FACTS, SCENARIO_FILES_WORKSPACES } from "../../../../../lib/new-run-fixtures/scenarios/files";
import type { MeDrive, MeDriveCatalogue, MemberDriveCreate } from "../../../../../lib/api/me-drives";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { AnnounceContext } from "../step-shell";
import { FilesRoute } from "./files-route";
import { keepDoor } from "./keep-work-folder";

const wfetchMock = vi.fn();
vi.mock("../../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));
const checks = vi.hoisted(() => ({ result: null as unknown }));
vi.mock("../../draft/use-intent-checks", () => ({
  useDraftIdentity: () => ({ principal: "alice", resolved: true, revision: 0, authGeneration: 0 }),
  useIntentChecks: (params: { body: unknown }) => ({ preflightResult: params.body === null ? null : checks.result }),
}));

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
const ONLY_MAIN: RunIntent = { shape: "agent_task", experience: "background", no_repositories_or_drives: true };
const OPEN: MemberDriveCreate = { ...CREATE_ALLOWED, keep_work_folder: true, enforcement: "none" };
const LIMIT: MemberDriveCreate = { ...CREATE_DOORS.limit_reached, keep_work_folder: true };
const RUN_ID = "7c1d0f4e-2b8a-4e53-9a60-1f0e5d3c8b21";
const KEPT: MeDrive = { ...DRIVE_OWNED, id: "d-kept", name: "nightly-tests 1a2b3c4d", target: "/home/agent/drives/nightly-tests", enforcement: "none", kept_from_run: RUN_ID, kept_from_run_title: "nightly-tests" };

let create: MemberDriveCreate | undefined;
let drives: MeDrive[];

beforeEach(() => {
  create = OPEN;
  drives = [DRIVE_LEGACY, KEPT];
  checks.result = filesPreflight(MAIN_DRIVE_FACTS.filesystem);
  wfetchMock.mockImplementation(async (path: string) => {
    if (path.startsWith("/workspaces")) return json(SCENARIO_FILES_WORKSPACES.member);
    if (path.startsWith("/setup/status")) return json({ runner: { driver: "", confinement_classes: [], kubernetes: false } });
    if (path.startsWith("/me/drives")) return json({ drives, pool_checked: true, ...(create ? { create } : {}) } satisfies MeDriveCatalogue);
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
const Drawn = () => <p data-testid="intent">{JSON.stringify(useNewRunDraft().intent)}</p>;
const drawn = (): RunIntent => JSON.parse(screen.getByTestId("intent").textContent!);

function show(intent: RunIntent = ONLY_MAIN) {
  render(
    <MemoryRouter>
      <AnnounceContext.Provider value={vi.fn()}>
        <NewRunDraftProvider owner="alice">
          <Seed intent={intent} />
          <FilesRoute location={{ screen: "step", step: "files" }} go={vi.fn()} startFromScratch={() => {}} />
          <Drawn />
        </NewRunDraftProvider>
      </AnnounceContext.Provider>
    </MemoryRouter>,
  );
}
const tick = () => screen.getByLabelText(NR_FILES.REUSE_LABEL) as HTMLInputElement;
/** Waits for the catalogue's answer to reach the row (the tick leaves its Coming soon state). */
const settled = () => waitFor(() => expect(screen.queryByText(NR_SHARED.COMING_SOON)).toBeNull());

describe("keepDoor", () => {
  it("is today's row until the switch is on, or the draft already asks", () => {
    expect(keepDoor(undefined, false)).toEqual({ kind: "today" });
    expect(keepDoor(CREATE_ALLOWED, false)).toEqual({ kind: "today" });
    expect(keepDoor(CREATE_DOORS.disabled, false)).toEqual({ kind: "today" });
    // A rerun that already keeps the folder while the keep switch is off: closed, and the tick says why.
    // Creating drives is allowed there, so the sentence is the keep switch's, never "can't create drives".
    expect(keepDoor(CREATE_ALLOWED, true)).toEqual({ kind: "closed", reason: NR_FILES.KEEP_DISABLED });
    expect(keepDoor(CREATE_DOORS.disabled, true)).toEqual({ kind: "closed", reason: NR_FILES.CREATE_DISABLED });
    expect(keepDoor(undefined, true)).toEqual({ kind: "closed", reason: undefined });
  });

  it("is open when the switch is on and the person may create, and says Docker's size is not enforced", () => {
    expect(keepDoor(OPEN, false)).toEqual({ kind: "open", count: 1, limit: 3, unenforced: true });
    expect(keepDoor({ ...OPEN, enforcement: "filesystem" }, false)).toMatchObject({ kind: "open", unenforced: false });
  });

  it("is closed with the limit sentence at the limit, and with the organisation's sentence when member drives are off", () => {
    expect(keepDoor(LIMIT, false)).toEqual({ kind: "closed", reason: NR_FILES.KEEP_LIMIT(3, 3) });
    expect(keepDoor({ ...CREATE_DOORS.disabled, keep_work_folder: true }, false)).toEqual({ kind: "closed", reason: NR_FILES.CREATE_DISABLED });
    // A draft that already asked meets the limit even when the catalogue does not say the switch is on.
    expect(keepDoor(CREATE_DOORS.limit_reached, true)).toEqual({ kind: "closed", reason: NR_FILES.KEEP_LIMIT(3, 3) });
  });
});

describe("the reuse row", () => {
  it("with the switch off is today's disabled tick and Coming soon", async () => {
    create = CREATE_ALLOWED;
    show();
    await waitFor(() => expect(wfetchMock.mock.calls.some(([path]) => String(path).startsWith("/me/drives"))).toBe(true));
    expect(tick()).toBeDisabled();
    expect(screen.getByText(NR_SHARED.COMING_SOON)).toBeInTheDocument();
    expect(screen.queryByTestId("nr-keep-work-folder")).toBeNull();
  });

  it("ticks into the request, explains what is kept and what comes back, and unticks to nothing", async () => {
    show();
    await settled();
    expect(tick()).toBeEnabled();
    expect(tick()).not.toBeChecked();
    expect(drawn().keep_work_folder).toBeUndefined();
    expect(screen.queryByTestId("nr-keep-work-folder")).toBeNull();

    await userEvent.click(tick());
    expect(drawn().keep_work_folder).toBe(true);
    const box = await screen.findByTestId("nr-keep-work-folder");
    expect(box).toHaveTextContent(NR_FILES.KEEP_WHAT);
    expect(within(box).getByRole("note")).toHaveTextContent(NR_FILES.KEEP_WARNING);
    expect(box).toHaveTextContent(`${NR_FILES.KEEP_COUNTS(1, 3)} ${NR_FILES.KEEP_UNENFORCED}`);
    // What happens to it, and the sentence under the step.
    expect(screen.getByText(NR_FILES.KEEP_LIFE)).toBeInTheDocument();
    // The sentence under the step settles half a second after the last change.
    await userEvent.type(screen.getByLabelText(NR_FILES.SIZE_LABEL), "4");
    await userEvent.tab();
    await waitFor(
      () => expect(screen.getByTestId("nr-files-summary")).toHaveTextContent(/^Main drive 4 GiB · starts in the main drive · the work folder is kept as a drive after the run$/),
      { timeout: 2000 },
    );

    await userEvent.click(tick());
    expect(drawn()).not.toHaveProperty("keep_work_folder");
    expect(screen.queryByTestId("nr-keep-work-folder")).toBeNull();
    expect(screen.getByText(NR_FILES.LIFE_DELETED)).toBeInTheDocument();
  });

  it("leaves the size sentence out where the runner enforces it", async () => {
    create = { ...OPEN, enforcement: "filesystem" };
    show({ ...ONLY_MAIN, keep_work_folder: true });
    const box = await screen.findByTestId("nr-keep-work-folder");
    expect(box).toHaveTextContent(NR_FILES.KEEP_COUNTS(1, 3));
    expect(box).not.toHaveTextContent(NR_FILES.KEEP_UNENFORCED);
  });

  it("is closed, with the organisation's sentence, when member drives are off", async () => {
    create = { ...CREATE_DOORS.disabled, keep_work_folder: true };
    show();
    expect(await screen.findByText(NR_FILES.CREATE_DISABLED)).toBeInTheDocument();
    expect(tick()).toBeDisabled();
    expect(tick()).toHaveAccessibleDescription(NR_FILES.CREATE_DISABLED);
    expect(screen.queryByText(NR_SHARED.COMING_SOON)).toBeNull();
  });

  it("is closed at the drive limit with the count and what to do", async () => {
    create = LIMIT;
    show();
    expect(await screen.findByText(NR_FILES.KEEP_LIMIT(3, 3))).toBeInTheDocument();
    expect(tick()).toBeDisabled();
    expect(tick()).toHaveAccessibleDescription(NR_FILES.KEEP_LIMIT(3, 3));
  });

  it("keeps a draft that already asked untickable at the limit", async () => {
    create = LIMIT;
    show({ ...ONLY_MAIN, keep_work_folder: true });
    await screen.findByText(NR_FILES.KEEP_LIMIT(3, 3));
    expect(tick()).toBeEnabled();
    expect(tick()).toBeChecked();
    expect(screen.queryByTestId("nr-keep-work-folder")).toBeNull();
    await userEvent.click(tick());
    expect(drawn()).not.toHaveProperty("keep_work_folder");
  });
});

describe("a kept work folder in the Choose step", () => {
  async function openChoose() {
    show();
    await userEvent.click(await screen.findByRole("button", { name: NR_FILES.ADD_ARIA }));
    const dialog = screen.getByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: new RegExp(`^${NR_FILES.KIND_USER_DRIVE}`) }));
    return dialog;
  }

  it("names its run, says it stays until deleted, and on Docker that its size is not enforced", async () => {
    const dialog = await openChoose();
    const option = (await within(dialog).findByRole("radio", { name: /^nightly-tests 1a2b3c4d/ })).closest("label")!;
    expect(option).toHaveTextContent(NR_FILES.DRIVE_KEPT_FROM("nightly-tests"));
    expect(within(option).getByText(NR_FILES.DRIVE_SIZE_NOT_ENFORCED)).toBeInTheDocument();
    expect(option).toHaveTextContent("/home/agent/drives/nightly-tests");
    await userEvent.click(within(option).getByRole("button", { name: NR_FILES.DRIVE_SIZE_NOT_ENFORCED }));
    expect(await screen.findByText(NR_FILES.DRIVE_SIZE_NOT_ENFORCED_TIP)).toBeInTheDocument();
  });

  it("shows the run's title, never its id, and a short neutral line when the run is gone", async () => {
    drives = [DRIVE_LEGACY, KEPT, { ...KEPT, id: "d-gone", name: "old-run 5e6f7a8b", kept_from_run_title: undefined }];
    const dialog = await openChoose();
    const kept = (await within(dialog).findByRole("radio", { name: /^nightly-tests 1a2b3c4d/ })).closest("label")!;
    expect(kept).toHaveTextContent(NR_FILES.DRIVE_KEPT_FROM("nightly-tests"));
    expect(kept).not.toHaveTextContent(RUN_ID);
    const gone = (await within(dialog).findByRole("radio", { name: /^old-run 5e6f7a8b/ })).closest("label")!;
    expect(gone).toHaveTextContent(NR_FILES.DRIVE_KEPT_FROM_UNTITLED);
    expect(gone).not.toHaveTextContent(RUN_ID);
  });

  it("lists a drive that is not a kept folder as it always was", async () => {
    drives = [DRIVE_LEGACY, DRIVE_OWNED];
    const dialog = await openChoose();
    const option = (await within(dialog).findByRole("radio", { name: new RegExp(`^${DRIVE_OWNED.name}`) })).closest("label")!;
    expect(option).not.toHaveTextContent("Kept until you delete it");
    expect(within(option).queryByText(NR_FILES.DRIVE_SIZE_NOT_ENFORCED)).toBeNull();
  });
});
