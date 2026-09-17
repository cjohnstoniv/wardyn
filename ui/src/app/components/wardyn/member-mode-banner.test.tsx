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
let setMemberMode: MockInstance<(enabled: boolean, noCredential?: boolean) => Promise<void>>;

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

  it("states the mode and names the ceilings in its tooltip", () => {
    render(<MemberModeBanner active={true} />);
    const line = screen.getByText(MEMBER_MODE.BANNER);
    expect(line).toBeInTheDocument();
    expect(line).toHaveAttribute("title", MEMBER_MODE.CEILINGS);
    // The ceilings the doc and the banner must agree on — read as three claims,
    // not as one paragraph nobody checks.
    expect(MEMBER_MODE.CEILINGS).toMatch(/ROLE only/);
    expect(MEMBER_MODE.CEILINGS).toMatch(/already hold.*SSH key.*API token/);
    expect(MEMBER_MODE.CEILINGS).toMatch(/rolling upgrade/);
    // U-12: ceiling 1 in docs/OPERATIONS.md reads "Runs, workspaces and
    // secrets you created stay yours", and the code scopes all three the same
    // way — secretOwnerFromRequest hands a clamped admin their OWN namespace,
    // exactly as run and workspace ownership survive the clamp. Omitting the
    // third is the one of the three an admin is most likely to test blind.
    expect(MEMBER_MODE.CEILINGS).toMatch(/secrets/);
    // Ceiling 4 (0.7.5, field report finding 3): the plain mode still resolves
    // the admin's OWN model credential, and the tooltip has to say so — being
    // misled by exactly this is what the finding reports — and name the way out.
    expect(MEMBER_MODE.CEILINGS).toMatch(/Model access and ownership still resolve to you/);
    // …and it names the control that shows that state, not a doc section.
    expect(MEMBER_MODE.CEILINGS).toContain("View as a new member");
    expect(MEMBER_MODE.MENU_NEW).toContain("View as a new member");
  });

  // The no-credential posture: one prop, two sentences, and the sentence it
  // REPLACES is the one that would now be false.
  it("the no-credential preview states the AWS state and swaps ceiling 4", () => {
    render(<MemberModeBanner active={true} noCredential={true} />);
    const line = screen.getByText(MEMBER_MODE.BANNER_NEW);
    expect(line).toBeInTheDocument();
    expect(line).toHaveAttribute("title", MEMBER_MODE.CEILINGS_NEW);
    expect(screen.queryByText(MEMBER_MODE.BANNER)).not.toBeInTheDocument();
    expect(MEMBER_MODE.CEILINGS_NEW).toMatch(/hidden, not removed/);
    expect(MEMBER_MODE.CEILINGS_NEW).toMatch(/signing in is refused until you exit/);
    // The plain ceiling-4 sentence must NOT ride the variant: inside the preview
    // model access does not resolve to the admin, which is the whole point.
    expect(MEMBER_MODE.CEILINGS_NEW).not.toMatch(/Model access and ownership still resolve to you/);
    // Everything the two postures share is still there — the variant is a swap,
    // not a rewrite.
    expect(MEMBER_MODE.CEILINGS_NEW).toMatch(/ROLE only/);
    expect(MEMBER_MODE.CEILINGS_NEW).toMatch(/rolling upgrade/);
  });

  // W6-5. The control is offered to BOTH admin tiers (`eligible = operator ||
  // securityOperator`, and OPERATIONS.md says so), and both clamp to member —
  // which is why MEMBER_MODE.EXIT already names the mode rather than a tier to
  // go back to, and this file's own comment says that is why. The banner then
  // named a tier: a security_admin read "your admin role is paused" about a
  // role they do not hold, on the one surface that is unconditional and on
  // every screen — the same wrong-tier-sentence class SECURITY_ONLY_REASON was
  // added this release to close.
  it("the banner names no tier — it is offered to both admin tiers and both clamp to member", () => {
    expect(MEMBER_MODE.BANNER).not.toMatch(/\badmin\b/i);
    expect(MEMBER_MODE.BANNER).not.toMatch(/\bsecurity[ _-]?admin\b/i);
    // Still says WHAT is paused and for how long — tier-neutral, not vague.
    expect(MEMBER_MODE.BANNER).toMatch(/paused for this session/);
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
    await waitFor(() => expect(setMemberMode).toHaveBeenCalledWith(true, false));
    await waitFor(() => expect(onEntered).toHaveBeenCalled());
  });

  // The SECOND item (0.7.5). Both postures are offered at once because they
  // answer different questions, and the new one is the only way to reach the
  // state a per_user deployment's members are actually in on day one.
  it("offers BOTH postures, and the new one posts no_credential:true", async () => {
    const onEntered = vi.fn();
    renderItem({ meta: admin, onEntered });
    expect(screen.getByText(MEMBER_MODE.MENU)).toBeInTheDocument();
    await userEvent.click(screen.getByText(MEMBER_MODE.MENU_NEW));
    await waitFor(() => expect(setMemberMode).toHaveBeenCalledWith(true, true));
    await waitFor(() => expect(onEntered).toHaveBeenCalled());
  });

  // Mid-rolling-upgrade a 0.7.4 replica 400s the new posture (decodeStrict) and
  // still serves the plain one. The failure has to land on the item that failed,
  // or the admin reads "could not change member mode" on a control that works.
  it("a failed new-posture toggle marks only that item", async () => {
    setMemberMode.mockRejectedValue(new Error("400"));
    renderItem({ meta: admin });
    await userEvent.click(screen.getByText(MEMBER_MODE.MENU_NEW));
    expect(await screen.findByText(MEMBER_MODE.FAILED)).toBeInTheDocument();
    expect(screen.getByText(MEMBER_MODE.MENU)).toBeInTheDocument();
  });

  it("the new posture is hidden for a member too", () => {
    renderItem({ meta: member });
    expect(screen.queryByText(MEMBER_MODE.MENU_NEW)).not.toBeInTheDocument();
  });
});
