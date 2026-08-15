/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const listIntegrationsMock = vi.fn();
vi.mock("../../../lib/api/integrations", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/integrations")>(
    "../../../lib/api/integrations",
  );
  return { ...actual, integrationsApi: { list: () => listIntegrationsMock() } };
});

import { BuildStepsEditor, ImageCards, StepBaseImage } from "./step-base-image";
import { defaultBaseImageState, newSourceRow, type BaseImageState, type SourceScanState } from "./wizard-types";
import { C, V2C } from "../../../lib/workspace-copy";

beforeEach(() => {
  listIntegrationsMock.mockReset();
});

describe("StepBaseImage — Phase A scan progress", () => {
  const local = { ...newSourceRow("local_dir"), path: "/home/me/payments" };
  const repoScanning = { ...newSourceRow("repo"), source: "acme/payments-service" };
  const repoFailed = { ...newSourceRow("repo"), source: "acme/other" };
  const eph = newSourceRow("ephemeral", true);
  const scans: Record<string, SourceScanState> = {
    [local.id]: { status: "done" },
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
        state={defaultBaseImageState()}
        onChange={vi.fn()}
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
        state={defaultBaseImageState()}
        onChange={vi.fn()}
      />,
    );
    expect(screen.getByText("clone failed: terminal prompts disabled")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Edit source" }));
    expect(onEditSource).toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Rescan" }));
    expect(onRescan).toHaveBeenCalled();
  });
});

// A small stateful wrapper so the four cards + editor behave like they will
// under wizard.tsx (controlled state + patch), without pulling wizard.tsx in.
function CardsHarness({
  initial,
  onPatch,
}: {
  initial?: Partial<BaseImageState>;
  onPatch?: (patch: Partial<BaseImageState>) => void;
}) {
  const [state, setState] = React.useState<BaseImageState>({ ...defaultBaseImageState(), ...initial });
  return (
    <ImageCards
      detectedChips={["Go 1.22", "Node 20"]}
      partial={false}
      state={state}
      onChange={(patch) => {
        onPatch?.(patch);
        setState((s) => ({ ...s, ...patch }));
      }}
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

  it("selecting Customize the build discloses base + steps only — the dead tool checklist is gone", () => {
    render(<CardsHarness />);
    fireEvent.click(screen.getByText("Customize the build"));
    // What actually reaches the build: the base image + the steps editor —
    // never a tool checklist (checkboxes that changed nothing were deleted;
    // an operator expresses tools as build steps).
    expect(screen.getByRole("textbox", { name: "Base image" })).toBeInTheDocument();
    expect(screen.getByLabelText("Custom build steps")).toBeInTheDocument();
    expect(screen.queryByText("Claude Code CLI")).not.toBeInTheDocument();
    expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
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

  // The editor lives inside ImageCard, whose role=radio onKeyDown preventDefaults
  // Enter and Space to select the card. preventDefault on a keydown kills the
  // keystroke itself, so without a keydown guard on the disclosed body a real
  // build step — every one of which contains a space, most of which need a
  // second line — cannot be TYPED at all (paste still worked, which is how this
  // shipped). It must be userEvent, not fireEvent.change: only a real keydown
  // reaches the card, so a change-driven test cannot see the regression.
  it("BuildStepsEditor accepts spaces and newlines typed inside the card, without re-selecting it", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const onPatch = vi.fn();
    render(<CardsHarness initial={{ choice: "custom", buildSteps: "" }} onPatch={onPatch} />);
    const editor = screen.getByLabelText("Custom build steps");
    await user.click(editor);
    onPatch.mockClear();
    await user.keyboard("RUN apt-get install -y protobuf-compiler{Enter}ENV GOFLAGS=-mod=vendor");
    expect(editor).toHaveValue("RUN apt-get install -y protobuf-compiler\nENV GOFLAGS=-mod=vendor");
    // Typing must never re-run the card's selection: every patch is a buildSteps
    // edit, none is a choice change.
    expect(onPatch.mock.calls.every(([p]) => !("choice" in p))).toBe(true);
    expect(screen.getByTestId("image-card-custom")).toHaveAttribute("aria-checked", "true");
  });

  it("the BYO image-ref field accepts a typed space too (same disclosed-body guard)", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<CardsHarness initial={{ choice: "byo", byoRef: "" }} />);
    const field = screen.getByLabelText("Image ref");
    await user.click(field);
    await user.keyboard("ghcr.io/acme/dev:latest ");
    expect(field).toHaveValue("ghcr.io/acme/dev:latest ");
  });
});

