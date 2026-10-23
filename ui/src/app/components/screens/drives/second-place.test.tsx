/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Where a drive mounts from (mock M10, #113 + #112) on the admin Drives screen:
// the Mounts from column prints the server's phrase, the label under Backend
// says what the backend allows, and a share's "Also reachable from" section
// writes one second place, a person's own runner and the path there.
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

const getDrivesMock = vi.fn();
const createDriveMock = vi.fn();
const updateDriveMock = vi.fn();
vi.mock("../../../lib/api/drives", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/drives")>("../../../lib/api/drives");
  return {
    ...actual,
    drives: {
      getDrives: () => getDrivesMock(),
      createDrive: (...a: unknown[]) => createDriveMock(...a),
      updateDrive: (...a: unknown[]) => updateDriveMock(...a),
      upsertGrant: vi.fn(),
      deleteGrant: vi.fn(),
      driveGrants: vi.fn(),
      deleteDrive: vi.fn(),
      previewDrive: vi.fn().mockResolvedValue({}),
    },
  };
});
const listRunnersMock = vi.fn();
vi.mock("../../../lib/api/runners", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/runners")>("../../../lib/api/runners");
  return { ...actual, runners: { ...actual.runners, list: (...a: unknown[]) => listRunnersMock(...a) } };
});
const getProvidersMock = vi.fn();
vi.mock("../../../lib/api/providers", () => ({ providers: { getWorkspaceProviders: () => getProvidersMock() } }));

import { HttpError } from "../../../lib/api/core";
import type { UserDriveListItem, UserDrivesSnapshot } from "../../../lib/api/drives";
import { DRIVES } from "../../../lib/user-drives-copy";
import { aheadByHours } from "../../../lib/test-clock";
import { DrivesScreen } from "./drives-screen";

const when = aheadByHours(-1);
const MBP = "6f1c1d4e-1111-4a5b-9c2d-000000000001";

function drive(over: Partial<UserDriveListItem> = {}): UserDriveListItem {
  return {
    id: "d1", name: "team-share", backend: "host_path", host_root: "/srv/shares/team", home_template: "email_local",
    size_mib: 51200, writable: true, reclaim: "retain", grant_count: 1, created_at: when, updated_at: when,
    placement_eligible: true, mounts_from: "Organisation runners", ...over,
  };
}

function snapshot(drives: UserDriveListItem[]): UserDrivesSnapshot {
  return { drives, grants: [], host_roots_configured: true, runner_target: "docker", disabled: false };
}

async function renderScreen(snap: UserDrivesSnapshot) {
  getDrivesMock.mockResolvedValue(snap);
  getProvidersMock.mockResolvedValue({ providers: {}, etag: null });
  render(<DrivesScreen />);
  await screen.findAllByRole("table");
  await waitFor(() => expect(getProvidersMock).toHaveBeenCalled());
  await act(async () => {});
}

const editor = () => within(screen.getByTestId("drives-drive-editor"));
const openEdit = async (name: string) => userEvent.click(await screen.findByRole("button", { name: `${DRIVES.EDIT} ${name}` }));

beforeEach(() => {
  for (const m of [getDrivesMock, createDriveMock, updateDriveMock, listRunnersMock, getProvidersMock]) m.mockReset();
  updateDriveMock.mockImplementation(async (id: string, input: object) => ({ id, ...input }));
  listRunnersMock.mockResolvedValue([
    { id: MBP, name: "mbp-dev", state: "claimed", owner_kind: "person", owner: "ana@example.com", kind: "self_hosted" },
    { id: "gone", name: "old-laptop", state: "revoked", owner_kind: "person", kind: "self_hosted" },
    { id: "org1", name: "build-01", state: "claimed", owner_kind: "org", kind: "self_hosted" },
  ]);
});

describe("Drives list, Mounts from", () => {
  it("prints the server's phrase for each drive, naming no runner itself", async () => {
    await renderScreen(snapshot([
      drive({ alt_backend: "host_path", alt_host_root: "/Volumes/team-share", alt_runner_id: MBP, mounts_from: "Organisation runners and mbp-dev" }),
      drive({ id: "d2", name: "scratch", backend: "docker_volume", host_root: undefined, home_template: "hash", placement_eligible: false }),
    ]));
    expect(screen.getByRole("columnheader", { name: DRIVES.COL_MOUNTS_FROM })).toBeInTheDocument();
    expect(screen.getAllByTestId("drives-mounts-from").map((c) => c.textContent)).toEqual(["Organisation runners and mbp-dev", "Organisation runners"]);
  });
});

