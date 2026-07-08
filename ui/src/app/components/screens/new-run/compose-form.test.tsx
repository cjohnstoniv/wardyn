/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import {
  ComposeForm,
  attachmentCapError,
  byteLength,
  MAX_ATTACHMENT_BYTES,
  MAX_TOTAL_INPUT_BYTES,
  MAX_ATTACHMENTS_COUNT,
} from "./compose-form";
import type { ComposeAttachment, ComposerBackend } from "../../../lib/types";

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
      useSubscription: false,
      composing: false,
      onPromptChange: vi.fn(),
      onWorkspaceSelectionsChange: vi.fn(),
      onAttachmentsChange,
      onSourcesChange: vi.fn(),
      onBackendChange: vi.fn(),
      onModeChange: vi.fn(),
      onInteractiveChange: vi.fn(),
      onUseSubscriptionChange: vi.fn(),
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

  it("offers the onboarded-workspace multi-select picker (no free-text workspace fields)", () => {
    renderForm();
    expect(screen.getByText("Workspaces")).toBeInTheDocument();
    expect(screen.getByText(/add a workspace/i)).toBeInTheDocument(); // WorkspaceCombobox trigger
    expect(screen.getByRole("button", { name: /add workspace/i })).toBeInTheDocument();
    expect(screen.queryByLabelText(/git repo/i)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/local directory/i)).not.toBeInTheDocument();
  });

  it("shows the single backend's provider/model read-only (no dropdown) with one backend", () => {
    renderForm();
    expect(screen.getByText("Provider")).toBeInTheDocument();
    const single = screen.getByTestId("compose-backend-single");
    expect(single).toHaveTextContent("anthropic"); // name + provider
    expect(single).toHaveTextContent("claude"); // model
  });

  it("renders the dropdown (not the read-only display) when there are 2+ backends", () => {
    renderForm({
      backends: [
        { name: "anthropic", provider: "anthropic", model: "claude-opus-4-8", is_default: true },
        { name: "dev", provider: "fake", model: "demo", is_default: false },
      ],
    });
    expect(screen.queryByTestId("compose-backend-single")).not.toBeInTheDocument();
    expect(screen.getByText("Provider")).toBeInTheDocument();
  });

  it("captures the Interactive vs Background run mode UPFRONT in the initial panel", async () => {
    const onInteractiveChange = vi.fn();
    renderForm({ interactive: false, onInteractiveChange });
    // The selector is in the compose panel (not just the review), defaulting to Background.
    expect(screen.getByRole("radiogroup", { name: /run mode/i })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /Interactive/ })).toHaveAttribute("aria-checked", "false");
    expect(screen.getByRole("radio", { name: /Background/ })).toHaveAttribute("aria-checked", "true");
    const user = userEvent.setup();
    await user.click(screen.getByRole("radio", { name: /Interactive/ }));
    expect(onInteractiveChange).toHaveBeenCalledWith(true);
  });

  it("offers the per-run Claude-subscription opt-in, OFF by default, and reports toggling", async () => {
    const onUseSubscriptionChange = vi.fn();
    renderForm({ onUseSubscriptionChange });
    // Off by default: the api-key path is the governed default; subscription is
    // an explicit per-run choice (mirrors the server's consent model).
    const toggle = screen.getByRole("switch", { name: /use my claude subscription/i });
    expect(toggle).toHaveAttribute("aria-checked", "false");
    // The copy must be honest about residency: the credential lives in the
    // sandbox for the run (vs the proxy-side api key).
    expect(screen.getByText(/resident in the sandbox/i)).toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(toggle);
    expect(onUseSubscriptionChange).toHaveBeenCalledWith(true);
  });

  it("renders the subscription toggle ON when opted in", () => {
    renderForm({ useSubscription: true });
    expect(
      screen.getByRole("switch", { name: /use my claude subscription/i }),
    ).toHaveAttribute("aria-checked", "true");
  });
});
