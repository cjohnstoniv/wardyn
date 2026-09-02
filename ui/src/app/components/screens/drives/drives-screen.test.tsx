/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// DrivesScreen — the admin's drive registry (0.7 user drives, slice D5a). Every
// assertion reads its expected string from a copy module rather than retyping
// it, the convention governance-screen.test.tsx already follows: these tests
// fail the moment a rendered string stops coming from the canon.
//
// Six things are pinned here because they are DECISIONS, not rendering:
//   1. exactly one `default` (teal) button per state (CONSOLE-RULES §6),
//   2. the allocation form COLLAPSES while the editor is open — the mechanism
//      that keeps rule 1 true without inventing a modal,
//   3. the editor offers only THIS runner's two backends, and disables
//      `host_path` WITH ITS REASON when no roots are configured (Q3),
//   4. delete is TWO refusals: a pre-filled, count-bearing one the client can
//      see, and a COUNT-FREE 409 for the race it cannot,
//   5. the preview has three arms — a drive, no drive, and a PAUSED winner that
//      derives no directory and no storage object,
//   6. no component in this directory renders a product string of its own.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { readFileSync } from "node:fs";
import { join } from "node:path";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

const getDrivesMock = vi.fn();
const createDriveMock = vi.fn();
const updateDriveMock = vi.fn();
const deleteDriveMock = vi.fn();
const upsertGrantMock = vi.fn();
const deleteGrantMock = vi.fn();
const previewDriveMock = vi.fn();
vi.mock("../../../lib/api/drives", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/drives")>("../../../lib/api/drives");
  return {
    ...actual,
    drives: {
      getDrives: () => getDrivesMock(),
      createDrive: (...a: unknown[]) => createDriveMock(...a),
      updateDrive: (...a: unknown[]) => updateDriveMock(...a),
      deleteDrive: (...a: unknown[]) => deleteDriveMock(...a),
      upsertGrant: (...a: unknown[]) => upsertGrantMock(...a),
      deleteGrant: (...a: unknown[]) => deleteGrantMock(...a),
      previewDrive: (...a: unknown[]) => previewDriveMock(...a),
    },
  };
});

// The Who field is a DirectoryCombobox. It defaults here to the deployment with
// NO directory configured (the search reports unconfigured → null → the plain
// text input it has always been); the combobox's own states are pinned in
// wardyn/directory-combobox.test.tsx.
const directorySearchMock = vi.fn();
vi.mock("../../../lib/api/directory", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/directory")>("../../../lib/api/directory");
  return { ...actual, directory: { search: (...a: unknown[]) => directorySearchMock(...a) } };
});