describe("Drive editor, where a drive mounts from", () => {
  it("labels a share as two places and offers Add a second place; a managed volume is one place and offers none", async () => {
    await renderScreen(snapshot([drive()]));
    await openEdit("team-share");
    expect(editor().getByText(DRIVES.LABEL_TWO_PLACES)).toBeInTheDocument();
    expect(editor().getByText(DRIVES.LABEL_TWO_PLACES_BODY)).toBeInTheDocument();
    expect(editor().getByRole("button", { name: DRIVES.SECOND_PLACE_ADD })).toBeInTheDocument();
    await userEvent.click(editor().getByRole("button", { name: DRIVES.BACKEND_DOCKER_VOLUME }));
    expect(editor().getByText(DRIVES.LABEL_ONE_PLACE)).toBeInTheDocument();
    expect(editor().getByText(DRIVES.LABEL_ONE_PLACE_DOCKER)).toBeInTheDocument();
    expect(editor().getByText(DRIVES.SECOND_PLACE_SHARE_ONLY)).toBeInTheDocument();
    expect(editor().queryByRole("button", { name: DRIVES.SECOND_PLACE_ADD })).toBeNull();
  });

  it("adds a second place on a person's own runner, with the reach note, and saves all three fields", async () => {
    await renderScreen(snapshot([drive()]));
    await openEdit("team-share");
    await userEvent.click(editor().getByRole("button", { name: DRIVES.SECOND_PLACE_ADD }));
    const place = within(editor().getByTestId("drive-second-place"));
    expect(place.getByText(DRIVES.SECOND_PLACE_TITLE)).toBeInTheDocument();
    expect(place.getByText(DRIVES.SECOND_PLACE_LEAD)).toBeInTheDocument();
    expect(place.getByText(DRIVES.WHERE_OWN_RUNNER)).toBeInTheDocument();
    expect(place.getByText(DRIVES.REACH_NOTE)).toBeInTheDocument();
    // Only a claimed runner of a person's own is offered.
    await waitFor(() => expect(place.getByRole("option", { name: DRIVES.RUNNER_OPTION("mbp-dev") })).toBeInTheDocument());
    expect(place.queryByRole("option", { name: DRIVES.RUNNER_OPTION("old-laptop") })).toBeNull();
    expect(place.queryByRole("option", { name: DRIVES.RUNNER_OPTION("build-01") })).toBeNull();
    await userEvent.selectOptions(place.getByLabelText(DRIVES.FIELD_RUNNER), MBP);
    await userEvent.type(place.getByLabelText(DRIVES.FIELD_ALT_HOST_ROOT), " /Volumes/team-share ");
    await userEvent.click(editor().getByRole("button", { name: DRIVES.SAVE_CTA }));
    await waitFor(() => expect(updateDriveMock).toHaveBeenCalled());
    expect(updateDriveMock.mock.calls[0][1]).toMatchObject({ alt_backend: "host_path", alt_host_root: "/Volumes/team-share", alt_runner_id: MBP });
  });

  it("an existing second place shows without the reach note, and Remove this place saves the drive with one place", async () => {
    await renderScreen(snapshot([drive({ alt_backend: "host_path", alt_host_root: "/Volumes/team-share", alt_runner_id: MBP })]));
    await openEdit("team-share");
    const place = within(editor().getByTestId("drive-second-place"));
    expect(place.queryByText(DRIVES.REACH_NOTE)).toBeNull();
    expect(place.getByLabelText(DRIVES.FIELD_ALT_HOST_ROOT)).toHaveValue("/Volumes/team-share");
    await userEvent.click(place.getByRole("button", { name: DRIVES.SECOND_PLACE_REMOVE }));
    expect(editor().queryByTestId("drive-second-place")).toBeNull();
    await userEvent.click(editor().getByRole("button", { name: DRIVES.SAVE_CTA }));
    await waitFor(() => expect(updateDriveMock).toHaveBeenCalled());
    const sent = updateDriveMock.mock.calls[0][1];
    expect(sent.alt_backend).toBeUndefined();
    expect(sent.alt_runner_id).toBeUndefined();
  });

  it("the server's save refusal renders as it is sent, under the refusal heading", async () => {
    updateDriveMock.mockRejectedValue(new HttpError(400, DRIVES.SECOND_PLACE_REFUSED("team-share")));
    await renderScreen(snapshot([drive()]));
    await openEdit("team-share");
    await userEvent.click(editor().getByRole("button", { name: DRIVES.SAVE_CTA }));
    const alert = await editor().findByRole("alert");
    expect(alert).toHaveTextContent(DRIVES.SAVE_REFUSED_TITLE);
    expect(alert).toHaveTextContent(`user_drives: "team-share": the second place must also be a share, and somewhere the first is not`);
  });

  it("SECOND_PLACE_REFUSED is the server's own literal", () => {
    const go = readFileSync(resolve(process.cwd(), "../internal/api/user_drives_second_place.go"), "utf8");
    expect(go).toContain(`"${DRIVES.SECOND_PLACE_REFUSED("%q").replace('"%q"', "%q")}"`);
  });
});
