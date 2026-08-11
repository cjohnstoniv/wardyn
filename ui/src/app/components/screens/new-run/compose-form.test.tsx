/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import {
  ComposeForm,
  attachmentCapError,
  byteLength,
  MAX_ATTACHMENT_BYTES,
  MAX_TOTAL_INPUT_BYTES,
  MAX_ATTACHMENTS_COUNT,
} from "./compose-form";
import type { ComposeAttachment, ComposerBackend, Workspace } from "../../../lib/types";
import { baseStatus } from "../setup/test-fixtures";

// ComposeForm now renders ModelAccessCard (step-access.tsx) in place of the old
// subscription toggle — same self-fetch mocks its own tests use, so every
// render here settles without an unhandled-rejection/act() warning.
const listIntegrationsMock = vi.fn();
vi.mock("../../../lib/api/integrations", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../lib/api/integrations")>();
  return { ...actual, integrationsApi: { list: (...a: unknown[]) => listIntegrationsMock(...a) } };
});
const listWorkspacesMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: { listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a) },
}));
const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));
// §5: the context line's own unstored-secrets chip self-fetches stored secret
// names directly (same idiom as WorkspacePicker's own identical effect) — see
// workspace-picker.test.tsx for the pattern this mirrors.
const listSecretsMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: { listSecrets: (...a: unknown[]) => listSecretsMock(...a) },
}));

beforeEach(() => {
  listIntegrationsMock.mockReset().mockResolvedValue({ ai: [], scm: [], mirror: [], proxy: [] });
  listWorkspacesMock.mockReset().mockResolvedValue([]);
  getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
  listSecretsMock.mockReset().mockResolvedValue([]);
});

// The client-side attachment caps MUST match the server caps (internal/composer):
// 256 KiB per file, 1 MiB total across prompt + attachments, 32 attachments. The
// UI enforces them so it never sends a body the server would 413, and surfaces a
// clear inline error rather than silently dropping or truncating.
describe("attachmentCapError (pure cap logic)", () => {
  it("accepts a small file under both caps", () => {
    expect(attachmentCapError("a.txt", "hello", "prompt", [])).toBeNull();
  });

  it("rejects a single file over the 256 KiB per-file cap", () => {
    const big = "x".repeat(MAX_ATTACHMENT_BYTES + 1);
    const err = attachmentCapError("big.txt", big, "", []);
    expect(err).toMatch(/per-file limit/i);
  });

  it("accepts a file exactly at the per-file cap", () => {
    const exact = "x".repeat(MAX_ATTACHMENT_BYTES);
    expect(attachmentCapError("exact.txt", exact, "", [])).toBeNull();
  });

  it("rejects a file that pushes prompt + attachments over the 1 MiB total cap", () => {
    // Existing attachment near the total, new file tips it over.
    const existing: ComposeAttachment[] = [
      { name: "first.txt", content: "y".repeat(MAX_TOTAL_INPUT_BYTES - 1000) },
    ];
    const next = "z".repeat(2000);
    const err = attachmentCapError("second.txt", next, "", existing);
    expect(err).toMatch(/total input limit/i);
  });

  it("counts the prompt bytes toward the total cap", () => {
    const prompt = "p".repeat(MAX_TOTAL_INPUT_BYTES - 100);
    const file = "q".repeat(200);
    const err = attachmentCapError("f.txt", file, prompt, []);
    expect(err).toMatch(/total input limit/i);
  });

  it("rejects once the attachment count cap is reached", () => {
    const full: ComposeAttachment[] = Array.from({ length: MAX_ATTACHMENTS_COUNT }, (_, i) => ({
      name: `f${i}.txt`,
      content: "small",
    }));
    const err = attachmentCapError("one-more.txt", "small", "", full);
    expect(err).toMatch(new RegExp(`at most ${MAX_ATTACHMENTS_COUNT}`, "i"));
  });

  it("byteLength measures UTF-8 bytes (multi-byte chars count > char length)", () => {
    expect(byteLength("abc")).toBe(3);
    // A 4-byte emoji.
    expect(byteLength("😀")).toBe(4);
  });
});

