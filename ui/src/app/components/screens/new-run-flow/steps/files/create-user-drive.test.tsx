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
import { MEMBER } from "../../../../../lib/governance-copy";
import { DRIVE_MEMBER } from "../../../../../lib/user-drives-copy";
import type { MeDrive, MeDriveCatalogue, MemberDriveCreate, MemberDriveCreateReason } from "../../../../../lib/api/me-drives";
import { Dialog, DialogContent, DialogTitle } from "../../../../ui/dialog";
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
let post: (body: { name: string }) => Response | Promise<Response>;
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
/** Opens the Add dialog on the user drives' Choose step and waits for the person's own drives list. */
async function openOwn() {
  show();
  await userEvent.click(await screen.findByRole("button", { name: NR_FILES.ADD_ARIA }));
  await userEvent.click(within(top()).getByRole("button", { name: new RegExp(`^${NR_FILES.KIND_USER_DRIVE}`) }));
  await screen.findByTestId("nr-own-drives");
}
const createLink = () => within(top()).getByRole("button", { name: NR_FILES.CREATE_USER_DRIVE });
const posts = () => wfetchMock.mock.calls.filter(([, init]) => init?.method === "POST");
const deletes = () => wfetchMock.mock.calls.filter(([, init]) => init?.method === "DELETE");

