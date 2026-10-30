/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MY_COMPONENTS, ORG_KUBECTL } from "../../../../../lib/new-run-fixtures/scenarios/catalogue";
import { SCENARIO_ACCESS } from "../../../../../lib/new-run-fixtures/scenarios/tools";
import type { AccessFacts, AccessHost } from "../../../../../lib/types/new-run-access";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { NR_WALLS } from "../../../../wardyn/copy/personal-walls";
import { initialDraft } from "../../draft/run-intent";
import type { ReviewPreflight } from "../review-seams";
import { AccessGroup } from "./access-group";

const getWalls = vi.hoisted(() => vi.fn());
vi.mock("../../../../../lib/api/walls", () => ({ walls: { get: getWalls } }));
vi.mock("../../../../../lib/api/components", () => ({ components: { mine: () => Promise.resolve({ ...MY_COMPONENTS, org: [ORG_KUBECTL] }) } }));

afterEach(() => {
  cleanup();
  getWalls.mockReset();
});
beforeEach(() => getWalls.mockRejectedValue(new Error("no walls")));

const access: AccessFacts = {
  ...SCENARIO_ACCESS.member,
  credentials: [{ id: `${ORG_KUBECTL.id}:kube_api`, row: ORG_KUBECTL.id, kind: "kube_api", owner: "shared", delivery: "proxy", after_end: "gone" }],
};

describe("Review: Access", () => {
  it("lists a kubectl cluster's service account under the hosts once opened", async () => {
    render(<AccessGroup draft={initialDraft("alice")} pre={{ access } as unknown as ReviewPreflight} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} />);
    expect(screen.queryByText(NR_REVIEW.POLICY_CREDENTIALS)).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_ACCESS}` }));
    expect(await screen.findByText(NR_TOOLS.KUBE_CREDENTIAL("staging"))).toBeTruthy();
  });

  it("lists and counts the cluster's API server the server adds for a kubectl row, named for the row", async () => {
    // The shape the server's preview gives a kubectl row's host: injected, brought by the row, its source the row.
    const kubeHost: AccessHost = {
      host: "api.staging.example.com", via: "injected", rows: [ORG_KUBECTL.id], verdict: "allowed",
      source: { kind: "component", id: ORG_KUBECTL.id, name: ORG_KUBECTL.name },
    };
    const withHost: AccessFacts = { ...access, hosts: [...access.hosts, kubeHost] };
    render(<AccessGroup draft={initialDraft("alice")} pre={{ access: withHost } as unknown as ReviewPreflight} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} />);
    expect(screen.getByText(NR_REVIEW.POLICY_HOSTS(2))).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_ACCESS}` }));
    const group = screen.getByRole("group", { name: NR_REVIEW.GROUP_ACCESS });
    const row = within(group).getByText(kubeHost.host).parentElement as HTMLElement;
    expect(within(row).getByText(ORG_KUBECTL.name)).toBeTruthy();
  });

  // T4-b: a run that reaches only its listed sites reads as the board draws it, and a host the person's walls cut gets the note.
  describe("a run that reaches only the listed sites", () => {
    const host = (name: string, verdict: AccessHost["verdict"]): AccessHost => ({ host: name, via: "requested", rows: [], verdict, source: { kind: "policy", id: "p", name: "p" } });
    const closed: AccessFacts = {
      ...access,
      hosts: [host("api.example.com", "allowed"), host("packages.example.com", "allowed"), host("git.example.com", "allowed"), host("pypi.org", "not_allowed")],
      unlisted: { mode: "always_deny", allow_all: false, decider: "you" },
    };
    const show = (facts: AccessFacts, onLeftOut?: (drawn: boolean) => void) =>
      render(<AccessGroup draft={initialDraft("alice")} pre={{ access: facts } as unknown as ReviewPreflight} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} onLeftOut={onLeftOut} />);

    it("says how many sites and that anything else is refused, with the walls note for a host they cut", async () => {
      getWalls.mockResolvedValue({ spec: { denied_domains: ["pypi.org"] } });
      show(closed);
      const note = await screen.findByTestId("walls-leave-out");
      expect(note).toHaveTextContent("Your walls leave out pypi.org.");
      expect(screen.getByText(NR_WALLS.REVIEW_SITES(4))).toBeTruthy();
      expect(screen.queryByText(NR_REVIEW.POLICY_HOSTS(4))).toBeNull();
    });

    it("tells Review whether a leave-out line is drawn, so the walls' own warning stays when none is", async () => {
      // pypi.org is a base-policy host the walls cut but the post-walls answer does not carry: no line, so the warning stays.
      getWalls.mockResolvedValue({ spec: { denied_domains: ["pypi.org"] } });
      const onLeftOut = vi.fn();
      show({ ...closed, hosts: closed.hosts.filter((h) => h.host !== "pypi.org") }, onLeftOut);
      await vi.waitFor(() => expect(getWalls).toHaveBeenCalled());
      await Promise.resolve();
      expect(screen.queryByTestId("walls-leave-out")).toBeNull();
      expect(onLeftOut).not.toHaveBeenCalledWith(true);
      cleanup();
      const drawn = vi.fn();
      show(closed, drawn);
      await screen.findByTestId("walls-leave-out");
      expect(drawn).toHaveBeenLastCalledWith(true);
    });

    it("draws no note when the walls cut nothing, and keeps the plain count when anything else may be asked for", async () => {
      getWalls.mockResolvedValue({ spec: {} });
      show({ ...closed, unlisted: { mode: "deny_with_review", allow_all: false, decider: "you" } });
      expect(await screen.findByText(NR_REVIEW.POLICY_HOSTS(4))).toBeTruthy();
      expect(screen.queryByTestId("walls-leave-out")).toBeNull();
    });
  });
});
