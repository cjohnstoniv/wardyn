/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one door (#544, packet MP-E §5.9): each shape it draws, one door at a
// time, and B9 (#540) — the strip's line for a relaunch refused after its
// screen was gone.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { toast } from "sonner";

// The pane is a terminal screen with its own suites; faked to the props the
// door hands it and its two ways out.
vi.mock("../screens/settings/harness-login-pane", () => ({
  HarnessLoginPane: (p: {
    provider: string;
    modelProvider?: string;
    startURLManaged?: boolean;
    onDone: () => void;
    onCancel: () => void;
  }) => (
    <div
      data-testid="fake-pane"
      data-provider={p.provider}
      data-model-provider={p.modelProvider ?? ""}
      data-managed={String(!!p.startURLManaged)}
    >
      <button type="button" onClick={p.onDone}>
        fake done
      </button>
      <button type="button" onClick={p.onCancel}>
        fake cancel
      </button>
    </div>
  ),
}));
const putCredential = vi.fn();
const deleteCredential = vi.fn();
vi.mock("../../lib/api/model-provider-credentials", () => ({
  modelProviderCredentials: {
    putCredential: (...a: unknown[]) => putCredential(...a),
    deleteCredential: (...a: unknown[]) => deleteCredential(...a),
  },
}));

import { useModelAccessDoor, type OpenDoorOptions } from "./model-access-context";
import { MODEL_ACCESS_BANNER } from "./model-access-copy";
import { CLAUDE_DOOR, KEY_DOOR } from "./copy/door";
import { HttpError } from "../../lib/api/core";
import { MODEL_PROVIDERS, baseStatus, providerStatus } from "../../lib/test-fixtures";
import type { SetupStatus } from "../../lib/types";
import { WithDoor } from "../../../test/door-harness";

const { bedrock, claude, gateway, anthropicKey } = MODEL_PROVIDERS;

/** An entrance: a button that opens the door with `options`. */
function Entrance({ label, options }: { label: string; options?: OpenDoorOptions }) {
  const door = useModelAccessDoor();
  return (
    <button type="button" onClick={() => door.openDoor(options)}>
      {label}
    </button>
  );
}

function renderDoor(status: SetupStatus, options?: OpenDoorOptions, onRefresh = vi.fn()) {
  render(
    <WithDoor status={status} onRefresh={onRefresh}>
      <Entrance label="entrance" options={options} />
    </WithDoor>,
  );
  return onRefresh;
}

let success: ReturnType<typeof vi.spyOn>;
beforeEach(() => {
  success = vi.spyOn(toast, "success").mockImplementation(() => "id");
  putCredential.mockReset().mockResolvedValue(undefined);
  deleteCredential.mockReset().mockResolvedValue(undefined);
});
afterEach(() => {
  vi.restoreAllMocks();
  window.history.pushState({}, "", "/");
});

describe("the AWS door for a provider (case a)", () => {
  it("draws packet E's frame and hands the pane the provider it is for", async () => {
    renderDoor(providerStatus([{ provider: bedrock, defaultFor: ["claude-code"] }]), { for: { provider: bedrock.id } });
    await userEvent.click(screen.getByRole("button", { name: "entrance" }));
    const dialog = await screen.findByRole("dialog", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE });
    expect(dialog).toHaveTextContent("For Bedrock (prod)");
    expect(dialog).toHaveTextContent(MODEL_ACCESS_BANNER.DIALOG_CLEANUP_NOTE);
    const pane = await screen.findByTestId("fake-pane");
    expect(pane).toHaveAttribute("data-provider", "aws");
    expect(pane).toHaveAttribute("data-model-provider", "bedrock-prod");
    expect(pane).toHaveAttribute("data-managed", "true");
  });

  it("signed in: the door closes, the shell re-reads, and the toast says so", async () => {
    const onRefresh = renderDoor(providerStatus([{ provider: bedrock }]), { for: { provider: bedrock.id } });
    await userEvent.click(screen.getByRole("button", { name: "entrance" }));
    await userEvent.click(await screen.findByRole("button", { name: "fake done" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(success).toHaveBeenCalledWith(MODEL_ACCESS_BANNER.SIGNED_IN_TOAST);
    expect(onRefresh).toHaveBeenCalled();
  });

  it("cancelled: the door closes and nothing is claimed", async () => {
    renderDoor(providerStatus([{ provider: bedrock }]), { for: { provider: bedrock.id } });
    await userEvent.click(screen.getByRole("button", { name: "entrance" }));
    await userEvent.click(await screen.findByRole("button", { name: "fake cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(success).not.toHaveBeenCalled();
  });
});

describe("the Claude door for a provider (case b)", () => {
  it("draws its title, sub-line, description and note, and its own toast", async () => {
    renderDoor(providerStatus([{ provider: claude }]), { for: { provider: claude.id } });
    await userEvent.click(screen.getByRole("button", { name: "entrance" }));
    const dialog = await screen.findByRole("dialog", { name: CLAUDE_DOOR.TITLE });
    expect(dialog).toHaveTextContent("For Claude subscription");
    expect(dialog).toHaveTextContent(CLAUDE_DOOR.DESCRIPTION);
    expect(dialog).toHaveTextContent(MODEL_ACCESS_BANNER.DIALOG_CLEANUP_NOTE);
    expect(await screen.findByTestId("fake-pane")).toHaveAttribute("data-model-provider", "claude-sub");
    await userEvent.click(screen.getByRole("button", { name: "fake done" }));
    expect(success).toHaveBeenCalledWith(CLAUDE_DOOR.SIGNED_IN_TOAST);
  });
});

describe("the key and token door (case c)", () => {
  it("adding a token: the field, where it goes (D7), how it is kept, and Save", async () => {
    renderDoor(providerStatus([{ provider: gateway }]), { for: { provider: gateway.id } });
    await userEvent.click(screen.getByRole("button", { name: "entrance" }));
    const dialog = await screen.findByRole("dialog", { name: "Add your token for Corp gateway" });
    expect(dialog).toHaveTextContent("Sent to gateway.corp.example");
    expect(dialog).toHaveTextContent(KEY_DOOR.NOTE);
    expect(screen.queryByRole("button", { name: KEY_DOOR.REMOVE })).toBeNull();
    const save = screen.getByRole("button", { name: KEY_DOOR.SAVE });
    expect(save).toBeDisabled();
    await userEvent.type(screen.getByLabelText("Token"), "gw-secret-123");
    await userEvent.click(save);
    expect(putCredential).toHaveBeenCalledWith("corp-gateway", "gw-secret-123");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(success).toHaveBeenCalledWith("Token saved");
  });

  it("a key provider reads 'key' throughout", async () => {
    renderDoor(providerStatus([{ provider: anthropicKey }]), { for: { provider: anthropicKey.id } });
    await userEvent.click(screen.getByRole("button", { name: "entrance" }));
    const dialog = await screen.findByRole("dialog", { name: "Add your key for Anthropic API key" });
    expect(dialog).toHaveTextContent("Sent to api.anthropic.com");
    await userEvent.type(screen.getByLabelText("Key"), "sk-ant-api03-x");
    await userEvent.click(screen.getByRole("button", { name: KEY_DOOR.SAVE }));
    await waitFor(() => expect(success).toHaveBeenCalledWith("Key saved"));
  });

  it("refused: the server's sentence verbatim, the door stays open", async () => {
    putCredential.mockRejectedValue(new HttpError(400, "the token is too short"));
    renderDoor(providerStatus([{ provider: gateway }]), { for: { provider: gateway.id } });
    await userEvent.click(screen.getByRole("button", { name: "entrance" }));
    await userEvent.type(await screen.findByLabelText("Token"), "abc12");
    await userEvent.click(screen.getByRole("button", { name: KEY_DOOR.SAVE }));
    expect(await screen.findByRole("alert")).toHaveTextContent("the token is too short");
    expect(screen.getByRole("dialog")).not.toHaveTextContent(KEY_DOOR.NOTE);
    expect(success).not.toHaveBeenCalled();
  });

  it("with a token stored, Remove lives in the door (opened from Replace)", async () => {
    const onRefresh = renderDoor(providerStatus([{ provider: gateway, state: "live" }]), { for: { provider: gateway.id } });
    await userEvent.click(screen.getByRole("button", { name: "entrance" }));
    await userEvent.click(await screen.findByRole("button", { name: KEY_DOOR.REMOVE }));
    expect(deleteCredential).toHaveBeenCalledWith("corp-gateway");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(onRefresh).toHaveBeenCalled();
  });
});

describe("today's door, where there are no providers", () => {
  it("the AWS door is unchanged: no provider line, the pane on /setup/harness-login", async () => {
    renderDoor(baseStatus(), { for: { login: "aws" } });
    await userEvent.click(screen.getByRole("button", { name: "entrance" }));
    const dialog = await screen.findByRole("dialog", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE });
    expect(dialog).not.toHaveTextContent(MODEL_ACCESS_BANNER.DIALOG_CLEANUP_NOTE);
    expect(await screen.findByTestId("fake-pane")).toHaveAttribute("data-model-provider", "");
  });

  it("in the Admin view a login request stays today's door even with providers", async () => {
    window.history.pushState({}, "", "/admin/providers");
    renderDoor(providerStatus([{ provider: bedrock, defaultFor: ["claude-code"] }]), { for: { login: "aws" } });
    await userEvent.click(screen.getByRole("button", { name: "entrance" }));
    expect(await screen.findByTestId("fake-pane")).toHaveAttribute("data-model-provider", "");
  });
});

describe("one door at a time", () => {
  it("a second entrance while it is open focuses the open door instead of opening another", async () => {
    let second: (() => void) | undefined;
    function Second() {
      const door = useModelAccessDoor();
      second = () => door.openDoor({ for: { provider: gateway.id } });
      return null;
    }
    render(
      <WithDoor status={providerStatus([{ provider: bedrock }, { provider: gateway }])}>
        <Entrance label="entrance" options={{ for: { provider: bedrock.id } }} />
        <Second />
      </WithDoor>,
    );
    await userEvent.click(screen.getByRole("button", { name: "entrance" }));
    await screen.findByTestId("fake-pane");
    act(() => second?.());
    expect(screen.getAllByRole("dialog")).toHaveLength(1);
    // Still the AWS door: the sign-in it started is not swapped for a key form.
    expect(screen.getByRole("dialog", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeInTheDocument();
    expect(document.activeElement).toBe(document.getElementById("model-access-door"));
  });
});

describe("B9 — a relaunch refused after its screen was gone", () => {
  it("shows the server's sentence with Dismiss, and Dismiss clears it", async () => {
    const sentence =
      "This run's model provider is Corp gateway, and you have not added your token for it — connect it from Getting started in the console, or from the banner the console shows on every page. Wardyn does not substitute a different model provider.";
    renderDoor(providerStatus([{ provider: bedrock }]), {
      for: { provider: bedrock.id },
      onSignedIn: () => Promise.resolve(sentence),
    });
    await userEvent.click(screen.getByRole("button", { name: "entrance" }));
    await userEvent.click(await screen.findByRole("button", { name: "fake done" }));
    expect(await screen.findByText(sentence)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: MODEL_ACCESS_BANNER.REFUSAL_DISMISS }));
    expect(screen.queryByText(sentence)).toBeNull();
  });
});