describe("the create door", () => {
  it("offers + Create user drive when the catalogue says the person may", async () => {
    await openChoose();
    expect(createLink()).toBeEnabled();
    expect(within(top()).queryByText(/can't create drives/)).toBeNull();
  });

  it.each<[MemberDriveCreateReason, string]>([
    ["denied_by_profile", NR_FILES.CREATE_DENIED("Contractors")],
    ["limit_reached", NR_FILES.CREATE_LIMIT(3, 3)],
    ["unsupported", NR_FILES.CREATE_UNSUPPORTED],
    ["delete_unavailable", NR_FILES.CREATE_NO_DELETE],
    // No CREATE_* string for these two: the step's own sentence for the same cause.
    ["governance_unavailable", DRIVE_MEMBER.NR_GOVERNANCE_UNAVAILABLE],
    ["groups_snapshot_stale", MEMBER.DENIED_STALE_GROUPS],
  ])("closes the control with its reason beside it: %s", async (reason, sentence) => {
    create = CREATE_DOORS[reason];
    await openChoose();
    const control = createLink();
    expect(control).toBeDisabled();
    expect(within(top()).getByText(sentence)).toBeInTheDocument();
    expect(control).toHaveAccessibleDescription(sentence);
  });

  it("shows no create control when person-created drives are off, or the caller is not a person", async () => {
    create = CREATE_DOORS.disabled;
    drives = [DRIVE_LEGACY, DRIVE_LOCKED];
    await openChoose();
    expect(within(top()).queryByRole("button", { name: NR_FILES.CREATE_USER_DRIVE })).toBeNull();
    expect(within(top()).queryByTestId("nr-own-drives")).toBeNull();
    cleanup();
    create = undefined;
    await openChoose();
    expect(within(top()).queryByRole("button", { name: NR_FILES.CREATE_USER_DRIVE })).toBeNull();
  });

  it("still lists the person's own drives for deleting when person-created drives are off", async () => {
    create = CREATE_DOORS.disabled;
    await openChoose();
    expect(within(top()).queryByRole("button", { name: NR_FILES.CREATE_USER_DRIVE })).toBeNull();
    expect(within(screen.getByTestId("nr-own-drives")).getByRole("button", { name: NR_FILES.DELETE_DRIVE })).toBeInTheDocument();
  });
});

describe("create", () => {
  const cardName = () => within(top()).getByLabelText(NR_FILES.DRIVE_NAME);
  const createButton = () => within(top()).getByRole("button", { name: NR_FILES.CREATE_CONFIRM });

  it("opens a card in the Choose step, not a second dialog, with the intro and the name field focused", async () => {
    await openChoose();
    await userEvent.click(createLink());
    expect(dialogs()).toHaveLength(1);
    expect(within(top()).getByText(NR_FILES.CREATE_INTRO)).toBeInTheDocument();
    expect(within(top()).getByText(NR_SHARED.REQUIRED)).toBeInTheDocument();
    expect(cardName()).toHaveFocus();
    expect(cardName()).toHaveAttribute("placeholder", NR_FILES.DRIVE_NAME_PLACEHOLDER);
    expect(within(top()).queryByRole("button", { name: NR_FILES.CREATE_USER_DRIVE })).toBeNull();
  });

  it("creates the drive, chooses it, reads the catalogue again and announces it", async () => {
    await openChoose();
    await userEvent.click(createLink());
    await userEvent.type(cardName(), "Case notes");
    await userEvent.click(createButton());
    expect(await within(top()).findByRole("radio", { name: /^Case notes/ })).toBeChecked();
    expect(JSON.parse(posts()[0][1].body)).toEqual({ name: "Case notes" });
    expect(within(top()).queryByLabelText(NR_FILES.DRIVE_NAME)).toBeNull();
    expect(announce).toHaveBeenCalledWith(NR_FILES.DRIVE_CREATED("Case notes"));
    expect(createLink()).toHaveFocus();
  });

  it("exposes the field as required", async () => {
    await openChoose();
    await userEvent.click(createLink());
    expect(cardName()).toBeRequired();
  });

  it("cannot be cancelled once the create is sent: Cancel is disabled, Escape does nothing, focus stays in the field", async () => {
    let release!: (r: Response) => void;
    post = () => new Promise<Response>((resolve) => { release = resolve; });
    await openChoose();
    await userEvent.click(createLink());
    await userEvent.type(cardName(), "Case notes");
    await userEvent.click(createButton());
    const card = cardName().closest("form")!;
    expect(within(card).getByRole("button", { name: NR_SHARED.CANCEL })).toBeDisabled();
    expect(cardName()).toHaveFocus();
    await userEvent.keyboard("{Escape}");
    expect(cardName()).toBeInTheDocument();
    expect(dialogs()).toHaveLength(1);
    drives = [...drives, { ...NEW_DRIVE, name: "Case notes" }];
    release(json({ ...NEW_DRIVE, name: "Case notes" }, 201));
    expect(await within(top()).findByRole("radio", { name: /^Case notes/ })).toBeChecked();
  });

  it("creates on Enter in the field", async () => {
    await openChoose();
    await userEvent.click(createLink());
    await userEvent.type(cardName(), "Case notes{Enter}");
    expect(await within(top()).findByRole("radio", { name: /^Case notes/ })).toBeChecked();
    expect(posts()).toHaveLength(1);
  });

  it("keeps the card open with the server's sentence under the field when the create is refused", async () => {
    const sentence = DRIVE_REFUSAL.MEMBER_NAME_TAKEN("Case notes");
    post = () => json({ error: sentence, reason: "member_drive_name_taken" }, 422);
    await openChoose();
    await userEvent.click(createLink());
    await userEvent.type(cardName(), "Case notes");
    await userEvent.click(createButton());
    expect(await within(top()).findByRole("alert")).toHaveTextContent(sentence);
    expect(cardName()).toHaveAttribute("aria-invalid", "true");
    expect(announce).not.toHaveBeenCalledWith(NR_FILES.DRIVE_CREATED("Case notes"));
  });

  it("closes only the card on Escape and gives focus back to the link, leaving the Add dialog open", async () => {
    await openChoose();
    await userEvent.click(createLink());
    await userEvent.keyboard("{Escape}");
    expect(within(top()).queryByLabelText(NR_FILES.DRIVE_NAME)).toBeNull();
    expect(dialogs()).toHaveLength(1);
    expect(within(top()).getByRole("radio", { name: /^Team reference/ })).toBeInTheDocument();
    expect(createLink()).toHaveFocus();
    expect(posts()).toHaveLength(0);
  });

  it("closes the card on its Cancel and gives focus back to the link", async () => {
    await openChoose();
    await userEvent.click(createLink());
    const card = cardName().closest("form")!;
    await userEvent.click(within(card).getByRole("button", { name: NR_SHARED.CANCEL }));
    expect(within(top()).queryByLabelText(NR_FILES.DRIVE_NAME)).toBeNull();
    expect(createLink()).toHaveFocus();
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

  it("clears the dialog's selection so Continue is held after a delete", async () => {
    await openChoose();
    await userEvent.click(within(top()).getByRole("radio", { name: new RegExp(`^${DRIVE_OWNED.name}`) }));
    const next = () => within(top()).getByRole("button", { name: new RegExp(`^${NR_SHARED.CONTINUE}`) });
    expect(next()).toBeEnabled();
    await userEvent.click(ownRow().getByRole("button", { name: NR_FILES.DELETE_DRIVE }));
    await userEvent.click(within(top()).getByRole("button", { name: NR_FILES.DELETE_DRIVE }));
    await waitFor(() => expect(dialogs()).toHaveLength(1));
    expect(next()).toBeDisabled();
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

describe("delete: Escape and focus", () => {
  it("leaves Escape to a dialog opened above the confirm", async () => {
    await openChoose();
    await userEvent.click(within(screen.getByTestId("nr-own-drives")).getByRole("button", { name: NR_FILES.DELETE_DRIVE }));
    const above = vi.fn();
    render(
      <Dialog open onOpenChange={above}>
        <DialogContent aria-describedby={undefined}>
          <DialogTitle>Sign in to continue</DialogTitle>
          <button type="button">Sign in</button>
        </DialogContent>
      </Dialog>,
    );
    screen.getByRole("button", { name: "Sign in" }).focus();
    await userEvent.keyboard("{Escape}");
    expect(above).toHaveBeenCalledWith(false);
    expect(screen.getByText(NR_FILES.DELETE_CONFIRM(DRIVE_OWNED.name))).toBeInTheDocument();
  });

  const deleteFirstOwn = async () => {
    await userEvent.click(within(screen.getByTestId("nr-own-drives")).getAllByRole("button", { name: NR_FILES.DELETE_DRIVE })[0]);
    await userEvent.click(within(top()).getByRole("button", { name: NR_FILES.DELETE_DRIVE }));
    await waitFor(() => expect(dialogs()).toHaveLength(1));
  };
  const focused = () => document.activeElement as HTMLElement;

  it.each([["the create door is closed", CREATE_DOORS.limit_reached], ["person-created drives are off", CREATE_DOORS.disabled]])(
    "moves focus to the first option after a delete when %s",
    async (_, door) => {
      create = door;
      await openChoose();
      await deleteFirstOwn();
      await waitFor(() => expect(focused()).toHaveAttribute("type", "radio"));
      expect(focused().closest("label")).not.toHaveTextContent(DRIVE_OWNED.name);
    },
  );

  it("moves focus to the next own drive's Delete when another remains and no create link is on screen", async () => {
    create = CREATE_DOORS.limit_reached;
    drives = [DRIVE_LEGACY, DRIVE_OWNED, DRIVE_LOCKED, { ...NEW_DRIVE, id: "d-second", name: "Second drive" }];
    await openChoose();
    await deleteFirstOwn();
    await waitFor(() => expect(focused()).toHaveAttribute("data-own-delete", "d-second"));
  });

  it("moves focus to Back when nothing else is left to land on", async () => {
    create = CREATE_DOORS.disabled;
    drives = [DRIVE_OWNED];
    await openOwn();
    await deleteFirstOwn();
    await waitFor(() => expect(focused()).toHaveTextContent(NR_SHARED.BACK));
  });
});

describe("with the public step's fixes", () => {
  const next = () => within(top()).getByRole("button", { name: new RegExp(`^${NR_SHARED.CONTINUE}`) });
  const radio = (name: string) => within(top()).getByRole("radio", { name: new RegExp(`^${name}`) });

  describe.each<[string, RunIntent]>([
    ["an agent task", { shape: "agent_task", experience: "background" }],
    ["an environment", INTERACTIVE],
  ])("deleting the drive the run starts in (%s)", (_, base) => {
    it("leaves no subfolder behind once another item becomes the start", async () => {
      drives = [DRIVE_OWNED, DRIVE_LOCKED];
      show({ ...base, drives: [{ drive_id: DRIVE_OWNED.id }], start_folder: { kind: "attachment", drive_id: DRIVE_OWNED.id, subpath: "src" } });
      const sub = await screen.findByPlaceholderText(NR_FILES.SUBFOLDER_PLACEHOLDER);
      await waitFor(() => expect(sub).toHaveValue("src"));
      await userEvent.click(screen.getByRole("button", { name: NR_FILES.ADD_ARIA }));
      await userEvent.click(within(top()).getByRole("button", { name: new RegExp(`^${NR_FILES.KIND_USER_DRIVE}`) }));
      await userEvent.click(await screen.findByRole("button", { name: NR_FILES.DELETE_DRIVE }));
      await userEvent.click(within(top()).getByRole("button", { name: NR_FILES.DELETE_DRIVE }));
      await waitFor(() => expect(dialogs()).toHaveLength(1));
      await userEvent.click(radio(DRIVE_LOCKED.name));
      await userEvent.click(next());
      await userEvent.click(within(top()).getByRole("button", { name: NR_FILES.ADD_CONFIRM }));
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      expect(drawn().start_folder).toEqual({ kind: "attachment", drive_id: DRIVE_LOCKED.id });
      expect(sub).toHaveValue("");
      const line = screen.getAllByText(NR_FILES.STARTS_HERE).find((e) => e.parentElement?.tagName === "P")!.parentElement!;
      expect(line).not.toHaveTextContent("src");
    });
  });

  it("attaches a drive created after a read-only option was selected with edit access", async () => {
    await openChoose();
    await userEvent.click(radio(DRIVE_LOCKED.name));
    await userEvent.click(createLink());
    await userEvent.type(within(top()).getByLabelText(NR_FILES.DRIVE_NAME), "Case notes{Enter}");
    expect(await within(top()).findByRole("radio", { name: /^Case notes/ })).toBeChecked();
    await userEvent.click(next());
    expect(within(top()).getByLabelText(NR_FILES.ACCESS_LABEL)).toHaveValue("edit");
    await userEvent.click(within(top()).getByRole("button", { name: NR_FILES.ADD_CONFIRM }));
    await waitFor(() => expect(drawn().drives).toContainEqual({ drive_id: NEW_DRIVE.id }));
  });

  it("holds Continue while the create form is open and puts the pick back on Cancel", async () => {
    await openChoose();
    await userEvent.click(radio(DRIVE_OWNED.name));
    expect(next()).toBeEnabled();
    await userEvent.click(createLink());
    expect(next()).toBeDisabled();
    expect(radio(DRIVE_OWNED.name)).not.toBeChecked();
    await userEvent.click(within(within(top()).getByLabelText(NR_FILES.DRIVE_NAME).closest("form")!).getByRole("button", { name: NR_SHARED.CANCEL }));
    expect(radio(DRIVE_OWNED.name)).toBeChecked();
    expect(next()).toBeEnabled();
  });

  it("leaves a pick the person made while the form is open when they cancel", async () => {
    await openChoose();
    await userEvent.click(radio(DRIVE_OWNED.name));
    await userEvent.click(createLink());
    await userEvent.click(radio(DRIVE_LOCKED.name));
    await userEvent.click(within(within(top()).getByLabelText(NR_FILES.DRIVE_NAME).closest("form")!).getByRole("button", { name: NR_SHARED.CANCEL }));
    expect(radio(DRIVE_LOCKED.name)).toBeChecked();
  });

  it("leaves Continue live when the picked drive cannot be told from another of the same name", async () => {
    drives = [DRIVE_OWNED, { ...DRIVE_LOCKED, name: DRIVE_OWNED.name }];
    show();
    await userEvent.click(await screen.findByRole("button", { name: NR_FILES.ADD_ARIA }));
    await userEvent.click(within(top()).getByRole("button", { name: new RegExp(`^${NR_FILES.KIND_USER_DRIVE}`) }));
    await userEvent.click((await within(top()).findAllByRole("radio"))[0]);
    await userEvent.click(createLink());
    expect(next()).toBeEnabled();
  });
});
