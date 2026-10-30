/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Drives in a macOS VM run (board J4): the picker, the card, the create and delete of a disk drive on the run's
// Mac, the note under the list and the keep control. Every sentence is read from the copy module or the refusal
// table, so a drift in the sheet fails here.

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { CREATE_ALLOWED } from "../../../../../lib/new-run-fixtures/scenarios/drives";
import { filesPreflight, MAIN_DRIVE_FACTS, SCENARIO_FILES_WORKSPACES } from "../../../../../lib/new-run-fixtures/scenarios/files";
import { DRIVE_REFUSAL } from "../../../../../lib/drive-refusals";
import type { MeDrive, MeDriveCatalogue, MemberDriveCreate } from "../../../../../lib/api/me-drives";
import { MAC_DRIVES } from "../../../../wardyn/copy/mac-drives";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch } from "../../new-run-route";
import { MacOSOfferContext, type MacOSOffer } from "../run/macos-offer";
import { AnnounceContext } from "../step-shell";
import { FilesRoute } from "./files-route";

const wfetchMock = vi.fn();
vi.mock("../../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));
const checks = vi.hoisted(() => ({ result: null as unknown }));
vi.mock("../../draft/use-intent-checks", () => ({
  useDraftIdentity: () => ({ principal: "alice", resolved: true, revision: 0, authGeneration: 0 }),
  useIntentChecks: (params: { body: unknown }) => ({ preflightResult: params.body === null ? null : checks.result }),
}));

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
const MAC = "mac-studio-07";
const base = { section: "user", source: "personal", enforcement: "none", legacy: false, team: false, writable: true } as const;
const DISK: MeDrive = { ...base, id: "d-disk", name: "Investigation files", backend: "mac_disk", target: "/Users/agent/drives/investigation-files", owned: true, mac: MAC };
const DOCKER: MeDrive = { ...base, id: "d-docker", name: "scratch-2025", backend: "docker_volume", target: "/home/agent/drives/scratch", owned: true };
const CLUSTER: MeDrive = { ...base, id: "d-pvc", name: "team-datasets", backend: "k8s_pvc", target: "/home/agent/drives/datasets", owned: false };
const OFFER: MacOSOffer = { ok: true, guestOS: "macOS 26.6.2 (25G83)", minCPUs: 4, minMemoryMiB: 8192, harnesses: ["none"], runner: MAC };
const MACOS: RunIntent = { shape: "environment", experience: "interactive", machine_kind: "macos_vm" };
const OWN_MAC = { id: "r-mac", name: MAC, macos_vm: { max: 2, disk_drives: true, base: { state: "ready" } } };
const announce = vi.fn();

let drives: MeDrive[];
let create: MemberDriveCreate | undefined;
let runners: unknown[];
let del: (id: string) => Response;
let offer: MacOSOffer | undefined;

beforeEach(() => {
  drives = [DISK, DOCKER, CLUSTER];
  create = CREATE_ALLOWED;
  runners = [OWN_MAC];
  offer = OFFER;
  del = () => new Response(null, { status: 204 });
  checks.result = filesPreflight(MAIN_DRIVE_FACTS.filesystem);
  wfetchMock.mockImplementation(async (path: string, init?: { method?: string; body?: string }) => {
    const method = init?.method ?? "GET";
    if (path.startsWith("/workspaces")) return json(SCENARIO_FILES_WORKSPACES.member);
    if (path.startsWith("/setup/status")) return json({ runner: { driver: "", confinement_classes: [], kubernetes: false } });
    if (path.startsWith("/me/runners")) return json(runners);
    if (path === "/me/drives" && method === "POST") {
      const body = JSON.parse(init!.body!);
      return json({ ...DISK, id: "d-new", name: body.name }, 201);
    }
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
  announce.mockReset();
});

function Seed({ intent }: { intent: RunIntent }) {
  const dispatch = useNewRunDispatch();
  React.useEffect(() => dispatch({ type: "seed", origin: { kind: "scratch" }, intent }), [dispatch, intent]);
  return null;
}
function show(intent: RunIntent = MACOS) {
  render(
    <MemoryRouter>
      <AnnounceContext.Provider value={announce}>
        <MacOSOfferContext.Provider value={offer}>
          <NewRunDraftProvider owner="alice">
            <Seed intent={intent} />
            <FilesRoute location={{ screen: "step", step: "files" }} go={vi.fn()} startFromScratch={() => {}} />
          </NewRunDraftProvider>
        </MacOSOfferContext.Provider>
      </AnnounceContext.Provider>
    </MemoryRouter>,
  );
}
const dialogs = () => screen.getAllByRole("dialog", { hidden: true });
const top = () => dialogs()[dialogs().length - 1];
async function openChoose(intent?: RunIntent) {
  show(intent);
  await userEvent.click(await screen.findByRole("button", { name: NR_FILES.ADD_ARIA }));
  await userEvent.click(within(top()).getByRole("button", { name: new RegExp(`^${NR_FILES.KIND_USER_DRIVE}`) }));
  await within(top()).findByRole("radio", { name: new RegExp(`^${DISK.name}`) });
}
const radio = (name: string) => within(top()).getByRole("radio", { name: new RegExp(`^${name}`) });
const posts = () => wfetchMock.mock.calls.filter(([, init]) => init?.method === "POST");

describe("the picker of a macOS run", () => {
  it("names the Mac a disk is kept on (MAC-D01) and greys a Docker or cluster volume with its reason (MAC-D03)", async () => {
    await openChoose();
    expect(within(top()).getByText(MAC_DRIVES.D01(MAC))).toBeInTheDocument();
    expect(radio(DISK.name)).toBeEnabled();
    for (const v of [DOCKER, CLUSTER]) {
      expect(radio(v.name)).toBeDisabled();
      expect(radio(v.name).closest("label")).toHaveTextContent(MAC_DRIVES.D03);
    }
  });
  it("a container run still picks its Docker volume, and says nothing of a Mac", async () => {
    await openChoose({ shape: "environment", experience: "interactive" });
    expect(radio(DOCKER.name)).toBeEnabled();
    expect(within(top()).queryByText(MAC_DRIVES.D03)).toBeNull();
  });
  it("the collapsed row of an attached disk says it is stored on the Mac, as a second line (MAC-D01)", async () => {
    show({ ...MACOS, drives: [{ drive_id: DISK.id }] });
    const card = await screen.findByTestId("nr-attach-card");
    expect(card.querySelector("button[aria-expanded]")).toHaveAttribute("aria-expanded", "false");
    const line = within(card).getByText(`${NR_FILES.STORED_ON}: ${MAC_DRIVES.D01(MAC)}`);
    expect(line).toBeInTheDocument();
    // It wraps at its spaces and is never cut with an ellipsis, so the Mac's name is not lost on a phone.
    expect(line.className).toContain("whitespace-normal");
    expect(line.className).not.toContain("truncate");
    expect(line).toHaveAttribute("title", MAC_DRIVES.D01(MAC));
  });
  it("a drive's name wraps at words, is cut rather than split, carries its full name, and gives its room to the name on a phone", async () => {
    show({ ...MACOS, drives: [{ drive_id: DISK.id }] });
    const card = await screen.findByTestId("nr-attach-card");
    const name = within(card).getByText(DISK.name);
    expect(name).toHaveAttribute("title", DISK.name);
    expect(name.className).toContain("text-ellipsis");
    expect(name.className).not.toContain("anywhere");
    expect(card.querySelector("button[aria-expanded] > span[aria-hidden]")?.className).toContain("max-[520px]:hidden");
  });
  it("the attached disk's card says where it is stored (MAC-D01)", async () => {
    show({ ...MACOS, drives: [{ drive_id: DISK.id }] });
    const card = await screen.findByTestId("nr-attach-card");
    await userEvent.click(card.querySelector<HTMLElement>("button[aria-expanded]")!);
    expect(await screen.findByText(MAC_DRIVES.D01(MAC))).toBeInTheDocument();
  });
});

describe("the Main drive card (MAC-D19)", () => {
  it("a macOS run describes the VM's own disk, not /home/agent", async () => {
    show();
    expect(await screen.findByText(MAC_DRIVES.D19)).toBeInTheDocument();
    expect(screen.queryByText(NR_FILES.MAIN_BODY)).toBeNull();
  });
  it("a container run keeps today's description", async () => {
    show({ shape: "environment", experience: "interactive" });
    expect(await screen.findByText(NR_FILES.MAIN_BODY)).toBeInTheDocument();
    expect(screen.queryByText(MAC_DRIVES.D19)).toBeNull();
  });
});

describe("the note under the list (MAC-D08)", () => {
  it("shows on a macOS run, and not on a container run", async () => {
    show();
    expect(await screen.findByText(MAC_DRIVES.D08)).toBeInTheDocument();
    cleanup();
    show({ shape: "environment", experience: "interactive" });
    await screen.findByRole("button", { name: NR_FILES.ADD_ARIA });
    expect(screen.queryByText(MAC_DRIVES.D08)).toBeNull();
  });
});

describe("creating a drive for a macOS run (MAC-D02)", () => {
  it("is a disk on the run's Mac: the intro names it and the create sends that runner", async () => {
    await openChoose();
    await userEvent.click(await within(top()).findByRole("button", { name: NR_FILES.CREATE_USER_DRIVE }));
    expect(within(top()).getByText(MAC_DRIVES.D02(MAC))).toBeInTheDocument();
    expect(within(top()).queryByText(NR_FILES.CREATE_INTRO)).toBeNull();
    await userEvent.type(within(top()).getByLabelText(NR_FILES.DRIVE_NAME), "Case notes{Enter}");
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(JSON.parse(posts()[0][1].body)).toEqual({ name: "Case notes", runner_id: OWN_MAC.id });
  });
  it("is not offered while the run's Mac is not one of the person's own", async () => {
    runners = [];
    await openChoose();
    expect(within(top()).queryByRole("button", { name: NR_FILES.CREATE_USER_DRIVE })).toBeNull();
  });
  it("is not offered while the person's Mac of that name cannot keep disk drives", async () => {
    runners = [{ ...OWN_MAC, macos_vm: { max: 2, base: { state: "ready" } } }];
    await openChoose();
    expect(within(top()).queryByRole("button", { name: NR_FILES.CREATE_USER_DRIVE })).toBeNull();
  });
  it("is not offered in a pool of several Macs, which names none", async () => {
    offer = { ...OFFER, runner: undefined };
    await openChoose();
    expect(within(top()).queryByRole("button", { name: NR_FILES.CREATE_USER_DRIVE })).toBeNull();
  });
  it("says how many drives the person has of how many, under the field", async () => {
    await openChoose();
    await userEvent.click(await within(top()).findByRole("button", { name: NR_FILES.CREATE_USER_DRIVE }));
    expect(within(top()).getByText(NR_FILES.CREATE_LIMIT(CREATE_ALLOWED.count, CREATE_ALLOWED.limit))).toBeInTheDocument();
  });
  it("a container run creates as before: no runner, today's intro, no count", async () => {
    await openChoose({ shape: "environment", experience: "interactive" });
    await userEvent.click(await within(top()).findByRole("button", { name: NR_FILES.CREATE_USER_DRIVE }));
    expect(within(top()).getByText(NR_FILES.CREATE_INTRO)).toBeInTheDocument();
    expect(within(top()).queryByText(NR_FILES.CREATE_LIMIT(CREATE_ALLOWED.count, CREATE_ALLOWED.limit))).toBeNull();
    await userEvent.type(within(top()).getByLabelText(NR_FILES.DRIVE_NAME), "Case notes{Enter}");
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(JSON.parse(posts()[0][1].body)).toEqual({ name: "Case notes" });
  });
});

describe("deleting a disk drive (MAC-D10, D11, D12)", () => {
  const confirmDelete = async () => {
    await userEvent.click(dialogs()[0].querySelector<HTMLElement>("[data-own-delete]")!);
    await userEvent.click(within(top()).getByRole("button", { name: NR_FILES.DELETE_DRIVE }));
  };
  it("a Mac that is offline: the server's sentence stays up until the person closes it", async () => {
    del = () => json({ reason: "member_drive_delete_deferred", message: DRIVE_REFUSAL.MEMBER_DELETE_DEFERRED("kofi-mbp") }, 202);
    await openChoose();
    await confirmDelete();
    expect(await within(top()).findByText(DRIVE_REFUSAL.MEMBER_DELETE_DEFERRED("kofi-mbp"))).toBeInTheDocument();
    expect(within(top()).queryByRole("button", { name: NR_FILES.DELETE_DRIVE })).toBeNull();
    await userEvent.click(within(top()).getByRole("button", { name: NR_SHARED.CLOSE_DIALOG }));
    await waitFor(() => expect(dialogs()).toHaveLength(1));
    // The live Notice spoke for it: no "deleted" on top.
    expect(announce).not.toHaveBeenCalledWith(NR_FILES.DRIVE_DELETED(DISK.name));
  });
  it("a Mac no longer registered: only the record goes, and the sentence says so", async () => {
    del = () => json({ reason: "member_drive_record_only", message: DRIVE_REFUSAL.MEMBER_RECORD_ONLY("old-mac-mini") }, 200);
    await openChoose();
    await confirmDelete();
    expect(await within(top()).findByText(DRIVE_REFUSAL.MEMBER_RECORD_ONLY("old-mac-mini"))).toBeInTheDocument();
    expect(announce).not.toHaveBeenCalledWith(NR_FILES.DRIVE_DELETED(DISK.name));
  });
  it("a drive in a run on its Mac is refused with the server's sentence, and the confirm stays", async () => {
    del = () => json({ error: DRIVE_REFUSAL.MEMBER_IN_RUN_ON_MAC(DISK.name, MAC), reason: "member_drive_in_use" }, 409);
    await openChoose();
    await confirmDelete();
    expect(await within(top()).findByRole("alert")).toHaveTextContent(DRIVE_REFUSAL.MEMBER_IN_RUN_ON_MAC(DISK.name, MAC));
    expect(within(top()).getByRole("button", { name: NR_FILES.DELETE_DRIVE })).toBeEnabled();
  });
  it("an ordinary delete (204) closes the confirm at once, and is announced", async () => {
    await openChoose();
    await confirmDelete();
    await waitFor(() => expect(dialogs()).toHaveLength(1));
    expect(announce).toHaveBeenCalledWith(NR_FILES.DRIVE_DELETED(DISK.name));
  });
});

describe("the keep control of a macOS run (MAC-D14, D06)", () => {
  beforeEach(() => {
    create = { ...CREATE_ALLOWED, keep_work_folder: true };
  });
  it("is 'Keep the work folder', and says the folder is kept as a disk on the run's Mac", async () => {
    show({ ...MACOS, keep_work_folder: true });
    expect(await screen.findByLabelText(MAC_DRIVES.D14)).toBeChecked();
    // The paths are set in code, so the sentence is read off the body's text, not off one node.
    expect(screen.getByTestId("nr-keep-work-folder")).toHaveTextContent(MAC_DRIVES.D06(MAC));
    expect(screen.queryByText(NR_FILES.REUSE_LABEL)).toBeNull();
    expect(screen.getByTestId("nr-keep-work-folder")).not.toHaveTextContent(NR_FILES.KEEP_WHAT);
    expect(screen.queryByRole("button", { name: NR_FILES.REUSE_ARIA })).toBeNull();
    expect(screen.getByText(NR_FILES.KEEP_WARNING)).toBeInTheDocument();
  });
  it("in a pool of several Macs the folder sentence names none, so it is left out", async () => {
    offer = { ...OFFER, runner: undefined };
    show({ ...MACOS, keep_work_folder: true });
    expect(await screen.findByLabelText(MAC_DRIVES.D14)).toBeChecked();
    expect(screen.getByTestId("nr-keep-work-folder")).not.toHaveTextContent(/Keeps the work folder/);
  });
  it("a container run keeps today's label and sentence", async () => {
    show({ shape: "environment", experience: "interactive", keep_work_folder: true });
    expect(await screen.findByLabelText(NR_FILES.REUSE_LABEL)).toBeChecked();
    expect(screen.getByTestId("nr-keep-work-folder")).toHaveTextContent(NR_FILES.KEEP_WHAT);
  });
});
