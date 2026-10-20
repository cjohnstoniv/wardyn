/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A file of its own: switchView's module state (the tab is mid-switch) would
// otherwise leak in from view-switch.test.tsx and silence the re-sync.
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { health, type Me } from "../../lib/api/health";
import { wfetch } from "../../lib/api/core";
import { useViewResync } from "./view-switch";
import type { ViewAccess } from "./console-view";

afterEach(() => {
  vi.restoreAllMocks();
});

function setup(access: ViewAccess, loadedMemberMode: boolean, path: string, memberMode: boolean) {
  const assign = vi.fn();
  vi.spyOn(window, "location", "get").mockReturnValue({ ...window.location, pathname: path, assign });
  const whoami = vi.spyOn(health, "whoami").mockResolvedValue({ user_view: memberMode } as Me);
  renderHook(() => useViewResync(access, loadedMemberMode));
  return { assign, whoami };
}

describe("useViewResync (§2.4)", () => {
  it("on focus, a session now in the User view reloads this Admin-view tab into the twin", async () => {
    const { assign } = setup("session-admin", false, "/admin/runs/abc", true);
    act(() => void window.dispatchEvent(new Event("focus")));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/runs/abc"));
  });

  it("a page with no twin lands on the other view's home", async () => {
    const { assign } = setup("session-user", true, "/runs/new", false);
    act(() => void window.dispatchEvent(new Event("focus")));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/admin"));
  });

  it("any 403 re-reads /me at once", async () => {
    const { assign, whoami } = setup("session-admin", false, "/admin/policies", true);
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("{}", { status: 403 }));
    await act(() => wfetch("/policies"));
    await waitFor(() => expect(whoami).toHaveBeenCalled());
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/runs"));
  });

  it("a session still in this tab's view changes nothing", async () => {
    const { assign, whoami } = setup("session-admin", false, "/admin/runs", false);
    act(() => void window.dispatchEvent(new Event("focus")));
    await waitFor(() => expect(whoami).toHaveBeenCalled());
    expect(assign).not.toHaveBeenCalled();
  });

  it("neg: a single-operator install has no session to follow", () => {
    const { whoami } = setup("url", false, "/runs", true);
    act(() => void window.dispatchEvent(new Event("focus")));
    expect(whoami).not.toHaveBeenCalled();
  });

  // #2618: a type or posture switched in another tab changes what a launch from this one is governed as.
  describe("inside the User view, a switched user type or no-credential posture", () => {
    const loaded = { userTypeId: "standard", noCredential: false };
    function inView(me: Partial<Me>, posture: Parameters<typeof useViewResync>[2] = loaded) {
      const assign = vi.fn();
      vi.spyOn(window, "location", "get").mockReturnValue({ ...window.location, pathname: "/runs", assign });
      const whoami = vi.spyOn(health, "whoami").mockResolvedValue({ user_view: true, user_type: { id: "standard", name: "Standard" }, ...me } as Me);
      renderHook(() => useViewResync("session-admin", true, posture));
      return { assign, whoami };
    }

    it("a different user type reloads this tab", async () => {
      const { assign } = inView({ user_type: { id: "contractor", name: "Contractor" } });
      act(() => void window.dispatchEvent(new Event("focus")));
      await waitFor(() => expect(assign).toHaveBeenCalledWith("/runs"));
    });

    it("a switched no-credential posture reloads this tab", async () => {
      const { assign } = inView({ user_view_no_credential: true });
      act(() => void window.dispatchEvent(new Event("focus")));
      await waitFor(() => expect(assign).toHaveBeenCalledWith("/runs"));
    });

    it("the same type and posture change nothing", async () => {
      const same = inView({});
      act(() => void window.dispatchEvent(new Event("focus")));
      await waitFor(() => expect(same.whoami).toHaveBeenCalled());
      expect(same.assign).not.toHaveBeenCalled();
    });

    it("a page whose /me had not answered when it loaded has nothing to compare, and changes nothing", async () => {
      const unknown = inView({ user_type: { id: "contractor", name: "Contractor" } }, null);
      act(() => void window.dispatchEvent(new Event("focus")));
      await waitFor(() => expect(unknown.whoami).toHaveBeenCalled());
      expect(unknown.assign).not.toHaveBeenCalled();
    });
  });
});