// ui-wsWizard-4: role=radio's interaction contract (APG radiogroup pattern)
// is roving tabindex + arrow keys moving selection and focus together —
// every card used to be its own tab stop with no arrow handling at all.
describe("StepBaseImage — radiogroup keyboard semantics (ui-wsWizard-4)", () => {
  it("only the selected card is a tab stop (roving tabindex)", () => {
    render(<CardsHarness />);
    expect(screen.getByTestId("image-card-recommended")).toHaveAttribute("tabindex", "0");
    expect(screen.getByTestId("image-card-registry")).toHaveAttribute("tabindex", "-1");
    expect(screen.getByTestId("image-card-custom")).toHaveAttribute("tabindex", "-1");
    expect(screen.getByTestId("image-card-byo")).toHaveAttribute("tabindex", "-1");
  });

  it("ArrowDown moves selection AND focus to the next card, and demotes the old one's tabindex", () => {
    render(<CardsHarness />);
    const recommended = screen.getByTestId("image-card-recommended");
    recommended.focus();
    fireEvent.keyDown(recommended, { key: "ArrowDown" });

    const registry = screen.getByTestId("image-card-registry");
    expect(registry).toHaveAttribute("aria-checked", "true");
    expect(registry).toHaveAttribute("tabindex", "0");
    expect(document.activeElement).toBe(registry);
    expect(recommended).toHaveAttribute("aria-checked", "false");
    expect(recommended).toHaveAttribute("tabindex", "-1");
  });

  it("ArrowUp from the first card wraps to the last, and ArrowLeft/ArrowRight behave the same as Up/Down", () => {
    render(<CardsHarness />);
    const recommended = screen.getByTestId("image-card-recommended");
    recommended.focus();
    fireEvent.keyDown(recommended, { key: "ArrowUp" });

    const byo = screen.getByTestId("image-card-byo");
    expect(byo).toHaveAttribute("aria-checked", "true");
    expect(document.activeElement).toBe(byo);

    fireEvent.keyDown(byo, { key: "ArrowRight" });
    expect(screen.getByTestId("image-card-recommended")).toHaveAttribute("aria-checked", "true");
  });
});

describe("ImageCards — inventory, never consequence (the tools-not-AI law)", () => {
  // The image doesn't decide whether or which AI is used — the workspace's
  // requirements do. A card may NAME a tool it carries; only the requirements
  // and run surfaces say what a tool is for.
  it("recommended card's Carries renders detectedChips verbatim", () => {
    render(<CardsHarness />);
    expect(screen.getAllByText("Go 1.22").length).toBeGreaterThan(0);
  });

  it("the registry card carries no warning — no card states an AI consequence", () => {
    render(<CardsHarness />);
    expect(screen.queryByText(/agent runs can't drive it/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Claude Code configured/)).not.toBeInTheDocument();
    expect(screen.queryByText(/No AI integration connected/)).not.toBeInTheDocument();
  });

  it("the BYO card states the no-inject law — tools only, no Claude clause", () => {
    render(<CardsHarness initial={{ choice: "byo" }} />);
    expect(screen.getByText(V2C.IMG_NO_INJECT)).toBeInTheDocument();
    expect(screen.queryByText(/governed commands only/)).not.toBeInTheDocument();
  });

  // ui-wsWizard-5: an empty scan used to render "What the image needs" over a
  // blank chip row with no fallback sentence — reads as a rendering glitch.
  it("shows a fallback line under 'What the image needs' when the scan detected nothing", () => {
    render(
      <StepBaseImage
        sources={[]}
        scans={{}}
        phaseA={false}
        partial={false}
        onEditSource={vi.fn()}
        onRescan={vi.fn()}
        detectedChips={[]}
        state={defaultBaseImageState()}
        onChange={vi.fn()}
      />,
    );
    expect(screen.getByText(/nothing detected in the scan/i)).toBeInTheDocument();
  });

  it("offers no power-source row and no Change… peek — that choice lives on Reach now", () => {
    render(
      <StepBaseImage
        sources={[]}
        scans={{}}
        phaseA={false}
        partial={false}
        onEditSource={vi.fn()}
        onRescan={vi.fn()}
        detectedChips={[]}
        state={defaultBaseImageState()}
        onChange={vi.fn()}
      />,
    );
    expect(screen.queryByText(/Agent runs here use/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Change…" })).not.toBeInTheDocument();
  });
});