describe("ComposeForm — file attach enforces the size cap", () => {
  const backends: ComposerBackend[] = [
    { name: "anthropic", provider: "anthropic", model: "claude", is_default: true },
  ];

  function renderForm(overrides: Partial<React.ComponentProps<typeof ComposeForm>> = {}) {
    const onAttachmentsChange = vi.fn();
    const props = {
      prompt: "do the thing",
      workspaceSelections: [],
      workspaces: [],
      workspacesLoading: false,
      onAddWorkspace: vi.fn(),
      attachments: [] as ComposeAttachment[],
      sources: [] as string[],
      backend: "anthropic",
      backends,
      mode: "auto" as const,
      interactive: false,
      composing: false,
      onPromptChange: vi.fn(),
      onWorkspaceSelectionsChange: vi.fn(),
      onAttachmentsChange,
      onSourcesChange: vi.fn(),
      onBackendChange: vi.fn(),
      onModeChange: vi.fn(),
      onInteractiveChange: vi.fn(),
      onCompose: vi.fn(),
      ...overrides,
    };
    render(<ComposeForm {...props} />);
    return { onAttachmentsChange };
  }

  it("accepts a small text file and adds it as an attachment", async () => {
    const { onAttachmentsChange } = renderForm();
    const user = userEvent.setup();
    const input = screen.getByLabelText(/attach files/i) as HTMLInputElement;
    const file = new File(["small contents"], "notes.txt", { type: "text/plain" });
    await user.upload(input, file);
    await waitFor(() => expect(onAttachmentsChange).toHaveBeenCalled());
    const added = onAttachmentsChange.mock.calls.at(-1)![0];
    expect(added).toEqual([{ name: "notes.txt", content: "small contents" }]);
  });

  it("rejects an oversize file with an inline error and does not add it", async () => {
    const { onAttachmentsChange } = renderForm();
    const user = userEvent.setup();
    const input = screen.getByLabelText(/attach files/i) as HTMLInputElement;
    const big = "x".repeat(MAX_ATTACHMENT_BYTES + 10);
    const file = new File([big], "huge.txt", { type: "text/plain" });
    await user.upload(input, file);
    // The cap error is surfaced inline...
    expect(await screen.findByText(/per-file limit/i)).toBeInTheDocument();
    // ...and the oversize file is never added (the change callback adds nothing).
    const lastCall = onAttachmentsChange.mock.calls.at(-1);
    if (lastCall) expect(lastCall[0]).toEqual([]);
  });

  it("disables Compose until there is a prompt", () => {
    renderForm({ prompt: "   " });
    expect(screen.getByRole("button", { name: /compose/i })).toBeDisabled();
  });

  it("enables Compose with a prompt and no workspace selected (empty = ephemeral)", () => {
    renderForm({ workspaceSelections: [] });
    expect(screen.getByRole("button", { name: /compose/i })).toBeEnabled();
  });

  it("offers the onboarded-workspace multi-select picker inside the context line (no free-text workspace fields)", () => {
    renderForm();
    expect(screen.getByText(/add a workspace/i)).toBeInTheDocument(); // WorkspaceCombobox trigger
    expect(screen.getByRole("button", { name: /add workspace/i })).toBeInTheDocument();
    expect(screen.queryByLabelText(/git repo/i)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/local directory/i)).not.toBeInTheDocument();
  });

  // #6: the single-backend read-only row is deleted outright — its name+model
  // now live in "More options"' own collapsed summary suffix instead.
  it("names the single backend in the More-options summary, with no dropdown and no legacy read-only row", () => {
    renderForm();
    // name + model both survive on the collapsed summary suffix.
    expect(screen.getByText(/analyzed by anthropic.*claude/)).toBeInTheDocument();
    expect(screen.queryByLabelText(/which integration analyzes your task/i)).not.toBeInTheDocument();
    expect(screen.queryByTestId("compose-backend-single")).not.toBeInTheDocument();
  });

  it("reveals the backend dropdown inside More options when there are 2+ backends", async () => {
    renderForm({
      backends: [
        { name: "anthropic", provider: "anthropic", model: "claude-opus-4-8", is_default: true },
        { name: "dev", provider: "fake", model: "demo", is_default: false },
      ],
    });
    expect(screen.queryByTestId("compose-backend-single")).not.toBeInTheDocument();
    const select = screen.getByLabelText("Which integration analyzes your task");
    // Collapsed by default — present in the DOM (native <details> semantics),
    // not yet visible.
    expect(select).not.toBeVisible();
    const user = userEvent.setup();
    await user.click(screen.getByText("More options"));
    expect(select).toBeVisible();
  });

  it("captures the Interactive vs Autonomous run mode UPFRONT in the initial panel", async () => {
    const onInteractiveChange = vi.fn();
    renderForm({ interactive: false, onInteractiveChange });
    // The selector is in the compose panel (not just the review), defaulting to
    // Autonomous — labels come verbatim from RUN_MODE (copy.ts D3 bans
    // Batch/Background as mode names).
    expect(screen.getByRole("radiogroup", { name: /run mode/i })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /Interactive/ })).toHaveAttribute("aria-checked", "false");
    expect(screen.getByRole("radio", { name: /Autonomous/ })).toHaveAttribute("aria-checked", "true");
    const user = userEvent.setup();
    await user.click(screen.getByRole("radio", { name: /Interactive/ }));
    expect(onInteractiveChange).toHaveBeenCalledWith(true);
  });

  it("run-mode radiogroup follows the APG keyboard pattern (roving tabindex + arrows)", async () => {
    const onInteractiveChange = vi.fn();
    renderForm({ interactive: false, onInteractiveChange });
    const autonomous = screen.getByRole("radio", { name: /Autonomous/ });
    const interactiveBtn = screen.getByRole("radio", { name: /Interactive/ });
    // Roving tabindex: the options are never separate tab stops — Tab reaches
    // the group once (it delegates focus to the checked option) and arrows move
    // within it.
    expect(screen.getByRole("radiogroup", { name: /run mode/i })).toHaveAttribute("tabindex", "0");
    expect(autonomous).toHaveAttribute("tabindex", "-1");
    expect(interactiveBtn).toHaveAttribute("tabindex", "-1");
    // Arrow from the checked option selects the sibling (either arrow works in
    // a two-option group). fireEvent, not userEvent: Radix defers the focus move
    // to a macrotask and only selects while an arrow is HELD, so userEvent's
    // immediate keyup clears that flag before the deferred focus lands.
    autonomous.focus();
    fireEvent.keyDown(autonomous, { key: "ArrowRight" });
    await waitFor(() => expect(onInteractiveChange).toHaveBeenCalledWith(true));
  });

  it("renders the model-access card in place of the removed subscription toggle", () => {
    renderForm();
    // The card's own static label — unambiguous even before its self-fetches
    // settle (the first-paint "Resolving model access…" line also matches a
    // loose /model access/i, see step-access.test.tsx's loading-gate coverage).
    expect(screen.getByText("Model access — resolved from integrations")).toBeInTheDocument();
  });
});