import { HttpError } from "../../../lib/api/core";
import type { UserDriveListItem, UserDrivesSnapshot } from "../../../lib/api/drives";
import { GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { ACCESS_STATE, DRIVES, PEOPLE, PERM, PREVIEW } from "../../../lib/user-drives-copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { DrivesScreen } from "./drives-screen";
import { question, sizeText } from "./display";

function drive(over: Partial<UserDriveListItem> = {}): UserDriveListItem {
  return {
    id: "d1",
    name: "Corporate homes",
    backend: "k8s_pvc_static",
    home_template: "email_local",
    reclaim: "retain",
    grant_count: 2,
    created_at: "2026-08-28T00:00:00Z",
    updated_at: "2026-08-28T00:00:00Z",
    ...over,
  };
}

const HOMES = drive();
const SCRATCH = drive({
  id: "d2",
  name: "Scratch",
  backend: "k8s_pvc",
  home_template: "hash",
  size_mib: 8192,
  writable: true,
  reclaim: "delete",
  grant_count: 1,
});

function snapshot(over: Partial<UserDrivesSnapshot> = {}): UserDrivesSnapshot {
  return {
    drives: [HOMES, SCRATCH],
    grants: [
      {
        id: "g1",
        subject_type: "group",
        subject: "wardyn.platform",
        drive_id: SCRATCH.id,
        priority: 10,
        size_mib_override: 16384,
        enabled: true,
        created_at: "2026-08-29T00:00:00Z",
      },
    ],
    host_roots_configured: false,
    runner_target: "k8s",
    ...over,
  };
}

// The screen's ONE `default` button. A checked Switch carries bg-primary too,
// and getAllByRole("button") correctly excludes it — it is a switch, not a
// button; OptionCard's selected tint is bg-primary/10, a different class.
function tealButtons(): HTMLElement[] {
  return screen.getAllByRole("button").filter((b) => b.className.split(/\s+/).includes("bg-primary"));
}

function renderScreen(snap: UserDrivesSnapshot | null = snapshot()) {
  if (snap) getDrivesMock.mockResolvedValue(snap);
  else getDrivesMock.mockRejectedValue(new Error("boom"));
  render(<DrivesScreen />);
}

beforeEach(() => {
  getDrivesMock.mockReset();
  createDriveMock.mockReset();
  updateDriveMock.mockReset();
  deleteDriveMock.mockReset();
  upsertGrantMock.mockReset();
  deleteGrantMock.mockReset();
  previewDriveMock.mockReset();
  previewDriveMock.mockResolvedValue({});
  directorySearchMock.mockReset();
  directorySearchMock.mockResolvedValue(null);
});

describe("DrivesScreen — states", () => {
  it("renders the frozen page header, with the mount target mono", async () => {
    renderScreen();
    expect(await screen.findByRole("heading", { name: DRIVES.TITLE, level: 1 })).toBeInTheDocument();
    // withMono splits the lead across sibling nodes, so no single node holds it;
    // textContent concatenates every descendant in order and String.split with
    // a capturing group neither drops nor adds a character.
    expect(document.body.textContent).toContain(DRIVES.LEAD);
    expect(screen.getAllByText("/home/agent/drive")[0].className).toContain("font-mono");
  });

  it("no drives: the empty state carries New drive, and it is the ONE teal", async () => {
    renderScreen(snapshot({ drives: [], grants: [] }));
    expect(await screen.findByText(DRIVES.EMPTY_TITLE)).toBeInTheDocument();
    expect(screen.getByText(DRIVES.EMPTY_BODY)).toBeInTheDocument();

    const teal = tealButtons();
    expect(teal).toHaveLength(1);
    expect(teal[0]).toHaveTextContent(DRIVES.NEW_CTA);
    // …and only one New drive button exists at all — the section header's is
    // dropped rather than duplicating the empty state's.
    expect(screen.getAllByRole("button", { name: DRIVES.NEW_CTA })).toHaveLength(1);
  });

  it("populated: the kind chip over the wire backend, the size with its gloss, the mode and the count", async () => {
    renderScreen();
    await screen.findByText(HOMES.name);
    // Scoped to the drives table: the allocation form below carries the same
    // mode words in its writable-override control, which is the point of the
    // mode vocabulary being one set rather than two.
    const table = within(screen.getAllByRole("table")[0]);

    expect(table.getByText(DRIVES.KIND_SHARE)).toBeInTheDocument();
    expect(table.getByText(DRIVES.KIND_MANAGED)).toBeInTheDocument();
    // The backend's wire value is mono; the drive NAME never is.
    expect(table.getByText("k8s_pvc_static").className).toContain("font-mono");
    expect(table.getByText(HOMES.name).className).not.toContain("font-mono");

    // A share shows no allocation and says where its bytes are bounded; the
    // managed claim shows GiB and says the storage class decides.
    expect(table.getByText(DRIVES.SIZE_NONE)).toBeInTheDocument();
    expect(table.getByText(DRIVES.ENFORCEMENT_EXTERNAL)).toBeInTheDocument();
    expect(table.getByText(DRIVES.SIZE_GIB(8))).toBeInTheDocument();
    expect(table.getByText(DRIVES.ENFORCEMENT_REQUEST)).toBeInTheDocument();

    expect(table.getByText(DRIVES.MODE_RO)).toBeInTheDocument();
    expect(table.getByText(DRIVES.MODE_RW)).toBeInTheDocument();
    expect(table.getByText(DRIVES.RECLAIM_RETAIN)).toBeInTheDocument();
    expect(table.getByText(DRIVES.RECLAIM_DELETE)).toBeInTheDocument();
    expect(table.getByText(DRIVES.ALLOCATED_COUNT(2))).toBeInTheDocument();
    expect(table.getByText(DRIVES.ALLOCATED_COUNT(1))).toBeInTheDocument();

    // HONESTY renders ONCE, under the table, and disk_mib is mono there.
    expect(document.body.textContent).toContain(DRIVES.HONESTY);
    expect(screen.getByText("disk_mib").className).toContain("font-mono");
  });

  it("a drive nobody mounts has its own words", async () => {
    renderScreen(snapshot({ drives: [drive({ grant_count: 0 })], grants: [] }));
    expect(await screen.findByText(DRIVES.ALLOCATED_NONE)).toBeInTheDocument();
  });

  it("fetch_failed is distinct from empty, says allocations still bind, and offers Retry", async () => {
    renderScreen(null);
    expect(await screen.findByText(DRIVES.FETCH_FAILED_TITLE)).toBeInTheDocument();
    expect(screen.getByText(DRIVES.FETCH_FAILED_BODY)).toBeInTheDocument();
    expect(screen.queryByText(DRIVES.EMPTY_TITLE)).not.toBeInTheDocument();

    getDrivesMock.mockResolvedValue(snapshot());
    await userEvent.click(screen.getByRole("button", { name: ACCESS_STATE.FETCH_FAILED_RETRY }));
    expect(await screen.findByText(HOMES.name)).toBeInTheDocument();
  });

  it("drives but no allocations is its own state", async () => {
    renderScreen(snapshot({ grants: [] }));
    expect(await screen.findByText(DRIVES.EMPTY_ALLOC_TITLE)).toBeInTheDocument();
    expect(screen.getByText(DRIVES.EMPTY_ALLOC_BODY)).toBeInTheDocument();
  });

  it("at rest the ONE teal is Allocate — New drive and Edit are not", async () => {
    renderScreen();
    await screen.findByText(HOMES.name);
    const teal = tealButtons();
    expect(teal).toHaveLength(1);
    expect(teal[0]).toHaveTextContent(DRIVES.ADD_CTA);
  });
});

describe("DrivesScreen — the editor collapses the allocation form (one teal at a time)", () => {
  it("opening the editor collapses the form to a disabled summary row and moves the teal to Save drive", async () => {
    renderScreen();
    await screen.findByText(HOMES.name);
    expect(screen.queryByTestId("drives-add-allocation-collapsed")).not.toBeInTheDocument();
    expect(screen.getByLabelText(GOV.FIELD_PRIORITY)).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: `${DRIVES.EDIT} ${HOMES.name}` }));

    expect(await screen.findByTestId("drives-drive-editor")).toBeInTheDocument();
    expect(screen.getByText(DRIVES.EDITOR_TITLE_EDIT(HOMES.name))).toBeInTheDocument();

    const collapsed = screen.getByTestId("drives-add-allocation-collapsed");
    expect(within(collapsed).getByText(DRIVES.ADD_TITLE)).toBeInTheDocument();
    expect(within(collapsed).getByRole("button", { name: DRIVES.ADD_CTA })).toBeDisabled();
    expect(screen.queryByLabelText(GOV.FIELD_PRIORITY)).not.toBeInTheDocument();

    const teal = tealButtons();
    expect(teal).toHaveLength(1);
    expect(teal[0]).toHaveTextContent(DRIVES.SAVE_CTA);
  });

  it("Cancel re-expands the form and hands the teal back to Allocate", async () => {
    renderScreen();
    await screen.findByText(HOMES.name);
    await userEvent.click(screen.getByRole("button", { name: `${DRIVES.EDIT} ${HOMES.name}` }));
    await userEvent.click(screen.getByRole("button", { name: PEOPLE.CANCEL }));

    expect(screen.queryByTestId("drives-drive-editor")).not.toBeInTheDocument();
    expect(screen.queryByTestId("drives-add-allocation-collapsed")).not.toBeInTheDocument();
    expect(tealButtons()[0]).toHaveTextContent(DRIVES.ADD_CTA);
  });
});

