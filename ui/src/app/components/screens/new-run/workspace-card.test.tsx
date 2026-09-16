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
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { MeUserDrive } from "../../../lib/api/health";
import { MEMBER } from "../../../lib/governance-copy";
import { baseMeDrive } from "../../../lib/test-fixtures";
import type { Workspace } from "../../../lib/types";
import { DRIVES, DRIVE_MEMBER as DM } from "../../../lib/user-drives-copy";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
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
  unavailable?: string;
  workspaces?: Workspace[];
  state?: Partial<WizardState>;
} = {}) {
  const patch = vi.fn();
  const state = { ...initialWizardState(), ...opts.state };
  const view = render(
    <WorkspaceCard
      state={state}
      patch={patch}
      workspaces={opts.workspaces ?? []}
      caps={null}
      onAddWorkspace={() => {}}
      drive={opts.drive ?? null}
      driveDeniedBy={opts.deniedBy ?? ""}
      driveUnavailable={opts.unavailable ?? ""}
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

// /me.user_drive_unavailable's four closed tokens (R1-F139 == R4-F052): the
// server already suppresses `drive` to null for every one of them, so each
// gets its own reason line instead of falling into the absent row.
describe("WorkspaceCard — /me could not answer for the drive", () => {
  it("groups_snapshot_stale renders the launch path's own MEMBER.DENIED_STALE_GROUPS verbatim", () => {
    renderCard({ drive: null, unavailable: "groups_snapshot_stale" });
    expect(screen.getByTestId("nr-drive-reason")).toHaveTextContent(MEMBER.DENIED_STALE_GROUPS);
    expect(screen.queryByTestId("nr-drive")).toBeNull();
  });

  it("unavailable renders NR_UNAVAILABLE", () => {
    renderCard({ drive: null, unavailable: "unavailable" });
    expect(screen.getByTestId("nr-drive-reason")).toHaveTextContent(DM.NR_UNAVAILABLE);
  });

  it("unmountable falls back to the same generic line as unavailable (no {reason} reaches /me)", () => {
    renderCard({ drive: null, unavailable: "unmountable" });
    expect(screen.getByTestId("nr-drive-reason")).toHaveTextContent(DM.NR_UNAVAILABLE);
  });

  it("governance_unavailable renders NR_GOVERNANCE_UNAVAILABLE", () => {
    renderCard({ drive: null, unavailable: "governance_unavailable" });
    expect(screen.getByTestId("nr-drive-reason")).toHaveTextContent(DM.NR_GOVERNANCE_UNAVAILABLE);
  });

  it("outranks an allocation the wire also sent (the server suppresses it, but the reason still wins)", () => {
    renderCard({ drive: drive(), unavailable: "unavailable" });
    expect(screen.getAllByTestId("nr-drive-reason")).toHaveLength(1);
    expect(screen.getByTestId("nr-drive-reason")).toHaveTextContent(DM.NR_UNAVAILABLE);
    expect(screen.queryByTestId("nr-drive")).toBeNull();
  });

  it("the governance door still outranks it — a named deny beats an unrelated resolve failure", () => {
    renderCard({ drive: null, deniedBy: "Greenfield contractors", unavailable: "unavailable" });
    expect(screen.getAllByTestId("nr-drive-reason")).toHaveLength(1);
    expect(screen.getByTestId("nr-drive-reason")).toHaveTextContent(DM.NR_DENIED("Greenfield contractors"));
  });
});

// A3's per-repo-source `admitted` flag (§5.3) — the workspace-row precedent's
// "not an enabled git provider" state, rendered here as the Select's own
// reason line.
describe("WorkspaceCard — a selected workspace's source is not an enabled provider", () => {
  function repoWorkspace(over: Partial<Workspace> = {}): Workspace {
    return {
      id: "ws-1",
      name: "payments",
      kind: "repo",
      source: "acme/payments",
      status: "scanned",
      created_at: "",
      updated_at: "",
      sources: [{ type: "repo", source: "acme/payments", admitted: false }],
      ...over,
    };
  }

  it("names it under the Select once that workspace is picked, with no base URL and no row id", () => {
    // A real https URL (rendered plainly elsewhere for any onboarded repo)
    // makes the "no base URL" check non-vacuous, and a distinctive id proves
    // neither leaks into the reason line specifically.
    const ws = repoWorkspace({
      id: "ws-secret-1",
      source: "https://git.acme.example/acme/payments",
      sources: [{ type: "repo", source: "https://git.acme.example/acme/payments", admitted: false }],
    });
    renderCard({ workspaces: [ws], state: { workspaces: [{ workspaceId: ws.id, enabledOptional: [] }] } });
    const text = screen.getByText(PROVIDERS.CARD_NOT_ADMITTED);
    expect(text).toBeInTheDocument();
    // Byte-exact — the frozen sentence carries nothing else.
    expect(text.textContent).toBe(PROVIDERS.CARD_NOT_ADMITTED);
    expect(text.textContent).not.toContain("https://");
    expect(text.textContent).not.toContain("ws-secret-1");
  });

  it("says nothing when the source IS admitted", () => {
    const ws = repoWorkspace({ sources: [{ type: "repo", source: "acme/payments", admitted: true }] });
    renderCard({ workspaces: [ws], state: { workspaces: [{ workspaceId: ws.id, enabledOptional: [] }] } });
    expect(screen.queryByText(PROVIDERS.CARD_NOT_ADMITTED)).toBeNull();
  });

  it("says nothing when no workspace is picked yet", () => {
    const ws = repoWorkspace();
    renderCard({ workspaces: [ws] });
    expect(screen.queryByText(PROVIDERS.CARD_NOT_ADMITTED)).toBeNull();
  });
});

// F2-F11: the Select had no id/name of its own before this — the field-list
// treats it like every other labeled control on the screen.
describe("WorkspaceCard — the Select's own identity", () => {
  it("carries id=nr-workspace and aria-label=Workspace", () => {
    renderCard();
    const trigger = screen.getByRole("combobox", { name: "Workspace" });
    expect(trigger).toHaveAttribute("id", "nr-workspace");
  });
});

// F2-F8: a B4b clone (or an earlier Add-workspace act) can leave MORE than one
// selection in state.workspaces[]. The primary (index 0) is what the Select
// shows and drives; the rest used to have no representation on screen at all
// and no way to survive a change to the primary.
describe("WorkspaceCard — extra selections from a multi-workspace clone", () => {
  const primary: Workspace = {
    id: "ws-primary",
    name: "payments",
    kind: "repo",
    source: "acme/payments",
    status: "scanned",
    created_at: "",
    updated_at: "",
  };
  const extra: Workspace = { ...primary, id: "ws-extra", name: "shared-libs" };

  it("renders every selection past index 0 as a removable chip", () => {
    renderCard({
      workspaces: [primary, extra],
      state: { workspaces: [{ workspaceId: primary.id, enabledOptional: [] }, { workspaceId: extra.id, enabledOptional: [] }] },
    });
    const chips = screen.getByTestId("nr-workspace-extras");
    expect(within(chips).getByText("shared-libs")).toBeInTheDocument();
    // The primary never gets a chip of its own — it's what the Select shows.
    expect(within(chips).queryByText("payments")).not.toBeInTheDocument();
  });

  it("a chip's Remove button drops only that selection", async () => {
    const { patch } = renderCard({
      workspaces: [primary, extra],
      state: { workspaces: [{ workspaceId: primary.id, enabledOptional: [] }, { workspaceId: extra.id, enabledOptional: [] }] },
    });
    await user.click(screen.getByRole("button", { name: "Remove shared-libs" }));
    expect(patch).toHaveBeenCalledWith({ workspaces: [{ workspaceId: primary.id, enabledOptional: [] }] });
  });

  it("changing the primary via the Select REPLACES index 0 only — the extra survives", async () => {
    const other: Workspace = { ...primary, id: "ws-other", name: "other-repo" };
    const { patch } = renderCard({
      workspaces: [primary, extra, other],
      state: { workspaces: [{ workspaceId: primary.id, enabledOptional: [] }, { workspaceId: extra.id, enabledOptional: [] }] },
    });
    await user.click(screen.getByRole("combobox", { name: "Workspace" }));
    await user.click(await screen.findByRole("option", { name: "other-repo" }));
    expect(patch).toHaveBeenCalledWith({
      workspaces: [
        { workspaceId: other.id, enabledOptional: [] },
        { workspaceId: extra.id, enabledOptional: [] },
      ],
    });
  });
});
