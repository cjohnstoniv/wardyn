/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { issue } from "../lenses/step-issues";
import { AnnounceContext, flowSteps, StepShell } from "./step-shell";

const LABELS = { shape: "Shape", run: "Run", harnesses: "Harnesses", files: "Files", tools: "Tools and access" };

afterEach(cleanup);

describe("StepShell", () => {
  it("focuses and announces its heading, and shows progress with the current step marked", () => {
    const announce = vi.fn();
    render(
      <AnnounceContext.Provider value={announce}>
        <StepShell title="What does it need?" eyebrow="Guided setup · 4 of 5" progress={{ label: "Progress", steps: flowSteps(LABELS, "files") }}
          primary={{ label: "Continue", onActivate: vi.fn() }} />
      </AnnounceContext.Provider>,
    );
    expect(screen.getByRole("heading", { level: 1 })).toHaveFocus();
    expect(announce).toHaveBeenCalledWith("What does it need?");
    expect(screen.getByRole("navigation", { name: "Progress" })).toBeInTheDocument();
    expect(screen.getByRole("listitem", { current: "step" })).toHaveTextContent("Files");
    expect(flowSteps(LABELS, "files").map((s) => s.state)).toEqual(["done", "done", "done", "current", "upcoming"]);
  });

  it("a Continue with a local issue focuses that issue's control and marks it invalid instead of moving on", () => {
    const onActivate = vi.fn();
    render(
      <StepShell title="Files" primary={{ label: "Continue", onActivate }} issues={[issue("files", "files_choice_required")]}>
        <input id="nr-files-choice" aria-label="Choice" />
      </StepShell>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    expect(onActivate).not.toHaveBeenCalled();
    expect(screen.getByLabelText("Choice")).toHaveFocus();
    expect(screen.getByLabelText("Choice")).toHaveAttribute("aria-invalid", "true");
  });

  it("Back goes back, and Ctrl+Enter continues a complete step", () => {
    const onBack = vi.fn();
    const onActivate = vi.fn();
    render(<StepShell title="Run" back={{ label: "Back", onBack }} primary={{ label: "Continue", onActivate }} />);
    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    fireEvent.keyDown(document, { key: "Enter", ctrlKey: true });
    expect(onBack).toHaveBeenCalledTimes(1);
    expect(onActivate).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("button", { name: "Continue" })).toHaveTextContent("Continue→");
  });

  it("a primary with arrow false shows no arrow", () => {
    render(<StepShell title="Review" primary={{ label: "Launch run", onActivate: vi.fn(), arrow: false }} />);
    expect(screen.getByRole("button", { name: "Launch run" })).toHaveTextContent(/^Launch run$/);
  });

  it("puts the heading aside beside the heading and the footer extras around the primary, in place of Back", () => {
    render(
      <StepShell
        title="Review"
        back={{ label: "Back", onBack: vi.fn() }}
        primary={{ label: "Launch run", onActivate: vi.fn(), arrow: false }}
        headingAside={<span>Toggle</span>}
        footerExtras={{ start: <button type="button">Save</button>, end: <button type="button">Check again</button> }}
      />,
    );
    expect(screen.getByText("Toggle").closest("div")?.previousElementSibling?.tagName).toBe("HEADER");
    const footer = screen.getByRole("contentinfo");
    expect([...footer.querySelectorAll("button")].map((b) => b.textContent)).toEqual(["Save", "Check again", "Launch run"]);
    expect(screen.queryByRole("button", { name: "Back" })).toBeNull();
  });
});
