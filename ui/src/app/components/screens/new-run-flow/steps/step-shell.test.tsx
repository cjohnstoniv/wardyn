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

  it("clears the invalid mark once the issue that put it there is gone", () => {
    const view = render(
      <StepShell title="Run" primary={{ label: "Continue", onActivate: vi.fn() }} issues={[issue("run", "pool_required")]}>
        <select id="nr-pool" aria-label="Pool"><option>One</option></select>
      </StepShell>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    expect(screen.getByLabelText("Pool")).toHaveAttribute("aria-invalid", "true");
    view.rerender(
      <StepShell title="Run" primary={{ label: "Continue", onActivate: vi.fn() }} issues={[]}>
        <select id="nr-pool" aria-label="Pool"><option>One</option></select>
      </StepShell>,
    );
    expect(screen.getByLabelText("Pool")).not.toHaveAttribute("aria-invalid");
  });

  it("clears every earlier mark, not only the latest, once its issue is gone", () => {
    const shell = (issues: ReturnType<typeof issue>[]) => (
      <StepShell title="Run" primary={{ label: "Continue", onActivate: vi.fn() }} issues={issues}>
        <select id="nr-pool" aria-label="Pool"><option>One</option></select>
        <input id="nr-command" aria-label="Command" />
      </StepShell>
    );
    const view = render(shell([issue("run", "pool_required"), issue("run", "command_required")]));
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    view.rerender(shell([issue("run", "command_required"), issue("run", "pool_required")]));
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    view.rerender(shell([issue("run", "command_required")]));
    expect(screen.getByLabelText("Pool")).not.toHaveAttribute("aria-invalid");
    expect(screen.getByLabelText("Command")).toHaveAttribute("aria-invalid", "true");
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

  it("refuses Back together with footer extras by type, since extras replace Back", () => {
    // The directive is the assertion: `tsc -p tsconfig.check.json` fails when the pair is accepted.
    // @ts-expect-error back and footerExtras are exclusive
    render(<StepShell title="Review" back={{ label: "Back", onBack: vi.fn() }} footerExtras={{ start: <span>Save</span> }} />);
    expect(screen.getByText("Save")).toBeInTheDocument();
  });

  it("pins the footer to the bottom only when asked", () => {
    render(<StepShell title="Tools" primary={{ label: "Review run", onActivate: vi.fn() }} />);
    expect(screen.getByRole("contentinfo")).not.toHaveClass("sticky");
    cleanup();
    render(<StepShell title="Tools" stickyFooter primary={{ label: "Review run", onActivate: vi.fn() }} />);
    expect(screen.getByRole("contentinfo")).toHaveClass("sticky", "bottom-0");
  });

  it("with no primary shows only Back, and Ctrl+Enter does nothing", () => {
    const onBack = vi.fn();
    render(<StepShell title="Shape" back={{ label: "Back", onBack }} />);
    expect(screen.getAllByRole("button").map((b) => b.textContent)).toEqual(["Back"]);
    const e = new KeyboardEvent("keydown", { key: "Enter", ctrlKey: true, cancelable: true });
    document.dispatchEvent(e);
    expect(e.defaultPrevented).toBe(false);
    expect(onBack).not.toHaveBeenCalled();
  });

  it("keeps the same Launch node, and its focus, while Check again comes and goes", () => {
    const shell = (checkAgain: boolean) => (
      <StepShell
        title="Review"
        primary={{ label: "Launch run", onActivate: vi.fn(), arrow: false }}
        footerExtras={{ start: <button type="button">Save</button>, end: checkAgain && <button type="button">Check again</button> }}
      />
    );
    const { rerender } = render(shell(true));
    const launch = screen.getByRole("button", { name: "Launch run" });
    launch.focus();
    rerender(shell(false));
    expect(screen.queryByRole("button", { name: "Check again" })).toBeNull();
    expect(screen.getByRole("button", { name: "Launch run" })).toBe(launch);
    expect(launch).toHaveFocus();
    rerender(shell(true));
    expect(screen.getByRole("button", { name: "Launch run" })).toBe(launch);
    expect(launch).toHaveFocus();
  });

  it("gives the heading the flow-title rung with or without an aside (#3080) and aligns the aside as asked", () => {
    const { unmount } = render(<StepShell title="Run" primary={{ label: "Go", onActivate: vi.fn() }} />);
    expect(screen.getByRole("heading", { level: 1 })).toHaveClass("text-display");
    unmount();
    render(<StepShell title="Run document" headingAside={<span>Open</span>} headingAsideAlign="end" primary={{ label: "Go", onActivate: vi.fn() }} />);
    expect(screen.getByRole("heading", { level: 1 })).toHaveClass("text-display");
    expect(screen.getByText("Open").parentElement?.parentElement).toHaveClass("items-end");
  });
});
