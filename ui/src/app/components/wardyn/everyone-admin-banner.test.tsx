/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { ADMIN_ACCESS_BANNER } from "../../lib/access-posture-copy";
import type { SetupCheck, SetupStatus } from "../../lib/types";
import { EveryoneAdminBanner } from "./everyone-admin-banner";
import { ModelAccessProvider } from "./model-access-context";
import { OperatorProvider } from "./operator-context";

function status(checks: SetupCheck[]): SetupStatus {
  return { checks } as unknown as SetupStatus;
}
const WARN: SetupCheck = { id: "sso_rbac", label: "Who is an admin", status: "warn", blocking: true };
const OK: SetupCheck = { id: "sso_rbac", label: "Who is an admin", status: "ok" };

function Where() {
  const loc = useLocation();
  return <p data-testid="where">{loc.pathname + loc.search}</p>;
}

function renderBanner(st: SetupStatus | null, { operator = true, resolved = true } = {}) {
  return render(
    <MemoryRouter initialEntries={["/runs"]}>
      <ModelAccessProvider status={st} onRefresh={() => {}}>
        <OperatorProvider operator={operator} operatorResolved={resolved}>
          <EveryoneAdminBanner />
          <Routes>
            <Route path="*" element={<Where />} />
          </Routes>
        </OperatorProvider>
      </ModelAccessProvider>
    </MemoryRouter>,
  );
}

describe("EveryoneAdminBanner (#484)", () => {
  it("shows the frozen strings to an admin while everyone is an admin, and the CTA opens the People step", async () => {
    renderBanner(status([WARN]));
    expect(screen.getByText(ADMIN_ACCESS_BANNER.TITLE)).toBeInTheDocument();
    expect(screen.getByText(ADMIN_ACCESS_BANNER.BODY)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: ADMIN_ACCESS_BANNER.ACTION }));
    expect(screen.getByTestId("where")).toHaveTextContent("/setup?step=people");
  });

  it("hidden for a member, even if a warn row somehow reached them", () => {
    renderBanner(status([WARN]), { operator: false });
    expect(screen.queryByText(ADMIN_ACCESS_BANNER.TITLE)).not.toBeInTheDocument();
  });

  it("hidden while /me has not answered (the fail-open operator default)", () => {
    renderBanner(status([WARN]), { resolved: false });
    expect(screen.queryByText(ADMIN_ACCESS_BANNER.TITLE)).not.toBeInTheDocument();
  });

  it("hidden once a role map or admin list is set (the row reads ok)", () => {
    renderBanner(status([OK]));
    expect(screen.queryByText(ADMIN_ACCESS_BANNER.TITLE)).not.toBeInTheDocument();
  });

  it("hidden with no status yet, and for a member's redacted checks", () => {
    renderBanner(null);
    expect(screen.queryByText(ADMIN_ACCESS_BANNER.TITLE)).not.toBeInTheDocument();
    renderBanner(status([]));
    expect(screen.queryByText(ADMIN_ACCESS_BANNER.TITLE)).not.toBeInTheDocument();
  });

  it("disappears as soon as the shell's status stops reading the state", () => {
    const { rerender } = renderBanner(status([WARN]));
    expect(screen.getByText(ADMIN_ACCESS_BANNER.TITLE)).toBeInTheDocument();
    rerender(
      <MemoryRouter>
        <ModelAccessProvider status={status([OK])} onRefresh={() => {}}>
          <OperatorProvider operator>
            <EveryoneAdminBanner />
          </OperatorProvider>
        </ModelAccessProvider>
      </MemoryRouter>,
    );
    expect(screen.queryByText(ADMIN_ACCESS_BANNER.TITLE)).not.toBeInTheDocument();
  });
});
