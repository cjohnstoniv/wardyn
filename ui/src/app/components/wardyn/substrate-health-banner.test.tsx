/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { SUBSTRATE_BANNER } from "../../lib/substrate-banner-copy";
import type { SetupCheck, SetupStatus } from "../../lib/types";
import type { ConsoleView } from "./console-view";
import { ModelAccessProvider } from "./model-access-context";
import { OperatorProvider } from "./operator-context";
import { SubstrateHealthBanner } from "./substrate-health-banner";

const DETAIL = "The sandbox runner refuses Wardyn's credentials.";
const row = (over: Partial<SetupCheck>): SetupCheck => ({
  id: "substrate_health",
  label: "Runner health",
  status: "fail",
  cause: "runner_auth",
  detail: DETAIL,
  ...over,
});

function Where() {
  const loc = useLocation();
  return <p data-testid="where">{loc.pathname + loc.search}</p>;
}

function renderBanner(
  checks: SetupCheck[],
  { operator = true, resolved = true, view = "admin" as ConsoleView } = {},
) {
  return render(
    <MemoryRouter initialEntries={["/runs"]}>
      <ModelAccessProvider status={{ checks } as unknown as SetupStatus} onRefresh={() => {}}>
        <OperatorProvider operator={operator} operatorResolved={resolved}>
          <SubstrateHealthBanner view={view} />
          <Routes>
            <Route path="*" element={<Where />} />
          </Routes>
        </OperatorProvider>
      </ModelAccessProvider>
    </MemoryRouter>,
  );
}

describe("SubstrateHealthBanner (M10)", () => {
  it("shows the cause's title, the row's detail verbatim, and opens the review step", async () => {
    renderBanner([row({})]);
    expect(screen.getByText(SUBSTRATE_BANNER.TITLE_AUTH)).toBeInTheDocument();
    expect(screen.getByText(DETAIL)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: SUBSTRATE_BANNER.ACTION }));
    expect(screen.getByTestId("where")).toHaveTextContent("/admin/setup?step=review");
  });

  it("titles each cause", () => {
    const { unmount } = renderBanner([row({ cause: "runner_unreachable" })]);
    expect(screen.getByText(SUBSTRATE_BANNER.TITLE_UNREACHABLE)).toBeInTheDocument();
    unmount();
    renderBanner([row({ status: "warn", cause: "sweep_stale" })]);
    expect(screen.getByText(SUBSTRATE_BANNER.TITLE_SWEEP)).toBeInTheDocument();
  });

  it("is silent when the row is ok", () => {
    renderBanner([row({ status: "ok", cause: undefined })]);
    expect(screen.queryByRole("button", { name: SUBSTRATE_BANNER.ACTION })).not.toBeInTheDocument();
  });

  it("is hidden for a member", () => {
    renderBanner([row({})], { operator: false });
    expect(screen.queryByText(SUBSTRATE_BANNER.TITLE_AUTH)).not.toBeInTheDocument();
  });

  it("is hidden while /me has not answered", () => {
    renderBanner([row({})], { resolved: false });
    expect(screen.queryByText(SUBSTRATE_BANNER.TITLE_AUTH)).not.toBeInTheDocument();
  });

  it("is hidden in the User view", () => {
    renderBanner([row({})], { view: "user" });
    expect(screen.queryByText(SUBSTRATE_BANNER.TITLE_AUTH)).not.toBeInTheDocument();
  });
});
