/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's Allowed models card: drawn from run.allowed_models alone, whoever is looking.
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ALLOWED_MODELS as T } from "../../../lib/governance-copy";
import type { RunDetail } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";
import { AllowedModelsCard } from "./allowed-models-card";

const run = (patch: Partial<RunDetail> = {}) =>
  ({
    id: "r1",
    allowed_models: { models: ["model-a", "small-model-a"], unit: { id: "p1", name: "Contractors" } },
    ...patch,
  }) as RunDetail;

function renderCard(r: RunDetail, { operator = true, securityOperator = true, resolved = true } = {}) {
  return render(
    <MemoryRouter>
      <OperatorProvider operator={operator} securityOperator={securityOperator} operatorResolved={resolved} principal="alice">
        <AllowedModelsCard run={r} />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

describe("AllowedModelsCard", () => {
  it("lists the models the run started with, the profile that set them, and what the limit means", () => {
    renderCard(run());
    expect(screen.getByRole("heading", { name: T.CARD_TITLE })).toBeInTheDocument();
    expect(screen.getByText("Governance · Contractors")).toBeInTheDocument();
    expect(screen.getAllByRole("listitem").map((li) => li.textContent)).toEqual(["model-a", "small-model-a"]);
    expect(screen.getByText(T.CARD_REFUSED)).toBeInTheDocument();
    expect(screen.getByText(T.CARD_FIXED)).toBeInTheDocument();
  });

  it("offers the profile to someone who may read profiles, and not to a member", () => {
    const { unmount } = renderCard(run());
    expect(screen.getByRole("link", { name: T.OPEN_PROFILE })).toHaveAttribute("href", "/admin/governance?profile=p1");
    unmount();
    renderCard(run(), { operator: false, securityOperator: false });
    expect(screen.getByText("model-a")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: T.OPEN_PROFILE })).toBeNull();
  });

  it("holds the link back until the caller's role is known", () => {
    renderCard(run(), { resolved: false });
    expect(screen.getByText("model-a")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: T.OPEN_PROFILE })).toBeNull();
  });

  it("an administrator's governed run shows the card too: only the run's own record decides", () => {
    renderCard(run(), { operator: true, securityOperator: true });
    expect(screen.getByTestId("run-allowed-models")).toBeInTheDocument();
  });

  it("draws nothing when the run holds no list, whoever is looking", () => {
    for (const o of [{ operator: true, securityOperator: true }, { operator: false, securityOperator: false }]) {
      const { container, unmount } = renderCard(run({ allowed_models: undefined }), o);
      expect(container).toBeEmptyDOMElement();
      unmount();
    }
  });

  it("scrolls a long list inside the card, keeping the sentences below it in view", () => {
    const models = Array.from({ length: 14 }, (_, i) => `model-${i + 1}`);
    renderCard(run({ allowed_models: { models, unit: { id: "p1", name: "Contractors" } } }));
    const list = screen.getByTestId("run-allowed-models-list");
    expect(list.className).toContain("overflow-y-auto");
    expect(list.className).toContain("max-h-");
    expect(list.contains(screen.getByText(T.CARD_REFUSED))).toBe(false);
  });
});