describe("DrivesScreen — the editor offers only this runner's backends (Q3)", () => {
  const openNew = async () => {
    await screen.findByText(HOMES.name);
    await userEvent.click(screen.getByRole("button", { name: DRIVES.NEW_CTA }));
    return screen.findByTestId("drives-drive-editor");
  };

  it("Kubernetes offers its pair and neither Docker one, and the managed claim's size is REQUIRED (Q7)", async () => {
    renderScreen();
    const editor = await openNew();

    expect(within(editor).getByText(DRIVES.BACKEND_K8S_PVC)).toBeInTheDocument();
    expect(within(editor).getByText(DRIVES.BACKEND_K8S_PVC_STATIC)).toBeInTheDocument();
    expect(within(editor).queryByText(DRIVES.BACKEND_DOCKER_VOLUME)).not.toBeInTheDocument();
    expect(within(editor).queryByText(DRIVES.BACKEND_HOST_PATH)).not.toBeInTheDocument();

    // k8s_pvc is the default selection on this runner, and its size hint is the
    // one that carries the word "Required" — there is no marker glyph.
    expect(within(editor).getByText(DRIVES.SIZE_HINT_REQUIRED)).toBeInTheDocument();
    expect(within(editor).queryByText(DRIVES.SIZE_HINT)).not.toBeInTheDocument();
    // A managed backend keeps the storage-class field; a share never shows one.
    expect(within(editor).getByLabelText(DRIVES.FIELD_STORAGE_CLASS)).toBeInTheDocument();
  });

  it("a share disables the derived directory name and moves off it rather than authoring a refusal", async () => {
    renderScreen();
    const editor = await openNew();
    // The derived id is legal on the managed default.
    expect(within(editor).getByText(DRIVES.HOME_HASH).closest("button")).not.toBeDisabled();

    await userEvent.click(within(editor).getByText(DRIVES.BACKEND_K8S_PVC_STATIC));

    expect(within(editor).getByText(DRIVES.HOME_HASH).closest("button")).toBeDisabled();
    // A share's directories are named by the corporation's own directory, so
    // the selection moved with the backend instead of waiting for the 400.
    expect(within(editor).getByText(DRIVES.HOME_SUB).closest("button")).toHaveAttribute("aria-pressed", "true");
    // …and the share loses the storage class, gains nothing else.
    expect(within(editor).queryByLabelText(DRIVES.FIELD_STORAGE_CLASS)).not.toBeInTheDocument();
  });

  it("Docker with no WARDYN_USER_DRIVE_HOST_ROOTS disables host_path WITH ITS REASON, never offers-and-refuses", async () => {
    renderScreen(snapshot({ runner_target: "docker", host_roots_configured: false }));
    const editor = await openNew();

    expect(within(editor).getByText(DRIVES.BACKEND_DOCKER_VOLUME)).toBeInTheDocument();
    const share = within(editor).getByText(DRIVES.BACKEND_HOST_PATH).closest("button");
    expect(share).toBeDisabled();
    // The reason IS the option's hint (withMono splits the env var out).
    expect(document.body.textContent).toContain(DRIVES.BACKEND_UNAVAILABLE_DOCKER_ROOTS);
    expect(screen.getAllByText("WARDYN_USER_DRIVE_HOST_ROOTS")[0].className).toContain("font-mono");
  });

  it("Docker WITH roots set offers host_path, and selecting it reveals the Host root field", async () => {
    renderScreen(snapshot({ runner_target: "docker", host_roots_configured: true }));
    const editor = await openNew();

    const share = within(editor).getByText(DRIVES.BACKEND_HOST_PATH).closest("button")!;
    expect(share).not.toBeDisabled();
    await userEvent.click(share);
    expect(within(editor).getByLabelText(DRIVES.FIELD_HOST_ROOT)).toBeInTheDocument();
  });

  it("a save refusal is the SERVER's message under the console's heading; a transport failure is SAVE_ERROR", async () => {
    const body =
      'host_root "/mnt/nas/homes" is not inside WARDYN_USER_DRIVE_HOST_ROOTS (/srv/nas) — a drive may bind only a subdirectory of a root this deployment allows';
    updateDriveMock.mockRejectedValueOnce(new HttpError(400, body));
    renderScreen();
    await screen.findByText(HOMES.name);
    await userEvent.click(screen.getByRole("button", { name: `${DRIVES.EDIT} ${HOMES.name}` }));
    await userEvent.click(screen.getByRole("button", { name: DRIVES.SAVE_CTA }));

    expect(await screen.findByText(DRIVES.SAVE_REFUSED_TITLE)).toBeInTheDocument();
    // Verbatim: the server names the path and the roots, which no frozen
    // sentence could — the roots are an env-borne ceiling the console cannot read.
    expect(screen.getByText(body)).toBeInTheDocument();
    expect(screen.getByTestId("drives-drive-editor")).toBeInTheDocument();

    updateDriveMock.mockRejectedValueOnce(new TypeError("Failed to fetch"));
    await userEvent.click(screen.getByRole("button", { name: DRIVES.SAVE_CTA }));
    expect(await screen.findByText(DRIVES.SAVE_ERROR)).toBeInTheDocument();
  });
});

