/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import type { MockInstance } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { DropdownMenu, DropdownMenuContent } from "../ui/dropdown-menu";
import { MEMBER_MODE, MemberModeBanner, MemberModeMenuItem } from "./member-mode-banner";
import { health as api } from "../../lib/api/health";

// The POST bodies matter as much as the render: the whole feature is one route
// with one boolean, and sending the wrong one silently strands an admin in
// member mode with an "Exit" that re-enters it.
let setMemberMode: MockInstance<(enabled: boolean) => Promise<void>>;

beforeEach(() => {
  setMemberMode = vi.spyOn(api, "setMemberMode").mockResolvedValue(undefined);
});
afterEach(() => {
  vi.restoreAllMocks();
});

describe("MemberModeBanner", () => {
  it("renders nothing when the mode is off", () => {
    const { container } = render(<MemberModeBanner active={false} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("states the mode and names the three ceilings in its tooltip", () => {
    render(<MemberModeBanner active={true} />);
    const line = screen.getByText(MEMBER_MODE.BANNER);
    expect(line).toBeInTheDocument();
    expect(line).toHaveAttribute("title", MEMBER_MODE.CEILINGS);
    // The ceilings the doc and the banner must agree on — read as three claims,
    // not as one paragraph nobody checks.
    expect(MEMBER_MODE.CEILINGS).toMatch(/ROLE only/);
    expect(MEMBER_MODE.CEILINGS).toMatch(/SSH key/);
    expect(MEMBER_MODE.CEILINGS).toMatch(/rolling upgrade/);
  });

  it("Exit posts enabled:false and then reloads", async () => {
    const onExited = vi.fn();
    render(<MemberModeBanner active={true} onExited={onExited} />);
    await userEvent.click(screen.getByRole("button", { name: MEMBER_MODE.EXIT }));
    await waitFor(() => expect(setMemberMode).toHaveBeenCalledWith(false));
    await waitFor(() => expect(onExited).toHaveBeenCalled());
  });

  it("a failed Exit says so and does NOT reload", async () => {
    setMemberMode.mockRejectedValue(new Error("boom"));
    const onExited = vi.fn();
    render(<MemberModeBanner active={true} onExited={onExited} />);
    await userEvent.click(screen.getByRole("button", { name: MEMBER_MODE.EXIT }));
    expect(await screen.findByText(MEMBER_MODE.FAILED)).toBeInTheDocument();
    expect(onExited).not.toHaveBeenCalled();
  });
});

describe("MemberModeMenuItem", () => {
  function renderItem(props: {
    meta: { operator: boolean; securityOperator: boolean; method: string };
    onEntered?: () => void;
  }) {
    return render(
      <DropdownMenu open>
        <DropdownMenuContent>
          <MemberModeMenuItem {...props} />
        </DropdownMenuContent>
      </DropdownMenu>,
    );
  }

  // The /me tier shapes, verbatim: "admin" is both predicates, "security_admin"
  // is only the second, "member" is neither (me.go's two predicates).
  const admin = { operator: true, securityOperator: true, method: "sso" };
  const securityAdmin = { operator: false, securityOperator: true, method: "sso" };
  const member = { operator: false, securityOperator: false, method: "sso" };

  it("is offered to an SSO operator", () => {
    renderItem({ meta: admin });
    expect(screen.getByText(MEMBER_MODE.MENU)).toBeInTheDocument();
  });

  // R-04: `operator` is super-admin-only (isOperator), so a security admin needs
  // the second predicate or the console never offers them a control the server
  // route (classMember, SSO human only) would happily serve over curl.
  it("is offered to an SSO security_admin too", () => {
    renderItem({ meta: securityAdmin });
    expect(screen.getByText(MEMBER_MODE.MENU)).toBeInTheDocument();
  });

  it("is hidden for a member", () => {
    renderItem({ meta: member });
    expect(screen.queryByText(MEMBER_MODE.MENU)).not.toBeInTheDocument();
  });

  // The server refuses the admin-token / local-mode / no-IdP lane with a 400 —
  // there is no per-person role to pause — so offering the control there would
  // be offering a refusal.
  it.each(["token", "local", ""])("is hidden for method=%s", (method) => {
    renderItem({ meta: { ...admin, method } });
    expect(screen.queryByText(MEMBER_MODE.MENU)).not.toBeInTheDocument();
  });

  it("posts enabled:true and then lands at the root", async () => {
    const onEntered = vi.fn();
    renderItem({ meta: admin, onEntered });
    await userEvent.click(screen.getByText(MEMBER_MODE.MENU));
    await waitFor(() => expect(setMemberMode).toHaveBeenCalledWith(true));
    await waitFor(() => expect(onEntered).toHaveBeenCalled());
  });
});
