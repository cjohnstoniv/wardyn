/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// DrivesScreen — the admin's drive registry (0.7 user drives, slice D5a). Every
// assertion reads its expected string from a copy module rather than retyping
// it, the convention governance-screen.test.tsx already follows: these tests
// fail the moment a rendered string stops coming from the canon.
//
// THE REGISTRY HALF. The allocation list, the form that writes one and the
// preview live in allocations.test.tsx — split off at the seam their own source
// has when this file reached the 1000-line ceiling scripts/check-file-size.sh
// holds. The no-own-copy pin below still covers all four components, because it
// is a claim about the DIRECTORY, not about one screen.
//
// Five things are pinned here because they are DECISIONS, not rendering:
//   1. exactly one `default` (teal) button per state (CONSOLE-RULES §6),
//   2. the allocation form COLLAPSES while the editor is open — the mechanism
//      that keeps rule 1 true without inventing a modal,
//   3. the editor offers only THIS runner's two backends, and disables
//      `host_path` WITH ITS REASON when no roots are configured (Q3),
//   4. delete is TWO refusals: a pre-filled, count-bearing one the client can
//      see, and a COUNT-FREE 409 for the race it cannot,
//   5. no component in this directory renders a product string of its own.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expectNoOwnCopy } from "../../../lib/test-fixtures";

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
import { ACCESS_STATE, DRIVES, PEOPLE, PREVIEW } from "../../../lib/user-drives-copy";
import { OPERATOR_ONLY_REASON } from "../../wardyn/copy";
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
    // Mock state 1 is the header and this empty state — nothing else. The whole
    // allocations block is absent: with no drive registered there is nothing to
    // allocate, so "No allocations yet" would answer a question this admin
    // cannot ask yet, and the form under it would offer an empty Drive select.
    expect(screen.queryByText(DRIVES.ALLOC_TITLE)).toBeNull();
    expect(screen.queryByText(DRIVES.EMPTY_ALLOC_TITLE)).toBeNull();
    expect(screen.queryByText(DRIVES.PREVIEW_TITLE)).toBeNull();
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

  // LAYOUT, which textContent cannot see. Note is a flex COLUMN and withMono
  // returns an array, so handing it to Note directly made every contiguous text
  // run and the `disk_mib` Mono a flex item of its own: the one frozen sentence
  // painted as three stacked lines with the identifier orphaned on a full-width
  // line. Both existing assertions passed throughout — textContent concatenates
  // and Playwright's getByText normalises whitespace — so the DOM shape is what
  // has to be pinned: the flex column gets exactly ONE child, and that child
  // holds the whole sentence.
  it("the honesty note is ONE flex item, so the sentence flows as a paragraph", async () => {
    renderScreen();
    await screen.findByText(HOMES.name);

    const note = screen.getByText("disk_mib").closest(".flex-col");
    expect(note).not.toBeNull();
    expect(note!.childNodes).toHaveLength(1);
    expect((note!.firstChild as HTMLElement).textContent).toBe(DRIVES.HONESTY);
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

  it("drives but no allocations is its own state — and the preview still answers", async () => {
    renderScreen(snapshot({ grants: [] }));
    expect(await screen.findByText(DRIVES.EMPTY_ALLOC_TITLE)).toBeInTheDocument();
    expect(screen.getByText(DRIVES.EMPTY_ALLOC_BODY)).toBeInTheDocument();

    // The server's {} → PREVIEW_NONE is the honest answer here, and the one an
    // admin checking their work came for: the button is never gated on there
    // being rows to match, only on there being claims to ask about.
    await userEvent.type(screen.getByLabelText(PREVIEW.FIELD_CLAIMS), "wardyn.platform");
    const ask = screen.getByRole("button", { name: DRIVES.PREVIEW_CTA });
    expect(ask).not.toBeDisabled();
    await userEvent.click(ask);
    expect(await screen.findByText(DRIVES.PREVIEW_NONE)).toBeInTheDocument();
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

  // wardynd's DEFAULT runner is `none` (cmd/wardynd/boot_deps.go), and NO
  // backend's RunnerTarget() is `none` — types.ValidateUserDrive compares the
  // two, so on that deployment every save is refused, create and update alike.
  // The picker is therefore empty, and the Save that would compose a 400
  // naming a backend the admin never saw is not offered either: the `backend`
  // state's fallback is a type floor, not a choice.
  it("a runner that can mount NO backend offers none — and cannot save the one it fell back to", async () => {
    renderScreen(snapshot({ runner_target: "none" }));
    const editor = await openNew();

    for (const label of [
      DRIVES.BACKEND_DOCKER_VOLUME,
      DRIVES.BACKEND_HOST_PATH,
      DRIVES.BACKEND_K8S_PVC,
      DRIVES.BACKEND_K8S_PVC_STATIC,
    ]) {
      expect(within(editor).queryByText(label)).not.toBeInTheDocument();
    }
    // The one thing that otherwise ungates Save: a name.
    await userEvent.type(within(editor).getByLabelText(DRIVES.FIELD_NAME), "Anything");
    expect(within(editor).getByRole("button", { name: DRIVES.SAVE_CTA })).toBeDisabled();
    expect(createDriveMock).not.toHaveBeenCalled();
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

  it("a FRESH editor on a managed backend already has the subject-bearing names disabled", async () => {
    renderScreen();
    const editor = await openNew();
    // The mirror of the rule above, and the reason is not symmetry: Wardyn
    // mints a managed drive's volume itself, and a claim-derived name is not
    // unique across email domains — alice@corp and alice@partner would be one
    // volume — so the server refuses a claim template on a managed backend and
    // the editor must not offer the row it would refuse.
    expect(within(editor).getByText(DRIVES.HOME_SUB).closest("button")).toBeDisabled();
    expect(within(editor).getByText(DRIVES.HOME_EMAIL_LOCAL).closest("button")).toBeDisabled();
    expect(within(editor).getByText(DRIVES.HOME_HASH).closest("button")).toHaveAttribute("aria-pressed", "true");

    // Going out to a share and back must not leave `sub` selected on a managed
    // backend: the choice moves with the backend in BOTH directions, so the
    // round trip cannot author a row the server refuses.
    await userEvent.click(within(editor).getByText(DRIVES.BACKEND_K8S_PVC_STATIC));
    expect(within(editor).getByText(DRIVES.HOME_SUB).closest("button")).toHaveAttribute("aria-pressed", "true");

    await userEvent.click(within(editor).getByText(DRIVES.BACKEND_K8S_PVC));
    expect(within(editor).getByText(DRIVES.HOME_SUB).closest("button")).toBeDisabled();
    expect(within(editor).getByText(DRIVES.HOME_HASH).closest("button")).toHaveAttribute("aria-pressed", "true");
  });

  it("a managed backend disables the subject-bearing names and moves off them — the mirror of the share rule", async () => {
    renderScreen();
    const editor = await openNew();

    // Start on a share so a subject-bearing template is legitimately selected.
    await userEvent.click(within(editor).getByText(DRIVES.BACKEND_K8S_PVC_STATIC));
    expect(within(editor).getByText(DRIVES.HOME_SUB).closest("button")).toHaveAttribute("aria-pressed", "true");

    // Back to managed: the home segment is concatenated into the object name
    // that `docker volume ls` and `kubectl get pvc` print, so publishing the
    // subject there is refused by the API — for email_local always, and for
    // sub since 2026-09-03. The console must not offer a menu that 400s.
    await userEvent.click(within(editor).getByText(DRIVES.BACKEND_K8S_PVC));

    expect(within(editor).getByText(DRIVES.HOME_SUB).closest("button")).toBeDisabled();
    expect(within(editor).getByText(DRIVES.HOME_EMAIL_LOCAL).closest("button")).toBeDisabled();
    // …and the selection moved rather than waiting to be refused.
    expect(within(editor).getByText(DRIVES.HOME_HASH).closest("button")).not.toBeDisabled();
    expect(within(editor).getByText(DRIVES.HOME_HASH).closest("button")).toHaveAttribute("aria-pressed", "true");
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
    // …and PLAIN. It is prose that quotes wire facts, not a literal, so monoing
    // the whole sentence would claim otherwise — and a <Mono> regression is
    // invisible to an assertion that only checks the words.
    expect(screen.getByText(body).className).not.toContain("font-mono");
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
    // Same rule as the save refusal: the server's own sentence, PLAIN.
    expect(within(dialog).getByText(conflict).className).not.toContain("font-mono");
    // COUNT-FREE: the client believed the count was zero and the shipped 409
    // carries no n, so the count-bearing body must NOT appear on this path.
    expect(within(dialog).queryByText(DRIVES.DELETE_RESTRICT_BODY(LOOSE.name, 0))).not.toBeInTheDocument();
    expect(confirm).not.toBeDisabled();
  });

  // ...and the same 409 is the authority on the COUNT. ALLOCATED_NONE is what
  // let that dialog open unrestricted; the server has just said otherwise, so
  // the row is re-read. Without it the table keeps the count the server refused
  // and every later Delete on that row re-opens the same doomed confirm.
  it("the race: the 409 re-reads the list, so the row stops claiming nobody holds it", async () => {
    const conflict =
      "this drive is still allocated — remove its allocations first (deleting it while allocated would leave those subjects with a mount that names nothing)";
    const LOOSE = drive({ id: "d3", name: "Workbench", grant_count: 0 });
    deleteDriveMock.mockRejectedValue(new HttpError(409, conflict));
    // The first read is the stale one the dialog opened from; the re-read after
    // the refusal carries the allocation the other admin had just written.
    getDrivesMock.mockResolvedValueOnce(snapshot({ drives: [LOOSE] }));
    getDrivesMock.mockResolvedValue(snapshot({ drives: [{ ...LOOSE, grant_count: 1 }] }));
    render(<DrivesScreen />);
    await screen.findByText(LOOSE.name);
    expect(screen.getByText(DRIVES.ALLOCATED_NONE)).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: `${DRIVES.DELETE} ${LOOSE.name}` }));
    const dialog = await screen.findByRole("alertdialog");
    await userEvent.click(within(dialog).getByRole("button", { name: DRIVES.DELETE }));
    expect(await within(dialog).findByText(conflict)).toBeInTheDocument();

    expect(await screen.findByText(DRIVES.ALLOCATED_COUNT(1))).toBeInTheDocument();
    expect(screen.queryByText(DRIVES.ALLOCATED_NONE)).not.toBeInTheDocument();
    expect(getDrivesMock).toHaveBeenCalledTimes(2);
  });
});

describe("DrivesScreen — the write gate", () => {
  it("a caller without the SUPER tier gets every write control disabled", async () => {
    // OperatorProvider defaults operator TRUE (fail-open), so the restricted
    // case is the one worth pinning: it must come from the provider.
    //
    // A FIXTURE SPLICE, and it claims no more than that. GET /drives is
    // operatorOnly, so no REAL caller both reads this snapshot and lacks the
    // tier — what is pinned here is the wiring, that every write control takes
    // its `disabled` from the provider rather than from a local default. What a
    // real security admin actually meets is the next test.
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

  // The real one. /drives is operatorOnly and there is no nav item, no card and
  // no header button for this tier (mock state 9's note, §5 #7) — but a URL is
  // a URL, and the READ itself 403s. Answering that with FETCH_FAILED_* would
  // call an authorization answer a network fault and offer a Retry that returns
  // the same 403 forever, so the 403 gets the tier's own sentence instead: the
  // one every operator-only control already carries.
  it("a security admin's 403 read is the TIER refusal, not the transport one", async () => {
    getDrivesMock.mockRejectedValue(new HttpError(403, "forbidden"));
    render(
      <OperatorProvider operator={false} securityOperator={true}>
        <DrivesScreen />
      </OperatorProvider>,
    );

    expect(await screen.findByText(OPERATOR_ONLY_REASON)).toBeInTheDocument();
    // The page still names itself: an admin who reached the URL is told where
    // they are, not dropped onto a bare sentence.
    expect(screen.getByText(DRIVES.TITLE)).toBeInTheDocument();
    // NOT the transport copy, and no Retry — neither is true of a 403.
    expect(screen.queryByText(DRIVES.FETCH_FAILED_TITLE)).toBeNull();
    expect(screen.queryByText(DRIVES.FETCH_FAILED_BODY)).toBeNull();
    expect(screen.queryByRole("button", { name: ACCESS_STATE.FETCH_FAILED_RETRY })).toBeNull();
    // …and no registry at all: the refusal is of the READ, so there is nothing
    // behind it to show.
    expect(screen.queryByText(DRIVES.DRIVES_TITLE)).toBeNull();
    expect(screen.queryByRole("button", { name: DRIVES.NEW_CTA })).toBeNull();
  });

  // The other side of the same split: a transport failure is STILL the
  // transport failure. Splitting the catch must not swallow the state that was
  // already there.
  it("a non-403 read failure keeps FETCH_FAILED and its Retry", async () => {
    getDrivesMock.mockRejectedValue(new HttpError(500, "boom"));
    render(<DrivesScreen />);

    expect(await screen.findByText(DRIVES.FETCH_FAILED_TITLE)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: ACCESS_STATE.FETCH_FAILED_RETRY })).toBeInTheDocument();
    expect(screen.queryByText(OPERATOR_ONLY_REASON)).toBeNull();
  });
});

// THE canon pin: a component in this directory may render copy, never author
// it. Anything a reader sees comes from user-drives-copy.ts (frozen §7) or the
// modules §7.1 defers to — so a quoted or bare prose string in this source is a
// canon break, not a style question.
describe("Drives components render no copy of their own", () => {
  expectNoOwnCopy("src/app/components/screens/drives", [
    "drives-screen.tsx",
    "drive-editor.tsx",
    "allocations.tsx",
    "display.tsx",
  ]);
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
