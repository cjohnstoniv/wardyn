/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";

const listIntegrationsMock = vi.fn();
vi.mock("../../../lib/api/integrations", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/integrations")>(
    "../../../lib/api/integrations",
  );
  return { ...actual, integrationsApi: { list: () => listIntegrationsMock() } };
});

import { BuildStepsEditor, ImageCards, PowerSourcePeek, StepBaseImage } from "./step-base-image";
import { defaultBaseImageState, newSourceRow, type BaseImageState, type PowerSource, type SourceScanState } from "./wizard-types";
import { C, V2C } from "../../../lib/workspace-copy";
import type { IntegrationRow } from "../../../lib/api/integrations";

beforeEach(() => {
  listIntegrationsMock.mockReset();
});

describe("StepBaseImage — Phase A scan progress", () => {
  const local = { ...newSourceRow("local_dir"), path: "/home/me/payments" };
  const repoScanning = { ...newSourceRow("repo"), source: "acme/payments-service" };
  const repoFailed = { ...newSourceRow("repo"), source: "acme/other" };
  const eph = newSourceRow("ephemeral", true);
  const scans: Record<string, SourceScanState> = {
    [local.id]: { status: "done", secs: 0.4 },
    [repoScanning.id]: { status: "scanning", startedAt: Date.now() - 5000 },
    [repoFailed.id]: { status: "failed", error: "clone failed: terminal prompts disabled" },
  };

  it("shows nothing-to-scan for ephemeral, done for local dirs, and elapsed+Runs-link while a repo scans", () => {
    render(
      <StepBaseImage
        sources={[eph, local, repoScanning]}
        scans={scans}
        phaseA
        partial={false}
        onEditSource={vi.fn()}
        onRescan={vi.fn()}
        detectedChips={[]}
        harnessAvailable
        state={defaultBaseImageState()}
        onChange={vi.fn()}
        powerSource={{ kind: "default" }}
        onPowerSourceChange={vi.fn()}
      />,
    );
    expect(screen.getByText("nothing to scan")).toBeInTheDocument();
    expect(screen.getByText(/scanned/)).toBeInTheDocument();
    expect(screen.getByText(/watch it under Runs/)).toBeInTheDocument();
    expect(screen.getByText(C.SCAN_REPO)).toBeInTheDocument();
  });

  it("shows the server's real failure reason verbatim plus Edit source / Rescan, which fire their callbacks", () => {
    const onEditSource = vi.fn();
    const onRescan = vi.fn();
    render(
      <StepBaseImage
        sources={[repoFailed]}
        scans={scans}
        phaseA
        partial={false}
        onEditSource={onEditSource}
        onRescan={onRescan}
        detectedChips={[]}
        harnessAvailable
        state={defaultBaseImageState()}
        onChange={vi.fn()}
        powerSource={{ kind: "default" }}
        onPowerSourceChange={vi.fn()}
      />,
    );
    expect(screen.getByText("clone failed: terminal prompts disabled")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Edit source" }));
    expect(onEditSource).toHaveBeenCalledWith(repoFailed.id);
    fireEvent.click(screen.getByRole("button", { name: "Rescan" }));
    expect(onRescan).toHaveBeenCalledWith(repoFailed.id);
  });
});

// A small stateful wrapper so the four cards + editor behave like they will
// under wizard.tsx (controlled state + patch), without pulling wizard.tsx in.
function CardsHarness({ initial, harnessAvailable = true }: { initial?: Partial<BaseImageState>; harnessAvailable?: boolean }) {
  const [state, setState] = React.useState<BaseImageState>({ ...defaultBaseImageState(), ...initial });
  return (
    <ImageCards
      detectedChips={["Go 1.22", "Node 20"]}
      partial={false}
      harnessAvailable={harnessAvailable}
      state={state}
      onChange={(patch) => setState((s) => ({ ...s, ...patch }))}
    />
  );
}

describe("StepBaseImage — the four base-image cards", () => {
  it("renders all four, defaulting to Recommended selected", () => {
    render(<CardsHarness />);
    expect(screen.getByText("Recommended — built for this workspace")).toBeInTheDocument();
    expect(screen.getByText("A registry image that fits")).toBeInTheDocument();
    expect(screen.getByText("Customize the build")).toBeInTheDocument();
    expect(screen.getByText("Bring your own image")).toBeInTheDocument();
    expect(screen.getByTestId("image-card-recommended")).toHaveAttribute("aria-checked", "true");
  });

  it("selecting Bring your own image discloses its field and the no-scan honesty line", () => {
    render(<CardsHarness />);
    fireEvent.click(screen.getByText("Bring your own image"));
    expect(screen.getByTestId("image-card-byo")).toHaveAttribute("aria-checked", "true");
    expect(screen.getByLabelText("Image ref")).toBeInTheDocument();
    expect(screen.getByText(C.IMG_NO_SCAN)).toBeInTheDocument();
  });

  it("selecting Customize the build discloses the tool checklist (from the detected chips) and the Claude Code CLI toggle when available", () => {
    render(<CardsHarness />);
    fireEvent.click(screen.getByText("Customize the build"));
    expect(screen.getByText("Go 1.22")).toBeInTheDocument();
    expect(screen.getByText("Node 20")).toBeInTheDocument();
    expect(screen.getByText("Claude Code CLI")).toBeInTheDocument();
  });

  it("omits the Claude Code CLI toggle when no integration can drive it", () => {
    render(<CardsHarness harnessAvailable={false} />);
    fireEvent.click(screen.getByText("Customize the build"));
    expect(screen.queryByText("Claude Code CLI")).not.toBeInTheDocument();
  });

  it("BuildStepsEditor: a credential-shaped line warns with the line + detector kind, never echoes the value, and never disables anything", () => {
    render(<CardsHarness initial={{ choice: "custom" }} />);
    const editor = screen.getByLabelText("Custom build steps");
    fireEvent.change(editor, {
      target: { value: "RUN echo hi\nENV GITHUB_TOKEN=ghp_Zx9q4tW8kLmNo2rPvJd6yBhTcE1aSfUgIjKl" },
    });
    const warning = screen.getByTestId("cred-warning");
    expect(warning).toHaveTextContent("Line 2 looks like a credential (GitHub token–shaped).");
    expect(warning).toHaveTextContent(V2C.CRED_WARN);
    // The raw value is never echoed in the warning itself.
    expect(warning.textContent).not.toContain("ghp_Zx9q4tW8kLmNo2rPvJd6yBhTcE1aSfUgIjKl");
    // Nothing about the editor itself is blocked by the flag — it's a warning.
    expect(editor).not.toBeDisabled();
    expect(editor).toHaveValue("RUN echo hi\nENV GITHUB_TOKEN=ghp_Zx9q4tW8kLmNo2rPvJd6yBhTcE1aSfUgIjKl");
    // A card-level chip also names the count.
    expect(screen.getByText("1 credential-shaped build step")).toBeInTheDocument();
  });

  it("BuildStepsEditor leaves ordinary build steps unflagged", () => {
    render(<BuildStepsEditor value={"RUN apt-get install -y protobuf-compiler\nENV GOFLAGS=-mod=vendor"} onChange={vi.fn()} />);
    expect(screen.queryByTestId("cred-warning")).not.toBeInTheDocument();
  });
});

describe("StepBaseImage — the power-source peek", () => {
  const rows: IntegrationRow[] = [
    {
      id: "ai:anthropic_api_key",
      category: "ai_provider",
      name: "Anthropic (API key)",
      typeLabel: "anthropic · api key",
      chips: [],
      residency: "proxy_injected",
      posture: { kind: "configured" },
      secretNames: ["anthropic-api-key"],
      aiType: "anthropic_api_key",
      checkIds: [],
    },
    {
      id: "ai:openai_api_key",
      category: "ai_provider",
      name: "OpenAI (API key)",
      typeLabel: "openai · api key",
      chips: [],
      residency: "proxy_injected",
      posture: { kind: "configured" },
      secretNames: ["openai-api-key"],
      aiType: "openai_api_key",
      checkIds: [],
    },
  ];

  it("lists 'Use the server default' first, and mutes a row that can't drive Claude Code with its verbatim reason", async () => {
    listIntegrationsMock.mockResolvedValue({ ai: rows, scm: [], mirror: [], proxy: [] });
    const onPick = vi.fn();
    render(<PowerSourcePeek open onOpenChange={vi.fn()} powerSource={{ kind: "default" }} onPick={onPick} />);

    await screen.findByText("Anthropic (API key)");
    const buttons = screen.getAllByRole("button");
    expect(buttons[0]).toHaveTextContent("Use the server default");
    expect(buttons[0]).toHaveAttribute("aria-pressed", "true");

    const openaiButton = screen.getByRole("button", { name: /OpenAI \(API key\)/ });
    expect(openaiButton).toBeDisabled();
    // The verbatim IMPOSSIBLE-map reason for openai_api_key + claude_code.
    expect(within(openaiButton).getByText(/Claude Code speaks the Anthropic API only/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /Anthropic \(API key\)/ }));
    expect(onPick).toHaveBeenCalledWith({ kind: "pinned", integrationId: "ai:anthropic_api_key", name: "Anthropic (API key)" });
  });

  it("picking a source updates the resolved power-source line and closes the peek", async () => {
    listIntegrationsMock.mockResolvedValue({ ai: rows, scm: [], mirror: [], proxy: [] });

    function Harness() {
      const [powerSource, setPowerSource] = React.useState<PowerSource>({ kind: "default" });
      return (
        <StepBaseImage
          sources={[]}
          scans={{}}
          phaseA={false}
          partial={false}
          onEditSource={vi.fn()}
          onRescan={vi.fn()}
          detectedChips={[]}
          harnessAvailable
          state={defaultBaseImageState()}
          onChange={vi.fn()}
          powerSource={powerSource}
          onPowerSourceChange={setPowerSource}
        />
      );
    }
    render(<Harness />);
    expect(screen.getByText(/server default/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Change…" }));
    await screen.findByText("Anthropic (API key)");
    fireEvent.click(screen.getByRole("button", { name: /Anthropic \(API key\)/ }));
    await waitFor(() => expect(screen.getByText(/pinned to this workspace/)).toBeInTheDocument());
  });
});