describe("DrivesScreen — delete: the pre-fill and the race are different refusals", () => {
  it("a visibly-allocated drive opens PRE-FILLED with the count, confirm disabled, nothing attempted", async () => {
    renderScreen();
    await screen.findByText(HOMES.name);

    await userEvent.click(screen.getByRole("button", { name: `${DRIVES.DELETE} ${HOMES.name}` }));
    const dialog = await screen.findByRole("alertdialog");

    expect(within(dialog).getByText(DRIVES.DELETE_RESTRICT_TITLE)).toBeInTheDocument();
    expect(within(dialog).getByText(DRIVES.DELETE_RESTRICT_BODY(HOMES.name, 2))).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: DRIVES.DELETE })).toBeDisabled();
    expect(deleteDriveMock).not.toHaveBeenCalled();
  });

  it("an unallocated drive confirms with the frozen consequence, and deletes", async () => {
    const LOOSE = drive({ id: "d3", name: "Workbench", grant_count: 0 });
    deleteDriveMock.mockResolvedValue(undefined);
    renderScreen(snapshot({ drives: [LOOSE] }));
    await screen.findByText(LOOSE.name);

    await userEvent.click(screen.getByRole("button", { name: `${DRIVES.DELETE} ${LOOSE.name}` }));
    const dialog = await screen.findByRole("alertdialog");
    const [head, body] = question(DRIVES.DELETE_CONFIRM(LOOSE.name));
    expect(within(dialog).getByText(head)).toBeInTheDocument();
    expect(within(dialog).getByText(body)).toBeInTheDocument();
    expect(within(dialog).queryByText(DRIVES.DELETE_RESTRICT_TITLE)).not.toBeInTheDocument();

    await userEvent.click(within(dialog).getByRole("button", { name: DRIVES.DELETE }));
    expect(deleteDriveMock).toHaveBeenCalledWith(LOOSE.id);
  });

  it("the race: a 409 is rendered COUNT-FREE, post-attempt, with the confirm still enabled", async () => {
    const conflict =
      "this drive is still allocated — remove its allocations first (deleting it while allocated would leave those subjects with a mount that names nothing)";
    const LOOSE = drive({ id: "d3", name: "Workbench", grant_count: 0 });
    deleteDriveMock.mockRejectedValue(new HttpError(409, conflict));
    renderScreen(snapshot({ drives: [LOOSE] }));
    await screen.findByText(LOOSE.name);

    await userEvent.click(screen.getByRole("button", { name: `${DRIVES.DELETE} ${LOOSE.name}` }));
    const dialog = await screen.findByRole("alertdialog");
    const confirm = within(dialog).getByRole("button", { name: DRIVES.DELETE });
    await userEvent.click(confirm);

    expect(await within(dialog).findByText(DRIVES.DELETE_RESTRICT_TITLE)).toBeInTheDocument();
    expect(within(dialog).getByText(conflict)).toBeInTheDocument();
    // COUNT-FREE: the client believed the count was zero and the shipped 409
    // carries no n, so the count-bearing body must NOT appear on this path.
    expect(within(dialog).queryByText(DRIVES.DELETE_RESTRICT_BODY(LOOSE.name, 0))).not.toBeInTheDocument();
    expect(confirm).not.toBeDisabled();
  });
});

