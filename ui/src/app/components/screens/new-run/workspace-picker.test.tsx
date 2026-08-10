/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// WorkspacePicker renders straight off a workspace's requirements contract
// (Workspace.requirements): a "Comes with:" summary of the Required set
// (read-only, disclosure expands the list), an "Available if you need it"
// block of checkboxes over the Optional set that write into the selection's
// enabledOptional, an attention line for a Required secret that isn't stored,
// and write-mode rendered as a chip (Required) or a toggle (Optional) — never
// both. secrets are self-fetched, so every assertion that depends on them
// waits for the effect to settle (findBy*, not getBy*).
import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { WorkspacePicker } from "./workspace-picker";
import type { RunWorkspaceSelection } from "./wizard-types";
import type { Workspace } from "../../../lib/types";

const listSecretsMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: { listSecrets: (...a: unknown[]) => listSecretsMock(...a) },
}));

// Workspace.requirements isn't on the shared Workspace TS type yet (same
// stopgap wizard-types.ts documents) — the cast mirrors how the component
// itself reads it.
function workspace(overrides: Record<string, unknown> = {}): Workspace {
  return {
    id: "ws-1",
    name: "payments-service",
    kind: "local_dir",
    source: "/home/me/payments",
    status: "scanned",
    created_at: "",
    updated_at: "",
    ...overrides,
  } as Workspace;
}

function renderPicker(
  selections: RunWorkspaceSelection[],
  workspaces: Workspace[],
  optionalRequirementsEnabled?: boolean,
) {
  const onChange = vi.fn();
  render(
    <WorkspacePicker
      selections={selections}
      onChange={onChange}
      workspaces={workspaces}
      loading={false}
      onAddWorkspace={() => {}}
      optionalRequirementsEnabled={optionalRequirementsEnabled}
    />,
  );
  return { onChange };
}

