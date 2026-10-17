/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PathCard } from "./path-card";

const icon = <svg data-testid="icon" />;

describe("PathCard", () => {
  it("a selectable card is one button that selects", async () => {
    const onSelect = vi.fn();
    render(<PathCard title="Guided setup" description="A few questions" icon={icon} badge="Fastest" foot="Start" onSelect={onSelect} />);
    await userEvent.click(screen.getByRole("button", { name: /Guided setup/ }));
    expect(onSelect).toHaveBeenCalledTimes(1);
    expect(screen.getByText("Fastest")).toBeVisible();
  });

  it("a coming-soon card says so in words, is not a button, and sets no aria-disabled the article role does not allow (FR-0124)", () => {
    render(<PathCard title="AI composer" description="Soon" icon={icon} comingSoon onSelect={() => {}} />);
    expect(screen.getByText("Coming soon")).toBeVisible();
    expect(screen.getByRole("article")).not.toHaveAttribute("aria-disabled");
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("a card with actions is a named region holding its own buttons, not a button", async () => {
    const onYaml = vi.fn();
    render(
      <PathCard
        title="Import a document"
        description="YAML or JSON"
        icon={icon}
        actions={
          <>
            <button type="button" onClick={onYaml}>YAML</button>
            <button type="button">JSON</button>
          </>
        }
      />,
    );
    expect(screen.getByRole("region", { name: "Import a document" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "YAML" }));
    expect(onYaml).toHaveBeenCalledTimes(1);
    expect(screen.getAllByRole("button")).toHaveLength(2);
  });
});