describe("DrivesScreen — allocations", () => {
  it("renders both halves of the effect fact, the precedence rule, and the tier vocabulary", async () => {
    renderScreen();
    await screen.findByText(DRIVES.ALLOC_TITLE);
    expect(screen.getByText(DRIVES.PRECEDENCE)).toBeInTheDocument();
    expect(screen.getByText(DRIVES.EFFECT_NOTE)).toBeInTheDocument();
    expect(screen.getByText(DRIVES.SIGNIN_NOTE)).toBeInTheDocument();
    // The subject vocabulary is permissions-copy.ts's, on the row as on the
    // segmented control that authors it.
    const allocations = within(screen.getAllByRole("table")[1]);
    expect(allocations.getByText(PERM.SUBJECT_GROUP)).toBeInTheDocument();
    // The size override renders through the ONE size helper, as a chip.
    expect(allocations.getByText(DRIVES.OVERRIDE_SIZE(sizeText(16384)))).toBeInTheDocument();
  });

  it("priority is shown only inside the group tier, and a user row's overrides carry the directory", async () => {
    renderScreen(
      snapshot({
        grants: [
          {
            id: "g2",
            subject_type: "user",
            subject: "bob@corp.example",
            drive_id: HOMES.id,
            priority: 7,
            home_override: "bsmith",
            writable_override: true,
            enabled: true,
            created_at: "2026-08-30T00:00:00Z",
          },
        ],
      }),
    );
    await screen.findByText("bob@corp.example");
    expect(screen.getByText(GOV.PRIORITY_NA)).toBeInTheDocument();
    expect(screen.getByText(DRIVES.OVERRIDE_HOME("bsmith"))).toBeInTheDocument();
    // A writable override renders in the mode's own chip vocabulary.
    expect(screen.getAllByText(DRIVES.MODE_RW).length).toBeGreaterThan(1);
  });

  it("a paused allocation is a chip on the row, not a missing row", async () => {
    renderScreen(
      snapshot({
        grants: [
          {
            id: "g3",
            subject_type: "user",
            subject: "carol@corp.example",
            drive_id: HOMES.id,
            priority: 0,
            enabled: false,
            created_at: "2026-08-30T00:00:00Z",
          },
        ],
      }),
    );
    await screen.findByText("carol@corp.example");
    expect(screen.getByText(DRIVES.PAUSED_CHIP)).toBeInTheDocument();
    expect(screen.queryByText(DRIVES.OVERRIDES_NONE)).not.toBeInTheDocument();
  });

  it("a directory name is a person's: the field is disabled off the user tier and says which rows may name one", async () => {
    renderScreen();
    await screen.findByText(DRIVES.ALLOC_TITLE);
    // The form opens on the group tier.
    expect(screen.getByLabelText(DRIVES.FIELD_HOME_OVERRIDE)).toBeDisabled();
    expect(screen.getByText(DRIVES.HOME_OVERRIDE_NA)).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: PERM.SUBJECT_USER }));
    expect(screen.getByLabelText(DRIVES.FIELD_HOME_OVERRIDE)).not.toBeDisabled();
    expect(screen.getByText(DRIVES.HOME_OVERRIDE_HINT)).toBeInTheDocument();
  });

  it("an upsert that repointed an existing row says so inline — and a fresh one does not", async () => {
    upsertGrantMock.mockResolvedValueOnce({ grant: {}, replaced: true });
    renderScreen();
    await screen.findByText(DRIVES.ALLOC_TITLE);

    await userEvent.type(screen.getByRole("textbox", { name: PERM.FIELD_WHO }), "wardyn.platform");
    await userEvent.click(screen.getByRole("combobox"));
    await userEvent.click(await screen.findByRole("option", { name: SCRATCH.name }));
    await userEvent.click(screen.getByRole("button", { name: DRIVES.ADD_CTA }));

    expect(await screen.findByText(DRIVES.ALLOC_REPLACED)).toBeInTheDocument();
    // "Same as the drive" is the wire's absent *bool, so no key is sent at all.
    expect(upsertGrantMock).toHaveBeenCalledWith({
      subject_type: "group",
      subject: "wardyn.platform",
      drive_id: SCRATCH.id,
      priority: 0,
      size_mib_override: 0,
      home_override: "",
      enabled: true,
    });

    upsertGrantMock.mockResolvedValueOnce({ grant: {}, replaced: false });
    await userEvent.type(screen.getByRole("textbox", { name: PERM.FIELD_WHO }), "wardyn.other");
    await userEvent.click(screen.getByRole("button", { name: DRIVES.ADD_CTA }));
    expect(await screen.findByText(DRIVES.ADD_TITLE)).toBeInTheDocument();
    expect(screen.queryByText(DRIVES.ALLOC_REPLACED)).not.toBeInTheDocument();
  });

  it("Remove is outline and reversible: it confirms with the frozen sentence and deletes no data", async () => {
    deleteGrantMock.mockResolvedValue(undefined);
    renderScreen();
    await screen.findByText("wardyn.platform");

    await userEvent.click(screen.getByRole("button", { name: `${PERM.REMOVE} wardyn.platform` }));
    const dialog = await screen.findByRole("alertdialog");
    const [head, body] = question(DRIVES.REMOVE_CONFIRM("wardyn.platform", SCRATCH.name));
    expect(within(dialog).getByText(head)).toBeInTheDocument();
    expect(within(dialog).getByText(body)).toBeInTheDocument();

    const confirm = within(dialog).getByRole("button", { name: PERM.REMOVE });
    expect(confirm.className).not.toContain("bg-danger");
    await userEvent.click(confirm);
    expect(deleteGrantMock).toHaveBeenCalledWith("g1");
  });
});