describe("WorkspacePicker — Comes with / disclosure", () => {
  it("summarizes the required set on the collapsed line", async () => {
    listSecretsMock.mockResolvedValue([]);
    const ws = workspace({
      requirements: {
        "secret:DATABASE_URL": { level: "required", provenance: "scan_seeded" },
        "egress:api.stripe.com": { level: "required", provenance: "scan_seeded" },
      },
    });
    renderPicker([{ workspaceId: "ws-1" }], [ws]);
    expect(await screen.findByText("1 secret · 1 host")).toBeInTheDocument();
  });

  it("falls back to the honest 'nothing beyond the auto-allowed set' line", async () => {
    listSecretsMock.mockResolvedValue([]);
    renderPicker([{ workspaceId: "ws-1" }], [workspace({ requirements: {} })]);
    expect(await screen.findByText(/nothing beyond the auto-allowed set/)).toBeInTheDocument();
  });

  // Item 4 (found by live driving): enabling an Optional row never moved the
  // "Comes with:" line — the one summary whose whole job is "what does this
  // run actually get" silently ignored the operator's own edit. Proves the
  // CALL SITE threads `sel` through (wizard-types.test.ts covers
  // comesWithLine's pure logic; this proves the picker actually wires it).
  it("the Comes-with line reflects an already-enabled Optional host", async () => {
    listSecretsMock.mockResolvedValue([]);
    const ws = workspace({
      requirements: {
        "secret:DATABASE_URL": { level: "required", provenance: "scan_seeded" },
        "egress:telemetry.example.com": { level: "optional", provenance: "operator_set" },
      },
    });
    renderPicker(
      [{ workspaceId: "ws-1", enabledOptional: ["egress:telemetry.example.com"] }],
      [ws],
    );
    expect(await screen.findByText("1 secret · 1 opted in")).toBeInTheDocument();
  });

  it("required secrets/hosts are read-only text, not a checkbox, until disclosed", async () => {
    listSecretsMock.mockResolvedValue(["DATABASE_URL"]);
    const ws = workspace({
      requirements: { "secret:DATABASE_URL": { level: "required", provenance: "operator_set" } },
    });
    renderPicker([{ workspaceId: "ws-1" }], [ws]);
    await screen.findByText(/Comes with:/);
    expect(screen.queryByText("DATABASE_URL")).toBeNull(); // collapsed
    await userEvent.click(screen.getByRole("button", { name: "show" }));
    expect(screen.getByText("DATABASE_URL")).toBeInTheDocument();
    expect(screen.queryByRole("checkbox")).toBeNull(); // never a per-run toggle
  });

  // Honesty (Stage 4): applyWorkspaceRequirements' TRUST BOUNDARY auto-grants a
  // secret ONLY from an operator_set requirement row — a scan_seeded row NEVER
  // does, Required or not. The disclosed list must not read as "you get this
  // automatically" for one of those.
  it("flags a scan_seeded Required secret as not auto-granting", async () => {
    listSecretsMock.mockResolvedValue(["DATABASE_URL"]);
    const ws = workspace({
      requirements: { "secret:DATABASE_URL": { level: "required", provenance: "scan_seeded" } },
    });
    renderPicker([{ workspaceId: "ws-1" }], [ws]);
    await screen.findByText(/Comes with:/);
    await userEvent.click(screen.getByRole("button", { name: "show" }));
    expect(screen.getByText("DATABASE_URL")).toBeInTheDocument();
    expect(screen.getByText(/won't auto-grant/i)).toBeInTheDocument();
  });

  it("does not flag an operator_set Required secret — it genuinely auto-grants", async () => {
    listSecretsMock.mockResolvedValue(["DATABASE_URL"]);
    const ws = workspace({
      requirements: { "secret:DATABASE_URL": { level: "required", provenance: "operator_set" } },
    });
    renderPicker([{ workspaceId: "ws-1" }], [ws]);
    await screen.findByText(/Comes with:/);
    await userEvent.click(screen.getByRole("button", { name: "show" }));
    expect(screen.getByText("DATABASE_URL")).toBeInTheDocument();
    expect(screen.queryByText(/won't auto-grant/i)).toBeNull();
  });
});

// Item 2 (HIGH — honesty): the SAME trust-boundary caveat above ("Required
// secrets are read-only text... disclosed") was applied to exactly ONE of the
// two places a scan_seeded secret's requirement row renders — the "Available
// if you need it" OPTIONAL checkboxes (always visible, no "show" needed)
// carried none, so ticking one read as a real opt-in that
// runs_create.go's applyWorkspaceRequirements silently skips (only an
// operator_set row ever auto-mints a grant, Required or Optional — no level
// exemption).
describe("WorkspacePicker — optional-secret checkboxes carry the SAME auto-grant caveat (Item 2)", () => {
  it("flags a scan_seeded Optional secret as not auto-granting", async () => {
    listSecretsMock.mockResolvedValue([]);
    const ws = workspace({
      requirements: { "secret:REDIS_URL": { level: "optional", provenance: "scan_seeded" } },
    });
    renderPicker([{ workspaceId: "ws-1" }], [ws]);
    expect(await screen.findByRole("checkbox", { name: /REDIS_URL/i })).toBeInTheDocument();
    expect(screen.getByText(/won't auto-grant/i)).toBeInTheDocument();
  });

  it("does not flag an operator_set Optional secret — ticking it genuinely auto-grants", async () => {
    listSecretsMock.mockResolvedValue([]);
    const ws = workspace({
      requirements: { "secret:STRIPE_KEY": { level: "optional", provenance: "operator_set" } },
    });
    renderPicker([{ workspaceId: "ws-1" }], [ws]);
    await screen.findByRole("checkbox", { name: /STRIPE_KEY/i });
    expect(screen.queryByText(/won't auto-grant/i)).toBeNull();
  });

  it("an optional HOST checkbox never carries the secret caveat (egress has no provenance gate)", async () => {
    listSecretsMock.mockResolvedValue([]);
    const ws = workspace({
      requirements: { "egress:api.stripe.com": { level: "optional", provenance: "scan_seeded" } },
    });
    renderPicker([{ workspaceId: "ws-1" }], [ws]);
    await screen.findByRole("checkbox", { name: /api\.stripe\.com/i });
    expect(screen.queryByText(/won't auto-grant/i)).toBeNull();
  });
});

describe("WorkspacePicker — unstored-required-secret attention line", () => {
  it("shows the attention line + Add secret/Open when a required secret isn't stored", async () => {
    listSecretsMock.mockResolvedValue([]); // nothing stored
    const ws = workspace({
      requirements: { "secret:STRIPE_KEY": { level: "required", provenance: "operator_set" } },
    });
    renderPicker([{ workspaceId: "ws-1" }], [ws]);
    expect(await screen.findByText(/1 secret it requires isn't stored/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /add secret/i })).toHaveAttribute("href", "/secrets");
    expect(screen.getByRole("link", { name: /^open/i })).toHaveAttribute("href", "/workspaces");
  });

  it("stays quiet once the required secret is stored", async () => {
    listSecretsMock.mockResolvedValue(["STRIPE_KEY"]);
    const ws = workspace({
      requirements: { "secret:STRIPE_KEY": { level: "required", provenance: "operator_set" } },
    });
    renderPicker([{ workspaceId: "ws-1" }], [ws]);
    await screen.findByText(/Comes with:/);
    expect(screen.queryByText(/isn't stored/)).toBeNull();
  });
});

describe("WorkspacePicker — optional requirements write into enabledOptional", () => {
  it("an optional host renders an unchecked checkbox that adds its key on check", async () => {
    listSecretsMock.mockResolvedValue([]);
    const ws = workspace({
      requirements: { "egress:api.stripe.com": { level: "optional", provenance: "scan_seeded" } },
    });
    const { onChange } = renderPicker([{ workspaceId: "ws-1" }], [ws]);
    const box = await screen.findByRole("checkbox", { name: /api\.stripe\.com/i });
    expect(box).not.toBeChecked();
    await userEvent.click(box);
    expect(onChange).toHaveBeenCalledWith([
      { workspaceId: "ws-1", enabledOptional: ["egress:api.stripe.com"] },
    ]);
  });

  it("unchecking an already-enabled optional secret removes its key", async () => {
    listSecretsMock.mockResolvedValue([]);
    const ws = workspace({
      requirements: { "secret:REDIS_URL": { level: "optional", provenance: "scan_seeded" } },
    });
    const { onChange } = renderPicker(
      [{ workspaceId: "ws-1", enabledOptional: ["secret:REDIS_URL"] }],
      [ws],
    );
    const box = await screen.findByRole("checkbox", { name: /REDIS_URL/i });
    expect(box).toBeChecked();
    await userEvent.click(box);
    expect(onChange).toHaveBeenCalledWith([{ workspaceId: "ws-1", enabledOptional: [] }]);
  });

  it("Required items never appear in the optional checkbox block", async () => {
    listSecretsMock.mockResolvedValue([]);
    const ws = workspace({
      requirements: {
        "secret:DATABASE_URL": { level: "required", provenance: "operator_set" },
        "egress:optional-host.example.com": { level: "optional", provenance: "scan_seeded" },
      },
    });
    renderPicker([{ workspaceId: "ws-1" }], [ws]);
    await screen.findByRole("checkbox", { name: /optional-host/i });
    expect(screen.queryByRole("checkbox", { name: /DATABASE_URL/i })).toBeNull();
  });

  it("hides the optional block entirely when optionalRequirementsEnabled=false (the Composer path)", async () => {
    listSecretsMock.mockResolvedValue([]);
    const ws = workspace({
      requirements: { "egress:api.stripe.com": { level: "optional", provenance: "scan_seeded" } },
    });
    renderPicker([{ workspaceId: "ws-1" }], [ws], false);
    await screen.findByText(/Comes with:/);
    expect(screen.queryByText(/Available if you need it/i)).toBeNull();
    expect(screen.queryByRole("checkbox")).toBeNull();
  });
});

// A busy optional set (e.g. a workspace scanned before the junk-secrets filter
// existed) must never wall the card in checkboxes — capped preview + reveal.
describe("WorkspacePicker — optional grid cap", () => {
  it("caps the optional grid at 6 with a Show all N control that reveals the rest", async () => {
    listSecretsMock.mockResolvedValue([]);
    const requirements: Record<string, { level: string; provenance: string }> = {};
    for (let i = 0; i < 10; i++) {
      requirements[`secret:OPT_${i}`] = { level: "optional", provenance: "operator_set" };
    }
    const ws = workspace({ requirements });
    renderPicker([{ workspaceId: "ws-1" }], [ws]);
    await screen.findByText(/Available if you need it/i);
    expect(screen.getAllByRole("checkbox")).toHaveLength(6);
    await userEvent.click(screen.getByRole("button", { name: "Show all 10" }));
    expect(screen.getAllByRole("checkbox")).toHaveLength(10);
  });
});

describe("WorkspacePicker — write mode: Required chip vs Optional toggle", () => {
  it("a Required write renders a summary chip, never a toggle", async () => {
    listSecretsMock.mockResolvedValue([]);
    const ws = workspace({
      requirements: { "write:/home/me/payments": { level: "required", provenance: "operator_set" } },
    });
    renderPicker([{ workspaceId: "ws-1" }], [ws]);
    expect(await screen.findByText("writable — every run")).toBeInTheDocument();
    expect(screen.queryByRole("switch")).toBeNull();
  });

  it("an Optional write renders a toggle defaulting off; turning it on grants write and clears any narrowing", async () => {
    listSecretsMock.mockResolvedValue([]);
    const ws = workspace({
      requirements: { "write:/home/me/payments": { level: "optional", provenance: "operator_set" } },
    });
    const { onChange } = renderPicker([{ workspaceId: "ws-1", readOnly: true }], [ws]);
    const toggle = await screen.findByRole("switch");
    expect(toggle).not.toBeChecked();
    await userEvent.click(toggle);
    expect(onChange).toHaveBeenCalledWith([
      {
        workspaceId: "ws-1",
        readOnly: false,
        enabledOptional: ["write:/home/me/payments"],
      },
    ]);
  });

  it("shows neither a chip nor a toggle when no write requirement is declared", async () => {
    listSecretsMock.mockResolvedValue([]);
    renderPicker([{ workspaceId: "ws-1" }], [workspace({ requirements: {} })]);
    await screen.findByText(/Comes with:/);
    expect(screen.queryByText("writable — every run")).toBeNull();
    expect(screen.queryByRole("switch")).toBeNull();
  });
});

describe("WorkspacePicker — repos never get a read-only toggle", () => {
  it("a repo shows the REPO_RO rationale instead of a toggle", async () => {
    listSecretsMock.mockResolvedValue([]);
    const ws = workspace({ kind: "repo", source: "acme/payments-service" });
    renderPicker([{ workspaceId: "ws-1" }], [ws]);
    await screen.findByText(/Comes with:/);
    expect(screen.queryByRole("switch")).toBeNull();
    expect(
      screen.getByText(/nothing on your machine is touched, so there's nothing to protect/i),
    ).toBeInTheDocument();
  });
});

describe("WorkspacePicker — composition + empty states", () => {
  it("shows a composition summary for a multi-source workspace instead of a single kind", async () => {
    listSecretsMock.mockResolvedValue([]);
    const ws = workspace({
      sources: [
        { type: "local_dir", path: "/a" },
        { type: "local_dir", path: "/b" },
        { type: "repo", source: "acme/c" },
      ],
    });
    renderPicker([{ workspaceId: "ws-1" }], [ws]);
    expect(await screen.findByText("2 dirs · 1 repo")).toBeInTheDocument();
  });

  it("offers real CTAs — Attach a workspace and Add workspace — when nothing is selected", async () => {
    listSecretsMock.mockResolvedValue([]);
    renderPicker([], []);
    expect(screen.getByRole("button", { name: /attach a workspace/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /add workspace/i })).toBeInTheDocument();
    expect(screen.queryByText(/^no onboarded workspaces available\.$/i)).toBeNull();
    // Let the self-fetch settle so it doesn't warn on an unwrapped act() later.
    await waitFor(() => expect(listSecretsMock).toHaveBeenCalled());
  });

  it("the combobox's own empty state offers a real Add-a-workspace CTA, not bare text", async () => {
    const onAddWorkspace = vi.fn();
    render(
      <WorkspacePicker
        selections={[]}
        onChange={() => {}}
        workspaces={[]}
        loading={false}
        onAddWorkspace={onAddWorkspace}
      />,
    );
    await userEvent.click(screen.getByRole("combobox", { name: /add a workspace/i }));
    expect(await screen.findByText(/runs can only attach what exists here/i)).toBeInTheDocument();
    // Disambiguate from the persistent toolbar's "Add workspace" button (no "a").
    await userEvent.click(screen.getByRole("button", { name: /add a workspace/i }));
    expect(onAddWorkspace).toHaveBeenCalled();
  });
});
