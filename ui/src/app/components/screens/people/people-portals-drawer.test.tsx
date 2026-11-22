/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The portals a portal person used, in their drawer. The real people client runs; only the HTTP call is
// answered by principal, so the request URL is part of what is checked.
import * as React from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { PersonSummary } from "../../../lib/types";

const wfetchMock = vi.fn();
vi.mock("../../../lib/api/core", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../../lib/api/core")>()),
  wfetch: (...a: unknown[]) => wfetchMock(...a),
}));
vi.mock("../../../lib/api/credentials", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../../lib/api/credentials")>()),
  credentials: { erase: vi.fn() },
}));
vi.mock("../../../lib/api/access", () => ({
  access: { getAccess: () => Promise.reject(new Error("sso off")) },
  AccessCollisionError: class extends Error {},
  AccessPostureFlipRequiredError: class extends Error {},
}));
vi.mock("../../../lib/api/scim", () => ({
  scim: { getStatus: () => Promise.resolve({ configured: false, last_token_slot: "", purge_after_seconds: 0, keep_workspaces: false, deactivated: [], pending: [], drives: [] }) },
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { OperatorProvider } from "../../wardyn/operator-context";
import { PeopleScreen } from "./people-screen";
import { PersonDrawer } from "./person-drawer";

function person(over: Partial<PersonSummary>): PersonSummary {
  return {
    principal: "sub-ana",
    email: "ana@example.com",
    issuer_kind: "oidc",
    pre_created: false,
    last_signed_in_at: new Date(Date.now() - 3_600_000).toISOString(),
    role: "admin",
    active_sessions: 0,
    api_tokens: 0,
    ssh_keys: 0,
    credentials: 0,
    active_runs: 0,
    ...over,
  };
}

const ISSUER = "https://login.example.org";
const SAM = person({ principal: "delegate:person:k1:sub-sam", email: "sam@example.com", role: "user", issuer: ISSUER });
const KIM = person({ principal: "delegate:person:k1:sub-kim", email: "kim@example.com", role: "user", issuer: ISSUER });
const ANA = person({});
const UNIT_ENG = "11111111-1111-4111-8111-111111111111";
const UNIT_FIN = "22222222-2222-4222-8222-222222222222";

const portalsOf: Record<string, unknown[]> = {
  [SAM.principal]: [
    { delegate: "d-a", name: "Portal A", run_in_under: UNIT_ENG },
    { delegate: "d-b", name: "Portal B", run_in_under: UNIT_FIN },
    { delegate: "d-old", name: "Portal Old", run_in_under: UNIT_ENG, revoked_at: new Date(Date.now() - 86_400_000).toISOString() },
  ],
  [KIM.principal]: [{ delegate: "d-c", name: "Portal C", run_in_under: UNIT_FIN }],
};

const json = (body: unknown) => Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }));

beforeEach(() => {
  wfetchMock.mockReset();
  wfetchMock.mockImplementation((path: string) => {
    if (path.startsWith("/people?")) return json({ people: [ANA, SAM, KIM] });
    const m = /^\/people\/([^/]+)\/portals$/.exec(path);
    if (m) {
      const principal = decodeURIComponent(m[1]);
      return json({ principal, state: "active", display_email: "", portals_used: portalsOf[principal] ?? [] });
    }
    if (/^\/people\/[^/]+\/tokens/.test(path)) return json([]);
    return Promise.reject(new Error(`unexpected request ${path}`));
  });
});

async function open(label: string) {
  const user = userEvent.setup();
  render(
    <MemoryRouter>
      <OperatorProvider operator securityOperator operatorResolved principal="admin@corp.example">
        <PeopleScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
  await user.click(await screen.findByRole("button", { name: label }));
  return within(await screen.findByRole("dialog"));
}

const portalRequests = () => wfetchMock.mock.calls.map((c) => c[0] as string).filter((p) => p.endsWith("/portals"));

describe("the portals a portal person used", () => {
  it("lists each portal with its assigned unit, and keeps a revoked one marked as history", async () => {
    const drawer = await open("sam@example.com");
    const group = within(await drawer.findByRole("group", { name: "Portals used" }));
    expect(await group.findByText("Portal A")).toBeInTheDocument();
    const a = group.getByText("Portal A").closest("li")!;
    const b = group.getByText("Portal B").closest("li")!;
    expect(within(a).getByText(`Unit ${UNIT_ENG}`)).toBeInTheDocument();
    expect(within(b).getByText(`Unit ${UNIT_FIN}`)).toBeInTheDocument();
    const old = group.getByText("Portal Old").closest("li")!;
    expect(within(old).getByText(/^Revoked /)).toBeInTheDocument();
    expect(within(a).queryByText(/^Revoked /)).toBeNull();
  });

  it("asks for this person's portals only, so another person on the same issuer stays out", async () => {
    const drawer = await open("sam@example.com");
    await drawer.findByText("Portal A");
    expect(portalRequests()).toEqual([`/people/${encodeURIComponent(SAM.principal)}/portals`]);
    expect(drawer.queryByText("Portal C")).toBeNull();
  });

  // Holds one person's portals read until released; every other request is answered as in beforeEach.
  function holdPortals(principal: string) {
    let release!: () => void;
    const held = new Promise<void>((r) => (release = r));
    const answer = wfetchMock.getMockImplementation()!;
    wfetchMock.mockImplementation((path: string) => (path === `/people/${encodeURIComponent(principal)}/portals` ? held.then(() => answer(path)) : answer(path)));
    return release;
  }

  it("shows no 'no portal' line while the read is pending, then the answer", async () => {
    const release = holdPortals(SAM.principal);
    const drawer = await open("sam@example.com");
    await waitFor(() => expect(portalRequests()).toHaveLength(1));
    expect(drawer.queryByText("No portal used yet")).toBeNull();
    expect(drawer.queryByText("Portal A")).toBeNull();
    release();
    expect(await drawer.findByText("Portal A")).toBeInTheDocument();
  });

  it("shows neither an empty line nor the last person's list, in any committed render, for the next person's pending read", async () => {
    // What the page shows after every commit, read before the effects that start the next read have run.
    const commits: string[] = [];
    function Probe() {
      React.useLayoutEffect(() => {
        commits.push(document.body.textContent ?? "");
      });
      return null;
    }
    const tree = (person: PersonSummary) => (
      <MemoryRouter>
        <OperatorProvider operator securityOperator operatorResolved principal="admin@corp.example">
          <PersonDrawer person={person} onClose={() => {}} onChanged={() => {}} />
          <Probe />
        </OperatorProvider>
      </MemoryRouter>
    );
    const { rerender } = render(tree(SAM));
    expect(await within(await screen.findByRole("dialog")).findByText("Portal A")).toBeInTheDocument();
    const release = holdPortals(KIM.principal);
    const before = commits.length;
    rerender(tree(KIM));
    const drawer = within(await screen.findByRole("dialog"));
    await waitFor(() => expect(portalRequests()).toHaveLength(2));
    expect(commits.length).toBeGreaterThan(before);
    commits.slice(before).forEach((text) => {
      expect(text).not.toContain("No portal used yet");
      expect(text).not.toContain("Portal A");
    });
    release();
    expect(await drawer.findByText("Portal C")).toBeInTheDocument();
  });

  it("makes no portals request and shows no portals section for a workforce person", async () => {
    const drawer = await open("ana@example.com");
    expect(drawer.getByText("Sessions")).toBeInTheDocument();
    expect(drawer.queryByText("Portals used")).toBeNull();
    expect(portalRequests()).toEqual([]);
  });
});