describe("DrivesScreen — the preview has three arms", () => {
  const ask = async (claims = "wardyn.platform") => {
    await screen.findByText(DRIVES.PREVIEW_TITLE);
    await userEvent.type(screen.getByLabelText(PREVIEW.FIELD_CLAIMS), claims);
    await userEvent.click(screen.getByRole("button", { name: DRIVES.PREVIEW_CTA }));
  };

  it("a drive: the name, the tier, the directory, the object an offboarding command needs, the size and its gloss", async () => {
    previewDriveMock.mockResolvedValue({
      drive_name: SCRATCH.name,
      matched_tier: "group",
      home_name: "d-3f9a1c7e2b64d0a5e8c1",
      object_name: "wardyn-drive-scratch-d-3f9a1c7e2b64d0a5e8c1",
      size_mib: 16384,
      writable: true,
      enforcement: "request",
    });
    renderScreen();
    await ask();

    expect(
      await screen.findByText(DRIVES.PREVIEW_RESULT(SCRATCH.name, DRIVES.PREVIEW_TIER_GROUP)),
    ).toBeInTheDocument();
    expect(screen.getByText("d-3f9a1c7e2b64d0a5e8c1").className).toContain("font-mono");
    // The ONLY surface that names a storage object — no member string ever does.
    expect(screen.getByText("wardyn-drive-scratch-d-3f9a1c7e2b64d0a5e8c1").className).toContain("font-mono");
    expect(screen.getByText(DRIVES.PREVIEW_OBJECT_HINT)).toBeInTheDocument();
    expect(screen.getByText(DRIVES.SIZE_GIB(16))).toBeInTheDocument();
    expect(screen.getByText(GOV.PREVIEW_NOT_SAVED)).toBeInTheDocument();
  });

  it("no drive: an empty object is 'nobody' rather than a failure", async () => {
    renderScreen();
    await ask("nobody");
    expect(await screen.findByText(DRIVES.PREVIEW_NONE)).toBeInTheDocument();
  });

  it("paused: the drive and the tier, no directory and no storage object — nothing above a paused row is derived", async () => {
    previewDriveMock.mockResolvedValue({
      drive_name: HOMES.name,
      matched_tier: "user",
      writable: false,
      enforcement: "external",
      paused: true,
    });
    renderScreen();
    await ask("carol@corp.example");

    expect(
      await screen.findByText(DRIVES.PREVIEW_RESULT(HOMES.name, DRIVES.PREVIEW_TIER_USER)),
    ).toBeInTheDocument();
    expect(screen.getByText(DRIVES.PAUSED_CHIP)).toBeInTheDocument();
    const result = within(screen.getByTestId("drives-preview-result"));
    expect(result.queryByText(DRIVES.PREVIEW_OBJECT_LABEL)).not.toBeInTheDocument();
    expect(result.queryByText(DRIVES.FIELD_HOME)).not.toBeInTheDocument();
    // The folded size and mode DO run for a paused row: what the member is
    // shown is the allocation that is off.
    expect(result.getByText(DRIVES.PREVIEW_ENFORCEMENT_LABEL)).toBeInTheDocument();
  });

  it("a failed preview is the one honest cause of PREVIEW_RESULT_UNKNOWN", async () => {
    renderScreen();
    previewDriveMock.mockRejectedValueOnce(new Error("boom"));
    await ask();
    expect(await screen.findByText(GOV.PREVIEW_RESULT_UNKNOWN)).toBeInTheDocument();
  });

  // The drive resolver reads user_subjects POSITIONALLY to pick which claim an
  // `email_local` home is named from, so the order is load-bearing here in a way
  // it is not for a governance ceiling: an "@" line must arrive last.
  it("offers every claim to both tiers, sign-in subject before email", async () => {
    renderScreen();
    await ask("alice@corp.example{enter}8f2c-sub-id{enter}eng");
    expect(previewDriveMock).toHaveBeenCalledWith({
      user_subjects: ["8f2c-sub-id", "eng", "alice@corp.example"],
      groups: ["alice@corp.example", "8f2c-sub-id", "eng"],
    });
  });
});

