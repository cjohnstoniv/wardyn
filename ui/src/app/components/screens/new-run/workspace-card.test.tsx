/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Workspace card's drive block, one case per state the two /me bits can
// be in. Every expected string is IMPORTED from the frozen copy module — an
// assertion that retyped the wording would pass while the shipped sentence
// drifted, which is the exact failure the canon module exists to prevent.
//
// This is a component test rather than an e2e because the states are a
// property of the WIRE, not of the machine running the suite: whether a member
// has a paused allocation or a shut door cannot be arranged against a live
// daemon without an admin fixture per case.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { MeUserDrive } from "../../../lib/api/health";
import { baseMeDrive } from "../../../lib/test-fixtures";
import { DRIVES, DRIVE_MEMBER as DM } from "../../../lib/user-drives-copy";
import { WorkspaceCard } from "./workspace-card";
import { initialWizardState, type WizardState } from "./wizard-types";

const user = userEvent.setup({ pointerEventsCheck: 0 });

// The shared /me drive fixture the other three drive suites already import —
// byte-for-byte what this file used to declare locally.
const drive = baseMeDrive;

// Renders the card with a live WizardState, so a click on the checkbox is
// asserted through the SAME patch the screen wires up — a test that stubbed
// patch would pass with the wiring inverted.
function renderCard(opts: {
  drive?: MeUserDrive | null;
  deniedBy?: string;
  state?: Partial<WizardState>;
} = {}) {
  const patch = vi.fn();
  const state = { ...initialWizardState(), ...opts.state };
  const view = render(
    <WorkspaceCard
      state={state}
      patch={patch}
      workspaces={[]}
      caps={null}
      onAddWorkspace={() => {}}
      drive={opts.drive ?? null}
      driveDeniedBy={opts.deniedBy ?? ""}
    />,
  );
  return { patch, view };
}

// The absent-row doctrine: nothing allocated and no door is today's card, byte
// for byte. Not a disabled checkbox, not a placeholder line — nothing.
describe("WorkspaceCard — no allocation and no door", () => {
  it("renders neither the checkbox nor a reason line", () => {
    renderCard();
    expect(screen.queryByTestId("nr-drive")).toBeNull();
    expect(screen.queryByTestId("nr-drive-reason")).toBeNull();
    expect(screen.queryByText(DM.NR_CHECKBOX)).toBeNull();
    // The card itself is untouched — the Select and its ghost button remain.
    expect(screen.getByText("Ephemeral scratch — no repo")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Add workspace/ })).toBeInTheDocument();
  });
});

describe("WorkspaceCard — a writable allocation", () => {
  it("offers the checkbox, the hint and the writable sentence", () => {
    renderCard({ drive: drive() });
    expect(screen.getByText(DM.NR_CHECKBOX)).toBeInTheDocument();
    // The mount target renders mono, so the hint is split across sibling text
    // nodes — match the paragraph's textContent rather than one node.
    const hint = DM.NR_HINT("Scratch", DRIVES.SIZE_GIB(16), DRIVES.MODE_RW_INLINE);
    expect(screen.getByTestId("nr-drive").textContent).toContain(hint);
    expect(screen.getByTestId("nr-drive").textContent).toContain(DM.NR_RW_NOTE);
    expect(screen.getByTestId("nr-drive").textContent).not.toContain(DM.NR_RO_NOTE);
  });

  // A NARROWING NEEDS A MOUNT TO NARROW. The toggle rides the checkbox, the
  // way the mock draws it (7a/7b are both mount-ON; nothing draws it with the
  // mount off) — and it still defaults OFF once it appears (Q5).
  it("offers no narrowing until the mount is ticked, then offers it off", () => {
    renderCard({ drive: drive() });
    expect(screen.queryByText(DM.NR_READONLY_TOGGLE)).toBeNull();

    renderCard({ drive: drive(), state: { driveEnabled: true } });
    expect(screen.getByText(DM.NR_READONLY_TOGGLE)).toBeInTheDocument();
    expect(screen.getByLabelText(DM.NR_READONLY_TOGGLE)).not.toBeChecked();
  });

  // The half of that which is not cosmetic: with the mount off, `drive` is not
  // on the wire at all (wizard-spec.ts), so a retained driveReadOnly must not
  // rewrite the OFFER into a promise of a read-only mount this run will not
  // make. The sentence describes the allocation as the admin granted it.
  it("an unmounted drive is offered as granted, whatever driveReadOnly holds", () => {
    renderCard({ drive: drive(), state: { driveEnabled: false, driveReadOnly: true } });
    const block = screen.getByTestId("nr-drive").textContent ?? "";
    expect(block).toContain(DM.NR_HINT("Scratch", DRIVES.SIZE_GIB(16), DRIVES.MODE_RW_INLINE));
    expect(block).toContain(DM.NR_RW_NOTE);
    expect(block).not.toContain(DM.NR_RO_NOTE);
    expect(screen.queryByText(DM.NR_READONLY_TOGGLE)).toBeNull();
  });

  it("renders the target in mono without baking the span into the canon string", () => {
    renderCard({ drive: drive() });
    const mono = screen.getByTestId("nr-drive").querySelector(".font-mono");
    expect(mono?.textContent).toBe("/home/agent/drive");
  });

  it("flips BOTH the hint's mode and the sentence when the run narrows it", () => {
    renderCard({ drive: drive(), state: { driveEnabled: true, driveReadOnly: true } });
    const block = screen.getByTestId("nr-drive").textContent ?? "";
    expect(block).toContain(DM.NR_HINT("Scratch", DRIVES.SIZE_GIB(16), DRIVES.MODE_RO_INLINE));
    expect(block).toContain(DM.NR_RO_NOTE);
    // The one thing a read-only mount must never promise.
    expect(block).not.toContain(DM.NR_RW_NOTE);
    expect(screen.getByLabelText(DM.NR_READONLY_TOGGLE)).toBeChecked();
  });

  it("ticks driveEnabled through the screen's own patch", async () => {
    const { patch } = renderCard({ drive: drive() });
    await user.click(screen.getByLabelText(DM.NR_CHECKBOX));
    expect(patch).toHaveBeenCalledWith({ driveEnabled: true });
  });

  it("ticks driveReadOnly through the same patch", async () => {
    const { patch } = renderCard({ drive: drive(), state: { driveEnabled: true } });
    await user.click(screen.getByLabelText(DM.NR_READONLY_TOGGLE));
    expect(patch).toHaveBeenCalledWith({ driveReadOnly: true });
  });

  it("renders a non-GiB allocation in MiB", () => {
    renderCard({ drive: drive({ size_mib: 1536 }) });
    expect(screen.getByTestId("nr-drive").textContent).toContain(DRIVES.SIZE_MIB(1536));
  });
});