// #5: the workspace picker collapses to a context line whose <summary> is the
// answer, not a question — capability (multi-attach, optional opt-ins, target
// override) stays fully reachable one click away.
describe("ComposeForm — workspace context line (§5)", () => {
  const backends: ComposerBackend[] = [
    { name: "anthropic", provider: "anthropic", model: "claude", is_default: true },
  ];
  const workspace: Workspace = {
    id: "ws-1",
    name: "payments",
    kind: "local_dir",
    source: "/home/me/payments",
    status: "scanned",
    created_at: "now",
    updated_at: "now",
    // requirements isn't on the shared Workspace TS type yet (same stopgap
    // wizard-types.ts documents) — cast, matching how the picker itself reads it.
    requirements: { "secret:DATABASE_URL": { level: "required", provenance: "operator_set" } },
  } as Workspace;

  function renderForm(overrides: Partial<React.ComponentProps<typeof ComposeForm>> = {}) {
    const props = {
      prompt: "do the thing",
      workspaceSelections: [],
      workspaces: [],
      workspacesLoading: false,
      onAddWorkspace: vi.fn(),
      attachments: [] as ComposeAttachment[],
      sources: [] as string[],
      backend: "anthropic",
      backends,
      mode: "auto" as const,
      interactive: false,
      composing: false,
      onPromptChange: vi.fn(),
      onWorkspaceSelectionsChange: vi.fn(),
      onAttachmentsChange: vi.fn(),
      onSourcesChange: vi.fn(),
      onBackendChange: vi.fn(),
      onModeChange: vi.fn(),
      onInteractiveChange: vi.fn(),
      onCompose: vi.fn(),
      ...overrides,
    };
    return render(<ComposeForm {...props} />);
  }

  it("empty selection reads the honest empty-scratch summary, collapsed by default", () => {
    renderForm({ workspaceSelections: [], workspaces: [] });
    expect(
      screen.getByText("No workspace — an empty scratch directory inside the sandbox."),
    ).toBeInTheDocument();
    // The picker is present (native <details> keeps children mounted) but not visible.
    expect(screen.getByRole("button", { name: /add workspace/i })).not.toBeVisible();
  });

  it("a selection's collapsed summary shows name, status, composition/kind, and the unstored-secret attention chip", async () => {
    renderForm({ workspaceSelections: [{ workspaceId: "ws-1" }], workspaces: [workspace] });
    // Scoped to the <summary> itself: the (collapsed but still-mounted)
    // WorkspacePicker below renders its OWN "Usable" chip for the same
    // workspace, so an unscoped query is ambiguous.
    const summary = screen.getByText(/Workspace: payments/).closest("summary")!;
    expect(within(summary).getByText("Usable")).toBeInTheDocument(); // statusWord("scanned")
    expect(within(summary).getByText("local dir")).toBeInTheDocument(); // KIND_META fallback (single-source)
    // The one safety-relevant fact stays visible even collapsed (honesty, §6).
    expect(await within(summary).findByText(/1 secret isn't stored/)).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: /add a workspace/i })).not.toBeVisible();
  });

  it("no attention chip once the required secret is stored", async () => {
    listSecretsMock.mockResolvedValue(["DATABASE_URL"]);
    renderForm({ workspaceSelections: [{ workspaceId: "ws-1" }], workspaces: [workspace] });
    // Flush the self-fetch effect the same way workspace-picker.test.tsx does:
    // findBy* on any reliably-present element waits out the pending microtask
    // (the resolved listSecrets() promise) before the synchronous check below.
    await screen.findByText(/Workspace: payments/);
    expect(screen.queryByText(/isn't stored/)).not.toBeInTheDocument();
  });

  it("opening the details reveals the full WorkspacePicker, capability intact", async () => {
    renderForm({ workspaceSelections: [{ workspaceId: "ws-1" }], workspaces: [workspace] });
    const user = userEvent.setup();
    await user.click(screen.getByText(/Workspace: payments/));
    expect(screen.getByRole("combobox", { name: /add a workspace/i })).toBeVisible();
    expect(screen.getByRole("button", { name: /add workspace/i })).toBeVisible();
    // The trimmed hint moved into the body — only visible once opened.
    expect(
      screen.getByText(/The first workspace is the primary\. Attach another/),
    ).toBeVisible();
  });
});

// #6: one "More options" disclosure — clarify mode now has a real visible
// label, and the KiB budget sits beside the control it measures.
describe("ComposeForm — More options disclosure (§6)", () => {
  const backends: ComposerBackend[] = [
    { name: "anthropic", provider: "anthropic", model: "claude", is_default: true },
  ];

  function renderForm(overrides: Partial<React.ComponentProps<typeof ComposeForm>> = {}) {
    const props = {
      prompt: "do the thing",
      workspaceSelections: [],
      workspaces: [],
      workspacesLoading: false,
      onAddWorkspace: vi.fn(),
      attachments: [] as ComposeAttachment[],
      sources: [] as string[],
      backend: "anthropic",
      backends,
      mode: "auto" as const,
      interactive: false,
      composing: false,
      onPromptChange: vi.fn(),
      onWorkspaceSelectionsChange: vi.fn(),
      onAttachmentsChange: vi.fn(),
      onSourcesChange: vi.fn(),
      onBackendChange: vi.fn(),
      onModeChange: vi.fn(),
      onInteractiveChange: vi.fn(),
      onCompose: vi.fn(),
      ...overrides,
    };
    return render(<ComposeForm {...props} />);
  }

  it("the clarify-mode select has a real visible label, collapsed inside More options by default", async () => {
    renderForm();
    const select = screen.getByLabelText("Clarifying questions");
    expect(select).not.toBeVisible();
    const user = userEvent.setup();
    await user.click(screen.getByText("More options"));
    expect(select).toBeVisible();
    expect(select).toHaveTextContent(/auto/i);
  });

  it("attachments and source URLs live inside More options, not the top-level body", async () => {
    renderForm();
    const attachBtn = screen.getByRole("button", { name: /attach files/i });
    const sourceInput = screen.getByLabelText("Source URL");
    expect(attachBtn).not.toBeVisible();
    expect(sourceInput).not.toBeVisible();
    const user = userEvent.setup();
    await user.click(screen.getByText("More options"));
    expect(attachBtn).toBeVisible();
    expect(sourceInput).toBeVisible();
  });

  it("the KiB-used counter sits beside Attachments, not alone in the footer", async () => {
    renderForm();
    const user = userEvent.setup();
    await user.click(screen.getByText("More options"));
    const counter = screen.getByText(/KiB used/);
    expect(counter.closest("details")).not.toBeNull();
  });

  it("the footer keeps only Ask a question and Compose", () => {
    renderForm();
    const composeBtn = screen.getByRole("button", { name: /^compose$/i });
    // The footer row is Compose's own nearest flex container — walk up to it
    // and confirm the clarify select isn't one of its descendants any more.
    const footer = composeBtn.closest("div.justify-end");
    expect(footer).not.toBeNull();
    expect(footer!.querySelector("#compose-clarify-mode")).toBeNull();
  });
});
