/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The ALLOCATIONS half of the drives console — the allocation list, the form
// that writes one, and the preview that answers "who gets which drive". Split
// out of drives-screen.test.tsx at the seam its own source has (allocations.tsx
// against drives-screen.tsx / drive-editor.tsx), when that file reached the
// 1000-line ceiling scripts/check-file-size.sh holds.
//
// Same convention as its sibling: every expected string is read from a copy
// module rather than retyped, so these fail the moment a rendered string stops
// coming from the canon. Three things are pinned here because they are
// DECISIONS, not rendering:
//   1. the form's WIRE SHAPE arm by arm — writable_override is a *bool whose
//      absence is a third state, home_override is a pointer whose absence keeps
//      a pinned directory name, and enabled=false is a pause, not a delete,
//   2. a refusal is rendered as the SERVER wrote it, never re-worded here,
//   3. the preview has three ANSWERS — a drive, no drive, and a PAUSED winner
//      that derives no directory and no storage object.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
// vi.mock is hoisted above this import, so `toast` here IS the spy below.
import { toast } from "sonner";

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
import { DRIVES, PERM, PREVIEW } from "../../../lib/user-drives-copy";
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
    // The chip's label around a MONO directory name — the name is a literal
    // path segment the resolver matches byte for byte, not part of the label.
    const home = screen.getByText("bsmith");
    expect(home.className).toContain("font-mono");
    expect(home.parentElement).toHaveTextContent(DRIVES.OVERRIDE_HOME("bsmith"));
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
    // "Same as the drive" is the wire's absent *bool, so no key is sent at all
    // — and neither is home_override, which this form never looked at.
    expect(upsertGrantMock).toHaveBeenCalledWith({
      subject_type: "group",
      subject: "wardyn.platform",
      drive_id: SCRATCH.id,
      priority: 0,
      size_mib_override: 0,
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

// What the form actually PUTS on the wire, arm by arm. The rendering above
// proves the controls exist; this proves each one reaches upsertGrant as the
// server's own field, because these five bodies are not interchangeable:
// writable_override is a *bool whose absence is a THIRD state, enabled=false is
// a pause rather than a delete, and home_override/size_mib_override are refused
// outright off the user tier.
describe("DrivesScreen — the allocation form's wire shapes", () => {
  // The body every arm below differs from by one key: the group tier, the
  // Scratch drive, and nothing overridden.
  const base = {
    subject_type: "group",
    subject: "wardyn.platform",
    drive_id: SCRATCH.id,
    priority: 0,
    size_mib_override: 0,
    enabled: true,
  };

  // Name a subject and pick a drive — the two fields Allocate is gated on.
  const fill = async (who = "wardyn.platform") => {
    await screen.findByText(DRIVES.ALLOC_TITLE);
    await userEvent.type(screen.getByRole("textbox", { name: PERM.FIELD_WHO }), who);
    await userEvent.click(screen.getByRole("combobox"));
    await userEvent.click(await screen.findByRole("option", { name: SCRATCH.name }));
  };
  const allocate = () => userEvent.click(screen.getByRole("button", { name: DRIVES.ADD_CTA }));

  beforeEach(() => upsertGrantMock.mockResolvedValue({ grant: {}, replaced: false }));

  it("inherit sends NO writable_override key at all — an absent *bool is the third state", async () => {
    renderScreen();
    await fill();
    await allocate();
    expect(upsertGrantMock).toHaveBeenCalledWith(base);
    expect(upsertGrantMock.mock.calls[0][0]).not.toHaveProperty("writable_override");
  });

  it("Writable sends writable_override: true", async () => {
    renderScreen();
    await fill();
    await userEvent.click(screen.getByRole("button", { name: DRIVES.MODE_RW }));
    await allocate();
    expect(upsertGrantMock).toHaveBeenCalledWith({ ...base, writable_override: true });
  });

  it("Read-only sends writable_override: false — never the absent key", async () => {
    renderScreen();
    await fill();
    await userEvent.click(screen.getByRole("button", { name: DRIVES.MODE_RO }));
    await allocate();
    expect(upsertGrantMock).toHaveBeenCalledWith({ ...base, writable_override: false });
  });

  it("the Enabled switch off sends enabled: false — a pause, not a removal", async () => {
    renderScreen();
    await fill();
    await userEvent.click(screen.getByRole("switch", { name: DRIVES.FIELD_ENABLED }));
    await allocate();
    expect(upsertGrantMock).toHaveBeenCalledWith({ ...base, enabled: false });
    expect(deleteGrantMock).not.toHaveBeenCalled();
  });

  // home_override is a POINTER on the wire, and the store writes the column
  // VERBATIM: an unstated key keeps whatever directory name is pinned, while a
  // sent "" clears it. A form that stated the key on every submit therefore
  // re-homed anyone whose allocation it repointed without ever looking at this
  // field — silently, which is the act the server now answers 409 for.
  it("an untouched directory field states NO home_override at all", async () => {
    renderScreen();
    await screen.findByText(DRIVES.ALLOC_TITLE);
    await userEvent.click(screen.getByRole("button", { name: PERM.SUBJECT_USER }));
    await fill("bob@corp.example");
    await allocate();

    expect(upsertGrantMock.mock.calls[0][0]).not.toHaveProperty("home_override");
    expect(upsertGrantMock).toHaveBeenCalledWith({
      ...base,
      subject_type: "user",
      subject: "bob@corp.example",
    });
  });

  // THE REFUSAL THAT UNSTATED KEY EXISTS TO PROVOKE. Omitting home_override is
  // what stops a repoint from clearing a pinned directory name — and on an
  // allocation that HAS one pinned, the server answers 409 rather than
  // re-homing that person silently. The console composes nothing for it: the
  // form renders the sentence the server wrote, under its own heading.
  //
  // Asserted on the toast SPY and not on rendered text, unlike the preview's
  // refusals: sonner is mocked module-wide at the top of this file, so a toast
  // has no DOM here at all. The rendered half is the second assertion — a
  // refused allocation leaves the Who field filled, because `setSubject("")`
  // runs only on the success path, so the admin still has the row to fix.
  it("a 409 on an unstated home_override renders the server's own sentence", async () => {
    const refusal =
      "this allocation pins a directory name and your request did not mention home_override — writing it would " +
      "CLEAR that name, so this subject's next run would mount a different object and the one holding their work " +
      'would be left behind with nothing in Wardyn naming it. Re-send with home_override set to the name you want ' +
      'kept, or to "" to drop it deliberately';
    upsertGrantMock.mockRejectedValueOnce(new HttpError(409, refusal));
    renderScreen();
    await screen.findByText(DRIVES.ALLOC_TITLE);
    await userEvent.click(screen.getByRole("button", { name: PERM.SUBJECT_USER }));
    await fill("bob@corp.example");
    await allocate();

    await waitFor(() => expect(toast.error).toHaveBeenCalledWith(DRIVES.ADD_TITLE, { description: refusal }));
    expect(screen.getByRole("textbox", { name: PERM.FIELD_WHO })).toHaveValue("bob@corp.example");
    expect(screen.queryByText(DRIVES.ALLOC_REPLACED)).not.toBeInTheDocument();
  });

  it("a name typed and then cleared states an EMPTY home_override — the deliberate drop", async () => {
    renderScreen();
    await screen.findByText(DRIVES.ALLOC_TITLE);
    await userEvent.click(screen.getByRole("button", { name: PERM.SUBJECT_USER }));
    await fill("bob@corp.example");
    const home = screen.getByLabelText(DRIVES.FIELD_HOME_OVERRIDE);
    await userEvent.type(home, "bsmith");
    await userEvent.clear(home);
    await allocate();

    expect(upsertGrantMock).toHaveBeenCalledWith({
      ...base,
      subject_type: "user",
      subject: "bob@corp.example",
      home_override: "",
    });
  });

  // The tier that may not carry one never states it either — a group row with
  // home_override: "" is a 400 the form would have authored for itself.
  it("a group row states no home_override", async () => {
    renderScreen();
    await fill();
    await allocate();
    expect(upsertGrantMock.mock.calls[0][0]).not.toHaveProperty("home_override");
  });

  it("a user-tier row carries its directory name and its size override", async () => {
    renderScreen();
    await screen.findByText(DRIVES.ALLOC_TITLE);
    // The tier first: home_override is accepted on a user row only, and the
    // field is disabled until this click.
    await userEvent.click(screen.getByRole("button", { name: PERM.SUBJECT_USER }));
    await fill("bob@corp.example");
    await userEvent.type(screen.getByLabelText(DRIVES.FIELD_HOME_OVERRIDE), "bsmith");
    await userEvent.type(screen.getByLabelText(DRIVES.FIELD_SIZE_OVERRIDE), "4096");
    await allocate();

    expect(upsertGrantMock).toHaveBeenCalledWith({
      ...base,
      subject_type: "user",
      subject: "bob@corp.example",
      size_mib_override: 4096,
      home_override: "bsmith",
    });
  });
});

describe("DrivesScreen — the preview's three answers and its refusals", () => {
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

  // A failure with NO BODY — a dropped connection, a thrown TypeError — is the
  // only thing left that PREVIEW_RESULT_UNKNOWN answers, because it is the only
  // thing the server did not put a sentence on.
  it("a bodiless failure is the one remaining cause of PREVIEW_RESULT_UNKNOWN", async () => {
    renderScreen();
    previewDriveMock.mockRejectedValueOnce(new Error("boom"));
    await ask();
    expect(await screen.findByText(GOV.PREVIEW_RESULT_UNKNOWN)).toBeInTheDocument();
  });

  // THE PANEL RUNS THE LAUNCH'S GATES NOW, so it has real refusals to show —
  // the governance door's 403 and the launch's 422s. Collapsing every non-2xx
  // into "couldn't resolve this" threw away the one answer an admin opened the
  // panel for. Rendered the way drive-editor.tsx already renders a refused
  // save: the server's message verbatim, no new component and no new string.
  it("the governance door's 403 renders the server's own sentence", async () => {
    renderScreen();
    const denied = 'drive: mounting a user drive is not allowed by your governance profile "Locked down". Launch without drive.';
    previewDriveMock.mockRejectedValueOnce(new HttpError(403, denied));
    await ask();
    expect(await screen.findByText(denied)).toBeInTheDocument();
    expect(screen.queryByText(GOV.PREVIEW_RESULT_UNKNOWN)).not.toBeInTheDocument();
  });

  it("a 422 for a home that is not on the share renders the launch's own refusal", async () => {
    renderScreen();
    const missing = "drive: directory bob does not exist on the share — ask an admin to create it";
    previewDriveMock.mockRejectedValueOnce(new HttpError(422, missing));
    await ask();
    expect(await screen.findByText(missing)).toBeInTheDocument();
    expect(screen.queryByText(GOV.PREVIEW_RESULT_UNKNOWN)).not.toBeInTheDocument();
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