describe("WorkspaceCard — a read-only allocation", () => {
  it("offers the checkbox with the read-only sentence and NO narrowing toggle", () => {
    renderCard({ drive: drive({ name: "Corporate homes", writable: false }) });
    const block = screen.getByTestId("nr-drive").textContent ?? "";
    expect(block).toContain(DM.NR_RO_NOTE);
    // A run may narrow what an admin granted, never widen it — so there is no
    // control here at all, not a disabled one.
    expect(screen.queryByText(DM.NR_READONLY_TOGGLE)).toBeNull();
  });

  it("takes the _NOSIZE twin when no allocation is shown", () => {
    renderCard({ drive: drive({ name: "Corporate homes", writable: false, size_mib: 0 }) });
    const block = screen.getByTestId("nr-drive").textContent ?? "";
    expect(block).toContain(DM.NR_HINT_NOSIZE("Corporate homes", DRIVES.MODE_RO_INLINE));
    // SIZE_NONE is the ADMIN's size cell — it never appears in a member's
    // sentence, which is the whole reason the _NOSIZE twins exist.
    expect(block).not.toContain(DRIVES.SIZE_NONE);
  });
});

describe("WorkspaceCard — paused", () => {
  it("renders one line where the checkbox would be, not a disabled checkbox", () => {
    renderCard({ drive: drive({ paused: true }) });
    expect(screen.getByTestId("nr-drive-reason")).toHaveTextContent(DM.NR_PAUSED);
    expect(screen.queryByText(DM.NR_CHECKBOX)).toBeNull();
    expect(screen.queryByTestId("nr-drive")).toBeNull();
  });
});

// The door is a property of the caller's PROFILE, not of the allocation, so it
// refuses both with and without one — and the without case is the reason it is
// a sibling field: "ask an admin for an allocation" would be the wrong advice.
describe("WorkspaceCard — the governance door", () => {
  const profile = "Greenfield contractors";

  it("names the profile where the checkbox would be, with an allocation", () => {
    renderCard({ drive: drive(), deniedBy: profile });
    expect(screen.getByTestId("nr-drive-reason")).toHaveTextContent(DM.NR_DENIED(profile));
    expect(screen.queryByText(DM.NR_CHECKBOX)).toBeNull();
  });

  // With no allocation AND no door there is no line at all (the absent row
  // above), so a line here can only have come from the door — and there is no
  // no-allocation caption left to rule out: §7.6 froze none, because that
  // state has nothing to render in. The launch-path answer to it is the
  // server's REFUSED_NO_GRANT, which is a reply to an attempt.
  it("names it with NO allocation too — the door, and only the door", () => {
    renderCard({ drive: null, deniedBy: profile });
    expect(screen.getAllByTestId("nr-drive-reason")).toHaveLength(1);
    expect(screen.getByTestId("nr-drive-reason")).toHaveTextContent(DM.NR_DENIED(profile));
  });

  it("outranks a paused allocation — one reason, not two", () => {
    renderCard({ drive: drive({ paused: true }), deniedBy: profile });
    expect(screen.getAllByTestId("nr-drive-reason")).toHaveLength(1);
    expect(screen.getByTestId("nr-drive-reason")).toHaveTextContent(DM.NR_DENIED(profile));
  });
});
