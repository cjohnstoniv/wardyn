/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { MeUserDrive } from "../../../../../lib/api/health";
import { MEMBER } from "../../../../../lib/governance-copy";
import { LEGACY_USER_DRIVE, LEGACY_USER_DRIVE_READ_ONLY } from "../../../../../lib/new-run-fixtures/scenarios/drives";
import { DRIVE_MEMBER } from "../../../../../lib/user-drives-copy";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { OperatorProvider } from "../../../../wardyn/operator-context";
import { intentToWire } from "../../draft/intent-to-wire";
import type { RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { DrivesSection } from "./drives-section";

const ENV: RunIntent = { shape: "environment", experience: "interactive" };
const TASK: RunIntent = { shape: "agent_task", experience: "background" };

function Seed({ intent }: { intent: RunIntent }) {
  const dispatch = useNewRunDispatch();
  React.useEffect(() => dispatch({ type: "seed", origin: { kind: "scratch" }, intent }), [dispatch, intent]);
  return null;
}
const Drawn = () => <p data-testid="intent">{JSON.stringify(useNewRunDraft().intent)}</p>;
const drawn = (): RunIntent => JSON.parse(screen.getByTestId("intent").textContent!);
const wireDrive = () => (intentToWire(drawn()).body as Record<string, unknown> | null)?.drive;

interface Me {
  drive?: MeUserDrive | null;
  deniedBy?: string;
  unavailable?: string;
}

function show(intent: RunIntent, me: Me = {}) {
  render(
    <MemoryRouter>
      <OperatorProvider operator={false} userDrive={me.drive ?? null} userDriveDeniedByProfile={me.deniedBy ?? ""} userDriveUnavailable={me.unavailable ?? ""}>
        <NewRunDraftProvider owner="alice">
          <Seed intent={intent} />
          <DrivesSection />
          <Drawn />
        </NewRunDraftProvider>
      </OperatorProvider>
    </MemoryRouter>,
  );
}

afterEach(cleanup);

const mountBox = () => screen.getByRole("checkbox", { name: DRIVE_MEMBER.NR_CHECKBOX });

describe("the person's one drive (public arm)", () => {
  it("shows nothing for a person with no drive and no reason", () => {
    show(ENV, { drive: null });
    expect(screen.queryByRole("heading", { level: 2 })).toBeNull();
    expect(screen.queryByTestId("nr-drive")).toBeNull();
  });

  it("names the drive, its size and its mode behind an unchecked box, with no read-only toggle", () => {
    show(TASK, { drive: LEGACY_USER_DRIVE });
    expect(screen.getByRole("heading", { level: 2, name: NR_FILES.USER_DRIVES_TITLE })).toBeInTheDocument();
    expect(mountBox()).not.toBeChecked();
    expect(screen.getByTestId("nr-drive")).toHaveTextContent(/"Scratch" at \/home\/agent\/drive — 10 GiB, writable\./);
    expect(screen.getByTestId("nr-drive")).toHaveTextContent(DRIVE_MEMBER.NR_RW_NOTE);
    expect(screen.queryByLabelText(DRIVE_MEMBER.NR_READONLY_TOGGLE)).toBeNull();
    expect(wireDrive()).toBeUndefined();
  });

  it("mounting sends the mount and the read-only choice; unmounting sends no drive (R)", async () => {
    const user = userEvent.setup();
    show(TASK, { drive: LEGACY_USER_DRIVE });
    await user.click(mountBox());
    expect(wireDrive()).toEqual({ enabled: true });
    await user.click(screen.getByLabelText(DRIVE_MEMBER.NR_READONLY_TOGGLE));
    expect(wireDrive()).toEqual({ enabled: true, read_only: true });
    expect(screen.getByTestId("nr-drive")).toHaveTextContent(DRIVE_MEMBER.NR_RO_NOTE);
    await user.click(mountBox());
    expect(wireDrive()).toBeUndefined();
    expect(screen.queryByLabelText(DRIVE_MEMBER.NR_READONLY_TOGGLE)).toBeNull();
  });

  it("offers no read-only toggle on a drive an admin set read-only", async () => {
    show(TASK, { drive: LEGACY_USER_DRIVE_READ_ONLY });
    await userEvent.setup().click(mountBox());
    expect(screen.queryByLabelText(DRIVE_MEMBER.NR_READONLY_TOGGLE)).toBeNull();
    expect(screen.getByTestId("nr-drive")).toHaveTextContent(DRIVE_MEMBER.NR_RO_NOTE);
  });

  it.each([
    ["the governance door", { drive: LEGACY_USER_DRIVE, deniedBy: "Contractors" }, DRIVE_MEMBER.NR_DENIED("Contractors")],
    ["a paused drive", { drive: { ...LEGACY_USER_DRIVE, paused: true } }, DRIVE_MEMBER.NR_PAUSED],
    ["a stale groups snapshot", { unavailable: "groups_snapshot_stale" }, MEMBER.DENIED_STALE_GROUPS],
    ["governance that could not answer", { unavailable: "governance_unavailable" }, DRIVE_MEMBER.NR_GOVERNANCE_UNAVAILABLE],
    ["an unmountable drive", { unavailable: "unmountable" }, DRIVE_MEMBER.NR_UNAVAILABLE],
  ] satisfies [string, Me, string][])("says why in place of the checkbox: %s", (_, me, line) => {
    show(TASK, me);
    expect(screen.getByTestId("nr-drive-reason")).toHaveTextContent(line);
    expect(screen.queryByRole("checkbox")).toBeNull();
  });

  it("starts here only once mounted, and unmounting the starting drive clears the start", async () => {
    const user = userEvent.setup();
    show(TASK, { drive: LEGACY_USER_DRIVE });
    const starts = screen.getByRole("radio", { name: `${DRIVE_MEMBER.NR_CHECKBOX} ${NR_FILES.STARTS_HERE}` });
    expect(starts).toBeDisabled();
    await user.click(mountBox());
    await user.click(starts);
    expect(drawn().start_folder).toEqual({ kind: "attachment", attachment: "drive" });
    await user.click(mountBox());
    expect(drawn().start_folder).toBeUndefined();
  });
});

describe("Environment opens with the person's own drive (A18)", () => {
  it("mounts it and starts there when nothing is chosen yet", async () => {
    show(ENV, { drive: LEGACY_USER_DRIVE });
    await waitFor(() => expect(wireDrive()).toEqual({ enabled: true }));
    expect(drawn().start_folder).toEqual({ kind: "attachment", attachment: "drive" });
    expect(mountBox()).toBeChecked();
  });

  it("does not choose again once the person unchecks it", async () => {
    show(ENV, { drive: LEGACY_USER_DRIVE });
    await waitFor(() => expect(mountBox()).toBeChecked());
    await userEvent.setup().click(mountBox());
    expect(mountBox()).not.toBeChecked();
    expect(wireDrive()).toBeUndefined();
  });

  it.each([
    ["another shape", TASK, { drive: LEGACY_USER_DRIVE }],
    ["a choice already made", { ...ENV, no_repositories_or_drives: true }, { drive: LEGACY_USER_DRIVE }],
    ["a repository already included", { ...ENV, workspaces: [{ workspace_id: "w1" }] }, { drive: LEGACY_USER_DRIVE }],
    ["a drive that cannot attach", ENV, { drive: { ...LEGACY_USER_DRIVE, paused: true } }],
    ["a person with no drive", ENV, { drive: null }],
  ] satisfies [string, RunIntent, Me][])("leaves it alone for %s", (_, intent, me) => {
    show(intent, me);
    expect(wireDrive()).toBeUndefined();
  });
});
