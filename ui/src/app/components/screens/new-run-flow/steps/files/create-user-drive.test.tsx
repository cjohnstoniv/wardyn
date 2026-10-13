/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The person-created drives arm in the Files step's Add dialog: the create door
// as the catalogue reports it, create, and delete of the person's own drives.

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { CREATE_ALLOWED, CREATE_DOORS, DRIVE_LEGACY, DRIVE_LOCKED, DRIVE_OWNED } from "../../../../../lib/new-run-fixtures/scenarios/drives";
import { filesPreflight, MAIN_DRIVE_FACTS, SCENARIO_FILES_WORKSPACES } from "../../../../../lib/new-run-fixtures/scenarios/files";
import { DRIVE_REFUSAL } from "../../../../../lib/drive-refusals";
import type { MeDrive, MeDriveCatalogue, MemberDriveCreate, MemberDriveCreateReason } from "../../../../../lib/api/me-drives";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { AnnounceContext } from "../step-shell";
import { FilesRoute } from "./files-route";

const wfetchMock = vi.fn();
vi.mock("../../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));
// The shared checks hook is faked: it owns debounce and identity fencing, which are its own tests'.
const checks = vi.hoisted(() => ({ result: null as unknown }));
vi.mock("../../draft/use-intent-checks", () => ({
  useDraftIdentity: () => ({ principal: "alice", resolved: true, revision: 0, authGeneration: 0 }),
  useIntentChecks: (params: { body: unknown }) => ({ preflightResult: params.body === null ? null : checks.result }),
}));

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
const INTERACTIVE: RunIntent = { shape: "environment", experience: "interactive" };
const NEW_DRIVE: MeDrive = { ...DRIVE_OWNED, id: "d-new", name: "Case notes" };

let create: MemberDriveCreate | undefined;
let drives: MeDrive[];
let post: (body: { name: string }) => Response;
let del: (id: string) => Response;
const announce = vi.fn();

beforeEach(() => {
  create = CREATE_ALLOWED;
  drives = [DRIVE_LEGACY, DRIVE_OWNED, DRIVE_LOCKED];
  post = (body) => {
    drives = [...drives, { ...NEW_DRIVE, name: body.name }];
    return json({ ...NEW_DRIVE, name: body.name }, 201);
  };
  del = (id) => {
    drives = drives.filter((d) => d.id !== id);
    return new Response(null, { status: 204 });
  };
  announce.mockReset();
  checks.result = filesPreflight(MAIN_DRIVE_FACTS.filesystem);
  wfetchMock.mockImplementation(async (path: string, init?: { method?: string; body?: string }) => {
    const method = init?.method ?? "GET";
    if (path.startsWith("/workspaces")) return json(SCENARIO_FILES_WORKSPACES.member);
    if (path.startsWith("/setup/status")) return json({ runner: { driver: "", confinement_classes: [], kubernetes: false } });
    if (path === "/me/drives" && method === "POST") return post(JSON.parse(init!.body!));
    if (path.startsWith("/me/drives/") && method === "DELETE") return del(decodeURIComponent(path.slice("/me/drives/".length)));
    if (path.startsWith("/me/drives")) {
      const body: MeDriveCatalogue = { drives, pool_checked: true, ...(create ? { create } : {}) };
      return json(body);
    }
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

function show(intent: RunIntent = INTERACTIVE) {
  render(
    <MemoryRouter>
      <AnnounceContext.Provider value={announce}>
        <NewRunDraftProvider owner="alice">
          <Seed intent={intent} />
          <FilesRoute location={{ screen: "step", step: "files" }} go={vi.fn()} startFromScratch={() => {}} />
          <Drawn />
        </NewRunDraftProvider>
      </AnnounceContext.Provider>
    </MemoryRouter>,
  );
}
// The dialog under a nested one is hidden from the accessibility tree, so the stack is read with hidden: true.
const dialogs = () => screen.getAllByRole("dialog", { hidden: true });
const top = () => dialogs()[dialogs().length - 1];
/** Opens the Add dialog on the user drives' Choose step. */
async function openChoose(intent?: RunIntent) {
  show(intent);
  await userEvent.click(await screen.findByRole("button", { name: NR_FILES.ADD_ARIA }));
  await userEvent.click(within(top()).getByRole("button", { name: new RegExp(`^${NR_FILES.KIND_USER_DRIVE}`) }));
  await within(top()).findByRole("radio", { name: /^Team reference/ });
}
const createLink = () => within(top()).getByRole("button", { name: NR_FILES.CREATE_USER_DRIVE });
const nameField = () => within(top()).getByLabelText(NR_FILES.DRIVE_NAME);
const posts = () => wfetchMock.mock.calls.filter(([, init]) => init?.method === "POST");
const deletes = () => wfetchMock.mock.calls.filter(([, init]) => init?.method === "DELETE");

describe("the create door", () => {
  it("offers + Create user drive when the catalogue says the person may", async () => {
    await openChoose();
    expect(createLink()).toBeEnabled();
    expect(within(top()).queryByText(/can't create drives/)).toBeNull();
  });

  it.each<[MemberDriveCreateReason, string | undefined]>([
    ["denied_by_profile", NR_FILES.CREATE_DENIED("Contractors")],
    ["limit_reached", NR_FILES.CREATE_LIMIT(3, 3)],
    ["unsupported", NR_FILES.CREATE_UNSUPPORTED],
    ["delete_unavailable", NR_FILES.CREATE_NO_DELETE],
    // No canon string for these two: the control is closed and nothing is worded for it.
    ["governance_unavailable", undefined],
    ["groups_snapshot_stale", undefined],
  ])("closes the control with its reason beside it: %s", async (reason, sentence) => {
    create = CREATE_DOORS[reason];
    await openChoose();
    const control = createLink();
    expect(control).toBeDisabled();
    if (sentence) {
      expect(within(top()).getByText(sentence)).toBeInTheDocument();
      expect(control).toHaveAccessibleDescription(sentence);
    } else {
      expect(control).not.toHaveAttribute("aria-describedby");
    }
  });

  it("shows nothing when person-created drives are off, or the caller is not a person", async () => {
    create = CREATE_DOORS.disabled;
    await openChoose();
    expect(within(top()).queryByRole("button", { name: NR_FILES.CREATE_USER_DRIVE })).toBeNull();
    expect(within(top()).queryByTestId("nr-own-drives")).toBeNull();
    cleanup();
    create = undefined;
    await openChoose();
    expect(within(top()).queryByRole("button", { name: NR_FILES.CREATE_USER_DRIVE })).toBeNull();
  });
});

describe("create", () => {
  it("creates the drive, chooses it, reads the catalogue again and announces it", async () => {
    await openChoose();
    await userEvent.click(createLink());
    expect(dialogs()).toHaveLength(2);
    expect(within(top()).getByRole("heading", { name: NR_FILES.CREATE_TITLE })).toBeInTheDocument();
    expect(nameField()).toHaveFocus();
    await userEvent.type(nameField(), "Case notes");
    await userEvent.click(within(top()).getByRole("button", { name: NR_FILES.CREATE_CONFIRM }));
    await waitFor(() => expect(dialogs()).toHaveLength(1));
    expect(JSON.parse(posts()[0][1].body)).toEqual({ name: "Case notes" });
    expect(await within(top()).findByRole("radio", { name: /^Case notes/ })).toBeChecked();
    expect(announce).toHaveBeenCalledWith(NR_FILES.DRIVE_CREATED("Case notes"));
    expect(createLink()).toHaveFocus();
  });

  it("keeps the dialog open with the server's sentence under the field when the create is refused", async () => {
    const sentence = DRIVE_REFUSAL.MEMBER_NAME_TAKEN("Case notes");
    post = () => json({ error: sentence, reason: "member_drive_name_taken" }, 422);
    await openChoose();
    await userEvent.click(createLink());
    await userEvent.type(nameField(), "Case notes");
    await userEvent.click(within(top()).getByRole("button", { name: NR_FILES.CREATE_CONFIRM }));
    expect(await within(top()).findByRole("alert")).toHaveTextContent(sentence);
    expect(dialogs()).toHaveLength(2);
    expect(nameField()).toHaveAttribute("aria-invalid", "true");
    expect(announce).not.toHaveBeenCalledWith(NR_FILES.DRIVE_CREATED("Case notes"));
  });

  it("closes only the create dialog on Escape and gives focus back to the control that opened it", async () => {
    await openChoose();
    await userEvent.click(createLink());
    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(dialogs()).toHaveLength(1));
    expect(within(top()).getByRole("radio", { name: /^Team reference/ })).toBeInTheDocument();
    expect(createLink()).toHaveFocus();
    expect(posts()).toHaveLength(0);
  });
});

describe("delete", () => {
  const ownRow = () => within(screen.getByTestId("nr-own-drives"));

  it("offers Delete drive on the person's own drives only", async () => {
    await openChoose();
    expect(ownRow().getAllByRole("button", { name: NR_FILES.DELETE_DRIVE })).toHaveLength(1);
    expect(ownRow().getByText(DRIVE_OWNED.name)).toBeInTheDocument();
    expect(ownRow().queryByText(DRIVE_LOCKED.name)).toBeNull();
    expect(ownRow().queryByText(DRIVE_LEGACY.name)).toBeNull();
  });

  it("asks first, focuses Cancel, and leaves the drive when cancelled", async () => {
    await openChoose();
    await userEvent.click(ownRow().getByRole("button", { name: NR_FILES.DELETE_DRIVE }));
    expect(within(top()).getByText(NR_FILES.DELETE_CONFIRM(DRIVE_OWNED.name))).toBeInTheDocument();
    expect(within(top()).getByRole("button", { name: NR_SHARED.CANCEL })).toHaveFocus();
    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(dialogs()).toHaveLength(1));
    expect(deletes()).toHaveLength(0);
    expect(ownRow().getByRole("button", { name: NR_FILES.DELETE_DRIVE })).toHaveFocus();
  });

  it("deletes the drive, drops its option and announces it", async () => {
    await openChoose();
    await userEvent.click(ownRow().getByRole("button", { name: NR_FILES.DELETE_DRIVE }));
    await userEvent.click(within(top()).getByRole("button", { name: NR_FILES.DELETE_DRIVE }));
    await waitFor(() => expect(dialogs()).toHaveLength(1));
    expect(deletes()[0][0]).toBe(`/me/drives/${DRIVE_OWNED.id}`);
    await waitFor(() => expect(within(top()).queryByRole("radio", { name: new RegExp(`^${DRIVE_OWNED.name}`) })).toBeNull());
    expect(screen.queryByTestId("nr-own-drives")).toBeNull();
    expect(announce).toHaveBeenCalledWith(NR_FILES.DRIVE_DELETED(DRIVE_OWNED.name));
    expect(createLink()).toHaveFocus();
  });

  it("keeps the drive and shows the server's sentence when the delete is refused", async () => {
    del = () => json({ error: DRIVE_REFUSAL.MEMBER_IN_USE(), reason: "member_drive_in_use" }, 409);
    await openChoose();
    await userEvent.click(ownRow().getByRole("button", { name: NR_FILES.DELETE_DRIVE }));
    await userEvent.click(within(top()).getByRole("button", { name: NR_FILES.DELETE_DRIVE }));
    expect(await within(top()).findByRole("alert")).toHaveTextContent(DRIVE_REFUSAL.MEMBER_IN_USE());
    expect(dialogs()).toHaveLength(2);
    expect(announce).not.toHaveBeenCalledWith(NR_FILES.DRIVE_DELETED(DRIVE_OWNED.name));
  });

  it("takes a drive already attached to this run, and a start on it, out of the draft", async () => {
    await openChoose({ ...INTERACTIVE, drives: [{ drive_id: DRIVE_OWNED.id }, { drive_id: DRIVE_LOCKED.id }], start_folder: { kind: "attachment", drive_id: DRIVE_OWNED.id } });
    expect(drawn().start_folder).toEqual({ kind: "attachment", drive_id: DRIVE_OWNED.id });
    await userEvent.click(ownRow().getByRole("button", { name: NR_FILES.DELETE_DRIVE }));
    await userEvent.click(within(top()).getByRole("button", { name: NR_FILES.DELETE_DRIVE }));
    await waitFor(() => expect(drawn().drives).toEqual([{ drive_id: DRIVE_LOCKED.id }]));
    expect(drawn().start_folder).toBeUndefined();
  });
});
