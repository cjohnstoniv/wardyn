/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { GOVERNED_ADMIN_BANNER } from "../../lib/access-posture-copy";
import type { SetupStatus } from "../../lib/types";
import { GovernedAdminBanner } from "./governed-admin-banner";
import { ModelAccessProvider } from "./model-access-context";
import { OperatorProvider } from "./operator-context";
import type { ConsoleView } from "./console-view";

type Auth = NonNullable<SetupStatus["auth"]>;

function status(auth: Partial<Auth>): SetupStatus {
  return { checks: [], auth: { mode: "sso", local_loopback: false, ...auth } } as unknown as SetupStatus;
}

const ON = status({ govern_admin_runs: true });

function renderBanner(
  st: SetupStatus | null,
  { operator = true, resolved = true, view = "admin", principal = "sub-admin" }: {
    operator?: boolean;
    resolved?: boolean;
    view?: ConsoleView;
    principal?: string;
  } = {},
) {
  return render(
    <MemoryRouter>
      <ModelAccessProvider status={st} onRefresh={() => {}}>
        <OperatorProvider operator={operator} operatorResolved={resolved} principal={principal}>
          <GovernedAdminBanner view={view} />
        </OperatorProvider>
      </ModelAccessProvider>
    </MemoryRouter>,
  );
}

afterEach(() => sessionStorage.clear());

describe("GovernedAdminBanner (mock M10)", () => {
  it("shows the approved copy to a signed-in admin in the Admin view", () => {
    renderBanner(ON);
    expect(screen.getByText(GOVERNED_ADMIN_BANNER.TITLE)).toBeInTheDocument();
    expect(screen.getByText(GOVERNED_ADMIN_BANNER.BODY)).toBeInTheDocument();
    expect(screen.queryByText(GOVERNED_ADMIN_BANNER.BODY_RECORDING_EXEMPT)).not.toBeInTheDocument();
  });

  it("says Record Mode is exempt when recording is in the exempt list", () => {
    renderBanner(status({ govern_admin_runs: true, govern_admin_runs_exempt: ["recording"] }));
    expect(screen.getByText(GOVERNED_ADMIN_BANNER.BODY_RECORDING_EXEMPT)).toBeInTheDocument();
    expect(screen.queryByText(GOVERNED_ADMIN_BANNER.BODY)).not.toBeInTheDocument();
  });

  it("carries the canon strings byte for byte", () => {
    expect(GOVERNED_ADMIN_BANNER.TITLE).toBe("Your runs are governed like everyone else's");
    expect(GOVERNED_ADMIN_BANNER.BODY).toBe(
      "This deployment governs admins' own runs. Yours are bounded by the governance profile and grants that apply to you. Record Mode is refused while this is on. The admin token stays outside, for break-glass.",
    );
    expect(GOVERNED_ADMIN_BANNER.BODY_RECORDING_EXEMPT).toBe(
      "This deployment governs admins' own runs. Yours are bounded by the governance profile and grants that apply to you. Record Mode is exempt and runs as before. The admin token stays outside, for break-glass.",
    );
    expect(GOVERNED_ADMIN_BANNER.HIDE).toBe("Hide");
  });

  it.each([
    ["the switch is off", status({ govern_admin_runs: false }), {}],
    ["the field is absent (an older daemon)", status({}), {}],
    ["nobody is signed in (admin token mode)", status({ govern_admin_runs: true, mode: "token" }), {}],
    ["nobody is signed in (local mode)", status({ govern_admin_runs: true, mode: "local" }), {}],
    ["the caller is not an admin", ON, { operator: false }],
    ["/me has not answered", ON, { resolved: false }],
    ["the User view", ON, { view: "user" as ConsoleView }],
    ["no status yet", null, {}],
  ])("is absent when %s", (_name, st, opts) => {
    renderBanner(st, opts);
    expect(screen.queryByText(GOVERNED_ADMIN_BANNER.TITLE)).not.toBeInTheDocument();
  });

  it("Hide dismisses it for the session, for that person only", async () => {
    const { unmount } = renderBanner(ON);
    await userEvent.click(screen.getByRole("button", { name: GOVERNED_ADMIN_BANNER.HIDE }));
    expect(screen.queryByText(GOVERNED_ADMIN_BANNER.TITLE)).not.toBeInTheDocument();
    unmount();

    const again = renderBanner(ON);
    expect(screen.queryByText(GOVERNED_ADMIN_BANNER.TITLE)).not.toBeInTheDocument();
    again.unmount();

    renderBanner(ON, { principal: "sub-other-admin" });
    expect(screen.getByText(GOVERNED_ADMIN_BANNER.TITLE)).toBeInTheDocument();
  });
});