describe("DrivesScreen — the write gate", () => {
  it("a caller without the SUPER tier gets every write control disabled", async () => {
    // OperatorProvider defaults operator TRUE (fail-open), so the restricted
    // case is the one worth pinning: it must come from the provider.
    getDrivesMock.mockResolvedValue(snapshot());
    render(
      <OperatorProvider operator={false} securityOperator={false}>
        <DrivesScreen />
      </OperatorProvider>,
    );
    await screen.findByText(HOMES.name);

    expect(screen.getByRole("button", { name: DRIVES.NEW_CTA })).toBeDisabled();
    expect(screen.getByRole("button", { name: `${DRIVES.EDIT} ${HOMES.name}` })).toBeDisabled();
    expect(screen.getByRole("button", { name: `${DRIVES.DELETE} ${HOMES.name}` })).toBeDisabled();
    expect(screen.getByRole("button", { name: DRIVES.ADD_CTA })).toBeDisabled();
  });
});

// THE canon pin: a component in this directory may render copy, never author
// it. Anything a reader sees comes from user-drives-copy.ts (frozen §7) or the
// modules §7.1 defers to — so a quoted or bare prose string in this source is a
// canon break, not a style question.
describe("Drives components render no copy of their own", () => {
  const FILES = ["drives-screen.tsx", "drive-editor.tsx", "allocations.tsx", "display.tsx"];
  const DIR = "src/app/components/screens/drives";
  const source = (f: string) => readFileSync(join(process.cwd(), DIR, f), "utf8");

  const strip = (src: string) =>
    src
      .replace(/\/\*[\s\S]*?\*\//g, "")
      .replace(/^\s*\/\/.*$/gm, "")
      .replace(/\{\/\*[\s\S]*?\*\/\}/g, "")
      .replace(/className=(\{[^{}]*\}|"[^"]*")/g, "")
      .replace(/\bcn\([^()]*\)/g, "")
      .replace(/data-testid="[^"]*"/g, "");

  const PROSE = /[A-Za-z]{2,}\s+[A-Za-z]{2,}/;

  it.each(FILES)("%s holds no quoted product prose", (f) => {
    const quoted = [...strip(source(f)).matchAll(/"([^"\n]*)"|'([^'\n]*)'/g)]
      .map((m) => m[1] ?? m[2])
      .filter((s) => PROSE.test(s));
    expect(quoted).toEqual([]);
  });

  it.each(FILES)("%s holds no bare JSX text node either", (f) => {
    const bare = [...strip(source(f)).matchAll(/>([^<>{}\n]{4,})</g)]
      .map((m) => m[1].trim())
      .filter((s) => PROSE.test(s));
    expect(bare).toEqual([]);
  });
});

// SIZE_MIB / SIZE_GIB / SIZE_NONE through the ONE helper (§5 #10) — never
// lib/format.ts's fmtBytes, which labels a binary quotient "MB" and stops at
// megabytes.
describe("sizeText — the one size helper", () => {
  it.each([
    [0, DRIVES.SIZE_NONE],
    [undefined, DRIVES.SIZE_NONE],
    [512, DRIVES.SIZE_MIB(512)],
    [1024, DRIVES.SIZE_GIB(1)],
    [16384, DRIVES.SIZE_GIB(16)],
    [1500, DRIVES.SIZE_MIB(1500)],
  ])("%s renders %s", (mib, want) => {
    expect(sizeText(mib as number | undefined)).toBe(want);
  });
});
