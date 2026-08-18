/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The redesign's whole point is that the run's most consequential choice — what
// happens when it reaches a host you didn't list — is a decision with stated
// consequences, not a dropdown of dense phrases. These pin that it maps onto the
// REAL server enum, that the block list keeps its "wins over allow" meaning, and
// that Cancel cannot leak a draft into the run.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { NetworkDialog, HOST_GROUPS, UNLISTED_RULES, type NetworkSelection } from "./network-dialog";
import { PRESET_DOMAINS } from "./wizard-types";

const user = userEvent.setup({ pointerEventsCheck: 0 });

const base: NetworkSelection = {
  allowAllEgress: false,
  allowedDomains: ["github.com", "pypi.org"],
  deniedDomains: ["telemetry.example.com"],
  firstUseApproval: "wait_for_review",
};

let onSave: ReturnType<typeof vi.fn>;
let onOpenChange: ReturnType<typeof vi.fn>;

function open(value: NetworkSelection = base) {
  onSave = vi.fn();
  onOpenChange = vi.fn();
  return render(<NetworkDialog open value={value} onSave={onSave} onOpenChange={onOpenChange} />);
}

beforeEach(() => vi.clearAllMocks());

describe("NetworkDialog — the unlisted-host rule", () => {
  // The three cards ARE types.FirstUseMode, one per value. If someone adds a
  // fourth server mode, this fails rather than the UI silently offering three.
  it("offers exactly the three real FirstUseMode values", () => {
    open();
    expect(UNLISTED_RULES.map((r) => r.id)).toEqual([
      "wait_for_review",
      "deny_with_review",
      "always_deny",
    ]);
    expect(screen.getAllByRole("radio", { name: /Hold it for approval|Deny, but ask|Deny silently/ })).toHaveLength(3);
  });

  // Each card states its CONSEQUENCE. wait_for_review genuinely holds the
  // connection open; deny_with_review refuses now and passes on retry; the
  // copy must not blur the two, because that difference is the whole feature.
  it("each card states what actually happens, not the mode name", () => {
    open();
    expect(screen.getByText(/waits, live, until you approve/)).toBeInTheDocument();
    expect(screen.getByText(/Refused right away and raised for review/)).toBeInTheDocument();
    expect(screen.getByText(/no prompt, no wait/)).toBeInTheDocument();
  });

  it("saves the picked mode", async () => {
    open();
    await user.click(screen.getByRole("radio", { name: /Deny silently/ }));
    await user.click(screen.getByRole("button", { name: /save hosts/i }));
    expect(onSave).toHaveBeenCalledWith(expect.objectContaining({ firstUseApproval: "always_deny" }));
  });
});

describe("NetworkDialog — hosts", () => {
  // A host added to PRESET_DOMAINS but not to a group would silently disappear
  // from this dialog — reachable by the wizard, invisible here.
  it("the groups cover exactly PRESET_DOMAINS", () => {
    const grouped = HOST_GROUPS.flatMap((g) => g.hosts);
    expect([...grouped].sort()).toEqual([...PRESET_DOMAINS].sort());
  });

  it("toggling a chip adds and removes it", async () => {
    open();
    await user.click(screen.getByRole("button", { name: /crates\.io/ }));
    await user.click(screen.getByRole("button", { name: /^✓?github\.com$/ }));
    await user.click(screen.getByRole("button", { name: /save hosts/i }));
    const saved = onSave.mock.calls[0][0] as NetworkSelection;
    expect(saved.allowedDomains).toContain("crates.io");
    expect(saved.allowedDomains).not.toContain("github.com");
  });

  it("a custom host lands in its own 'Yours' group, not lost among the presets", async () => {
    open();
    await user.type(screen.getByLabelText("Add a host"), "api.internal.acme.com");
    await user.click(screen.getAllByRole("button", { name: /^add$/i })[0]);
    expect(screen.getByText("Yours")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /api\.internal\.acme\.com/ })).toBeInTheDocument();
  });
});

describe("NetworkDialog — Open mode", () => {
  it("states what Open does NOT stop — the old toggle never did", async () => {
    open();
    await user.click(screen.getByRole("radio", { name: /Open — any public host/ }));
    expect(screen.getByText(/Private and internal ranges, and the cloud metadata endpoint, stay/)).toBeInTheDocument();
    expect(screen.getByText(/doesn't stop a run from reaching a brand-new public host/i)).toBeInTheDocument();
  });

  // Allowlist-only controls are meaningless under allow-all and are hidden, but
  // the BLOCK list still applies — denied_domains wins in both modes server-side.
  it("hides the allowlist and the rule, keeps the block list", async () => {
    open();
    await user.click(screen.getByRole("radio", { name: /Open — any public host/ }));
    expect(screen.queryByRole("radiogroup", { name: "Unlisted host rule" })).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Add a host")).not.toBeInTheDocument();
    expect(screen.getByText(/Never allow these/)).toBeInTheDocument();
  });
});

describe("NetworkDialog — the block list", () => {
  it("says it wins over an allow rule, and removes on click", async () => {
    open();
    expect(screen.getByText(/Blocked even if an allow rule above would otherwise match/)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Remove telemetry.example.com" }));
    await user.click(screen.getByRole("button", { name: /save hosts/i }));
    expect(onSave).toHaveBeenCalledWith(expect.objectContaining({ deniedDomains: [] }));
  });
});

describe("NetworkDialog — it is a transaction", () => {
  // Cancel must leave the run's real selection untouched. The dialog holds a
  // draft precisely so a half-made edit cannot reach the launch payload.
  it("Cancel saves nothing", async () => {
    open();
    await user.click(screen.getByRole("radio", { name: /Deny silently/ }));
    await user.click(screen.getByRole("button", { name: /crates\.io/ }));
    await user.click(screen.getByRole("button", { name: /^cancel$/i }));
    expect(onSave).not.toHaveBeenCalled();
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("the summary counts hosts and names the rule in the same words as the card", async () => {
    open();
    expect(screen.getByText(/2 hosts allowed · everything else: hold it for approval/i)).toBeInTheDocument();
    await user.click(screen.getByRole("radio", { name: /Open — any public host/ }));
    expect(screen.getByText(/Any public host allowed · private and internal ranges blocked/i)).toBeInTheDocument();
  });
});

describe("NetworkDialog — the draft survives parent re-renders", () => {
  // The 2026-08-18 take-killer: new-run-screen rebuilds `value` as a fresh
  // object literal on every render, and its polls (health, workspaces, titles)
  // render constantly — so a re-seed effect keyed on `value` wiped the draft
  // between the operator's click and Save. "Hold it for approval" was picked
  // on camera and deny_with_review launched. The dialog's draft belongs to the
  // dialog while it is open; only OPENING may re-seed it.
  it("keeps a mid-edit rule pick when the parent re-renders with a fresh value object", async () => {
    const start: NetworkSelection = { ...base, firstUseApproval: "deny_with_review" };
    const view = render(
      <NetworkDialog open value={start} onSave={(onSave = vi.fn())} onOpenChange={(onOpenChange = vi.fn())} />,
    );

    await user.click(screen.getByRole("radio", { name: /^Hold it for approval/ }));

    // The parent polls and re-renders: same CONTENT, new object identity —
    // exactly what new-run-screen hands down every render.
    view.rerender(
      <NetworkDialog open value={{ ...start }} onSave={onSave} onOpenChange={onOpenChange} />,
    );

    await user.click(screen.getByRole("button", { name: "Save hosts" }));
    expect(onSave).toHaveBeenCalledWith(
      expect.objectContaining({ firstUseApproval: "wait_for_review" }),
    );
  });
});
