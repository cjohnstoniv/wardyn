/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// DirectoryCombobox (0.7 §I) — the "who" fields' typeahead. Every assertion
// reads its expected string from governance-copy.ts's DIRECTORY (§7.9) rather
// than retyping it, the convention governance-screen.test.tsx and
// access-panel.test.tsx already follow.
//
// Four of these are DECISIONS, not rendering:
//   1. ABSENT MODE — no directory configured means this IS a plain text input:
//      free text still works and NOTHING else is on the screen. §7.9 freezes no
//      string for that state because the state has no copy, and the deployment
//      that never turns the connector on is the common one.
//   2. absent (a distinct 503, silent forever) and broken (LOOKUP_FAILED, and
//      the typed text survives) are DIFFERENT answers.
//   3. a pick inserts ClaimValue, never the DisplayName it rendered — the whole
//      reason directory.Entry carries two fields.
//   4. a picked GROUP says so, because that is the one case where the stored
//      value (an object id) is not the thing the admin read.
import { describe, it, expect, vi, beforeEach } from "vitest";
import * as React from "react";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const searchMock = vi.fn();
vi.mock("../../lib/api/directory", async () => {
  const actual = await vi.importActual<typeof import("../../lib/api/directory")>("../../lib/api/directory");
  return { ...actual, directory: { search: (...a: unknown[]) => searchMock(...a) } };
});

import { HttpError } from "../../lib/api/core";
import type { DirectoryEntry, DirectorySearchKind } from "../../lib/api/directory";
import { DIRECTORY } from "../../lib/governance-copy";
import { PERM } from "../../lib/permissions-copy";
import { DirectoryCombobox } from "./directory-combobox";

const ALICE: DirectoryEntry = {
  display_name: "Alice Ng",
  claim_value: "alice@corp.example",
  kind: "user",
  detail: "alice.ng@corp.example",
};

const PLATFORM: DirectoryEntry = {
  display_name: "Platform Engineering",
  claim_value: "8f3c1a2b-0000-4d1e-9f00-abcdef012345",
  kind: "group",
  detail: "group · 8f3c1a2b",
};

const row = (e: DirectoryEntry) => DIRECTORY.SUGGEST_ROW(e.display_name, e.detail ?? "");

// Controlled by its caller in both real call sites, so the harness holds the
// value exactly as assignments.tsx / access-panel.tsx do.
function Harness({ kind }: { kind?: DirectorySearchKind }) {
  const [value, setValue] = React.useState("");
  return <DirectoryCombobox label={PERM.FIELD_WHO} value={value} onChange={setValue} kind={kind} />;
}

const field = () => screen.getByRole("textbox", { name: PERM.FIELD_WHO });

// Every DIRECTORY string that has a rendering, so "no chrome" can be asserted
// as an absence rather than as three separate hopes.
const noticeStrings = [
  DIRECTORY.SEARCHING,
  DIRECTORY.NO_MATCHES,
  DIRECTORY.LOOKUP_FAILED,
  DIRECTORY.GROUP_VALUE_NOTE,
  DIRECTORY.MIN_CHARS_HINT,
];

beforeEach(() => {
  searchMock.mockReset();
});

describe("DirectoryCombobox — absent mode", () => {
  // THE spine: a "who" field must stay free text forever, so the deployment
  // with no connector sees exactly what it saw before this feature existed.
  it("no directory configured: free text, and not one pixel of chrome", async () => {
    // Deferred, and landed INSIDE act: asserting an absence the moment the
    // request goes out would pass on a control that renders chrome one tick
    // later. The answer has to be fully handled before "nothing happened".
    let land: (rows: DirectoryEntry[] | null) => void = () => {};
    searchMock.mockReturnValue(
      new Promise<DirectoryEntry[] | null>((resolve) => {
        land = resolve;
      }),
    );
    render(<Harness kind="group" />);

    await userEvent.type(field(), "eng-team");
    await waitFor(() => expect(searchMock).toHaveBeenCalled());
    await act(async () => {
      land(null); // the endpoint's distinct 503, as the client resolves it
    });

    expect(field()).toHaveValue("eng-team");
    for (const s of noticeStrings) expect(screen.queryByText(s)).not.toBeInTheDocument();
    expect(screen.queryAllByRole("button")).toEqual([]);
    expect(screen.queryByRole("list")).not.toBeInTheDocument();
  });

  it("…and having answered once, it is not asked again for the rest of the session", async () => {
    searchMock.mockResolvedValue(null);
    render(<Harness kind="group" />);

    await userEvent.type(field(), "eng");
    await waitFor(() => expect(searchMock).toHaveBeenCalledTimes(1));
    await userEvent.type(field(), "-team");
    await new Promise((r) => setTimeout(r, 400)); // past the debounce
    expect(searchMock).toHaveBeenCalledTimes(1);
  });

  it("below the minimum query length nothing is asked at all", async () => {
    searchMock.mockResolvedValue([]);
    render(<Harness />);

    await userEvent.type(field(), "a");
    await new Promise((r) => setTimeout(r, 400));
    expect(searchMock).not.toHaveBeenCalled();
  });
});

describe("DirectoryCombobox — searching", () => {
  it("says it is searching, then renders the rows, and a pick inserts the CLAIM VALUE", async () => {
    let land: (rows: DirectoryEntry[]) => void = () => {};
    searchMock.mockReturnValue(
      new Promise<DirectoryEntry[]>((resolve) => {
        land = resolve;
      }),
    );
    render(<Harness kind="user" />);

    await userEvent.type(field(), "ali");
    // §O Q7: "searching…" and nothing else — no min-chars hint alongside it.
    expect(await screen.findByText(DIRECTORY.SEARCHING)).toBeInTheDocument();
    expect(screen.queryByText(DIRECTORY.MIN_CHARS_HINT)).not.toBeInTheDocument();

    await act(async () => {
      land([ALICE]);
    });
    expect(screen.queryByText(DIRECTORY.SEARCHING)).not.toBeInTheDocument();
    expect(searchMock).toHaveBeenCalledWith("ali", "user");
    // The detail is the disambiguator — two people share a display name — and
    // §7.9 renders that half mono.
    expect(screen.getByText(ALICE.detail!).className).toContain("font-mono");

    await userEvent.click(screen.getByRole("button", { name: row(ALICE) }));
    expect(field()).toHaveValue(ALICE.claim_value);
    expect(field()).not.toHaveValue(ALICE.display_name);
    // The insert must not re-open the list by searching for what it just wrote.
    expect(searchMock).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("button", { name: row(ALICE) })).not.toBeInTheDocument();
  });

  it("a picked GROUP says what actually got stored", async () => {
    searchMock.mockResolvedValue([PLATFORM]);
    render(<Harness kind="group" />);

    await userEvent.type(field(), "plat");
    await userEvent.click(await screen.findByRole("button", { name: row(PLATFORM) }));

    expect(field()).toHaveValue(PLATFORM.claim_value);
    expect(screen.getByText(DIRECTORY.GROUP_PICKED_CHIP(PLATFORM.display_name))).toBeInTheDocument();
    expect(screen.getByText(DIRECTORY.GROUP_VALUE_NOTE)).toBeInTheDocument();

    // Typing invalidates the pick: the field no longer holds that group's id.
    await userEvent.type(field(), "x");
    expect(screen.queryByText(DIRECTORY.GROUP_VALUE_NOTE)).not.toBeInTheDocument();
  });

  it("a user pick needs no such note — its display name IS its claim value's twin", async () => {
    searchMock.mockResolvedValue([ALICE]);
    render(<Harness kind="user" />);

    await userEvent.type(field(), "ali");
    await userEvent.click(await screen.findByRole("button", { name: row(ALICE) }));
    expect(screen.queryByText(DIRECTORY.GROUP_VALUE_NOTE)).not.toBeInTheDocument();
  });

  it("an empty result is NO_MATCHES, not silence", async () => {
    searchMock.mockResolvedValue([]);
    render(<Harness />);

    await userEvent.type(field(), "nobody");
    expect(await screen.findByText(DIRECTORY.NO_MATCHES)).toBeInTheDocument();
    expect(screen.queryAllByRole("button")).toEqual([]);
  });
});

describe("DirectoryCombobox — broken is not absent", () => {
  it("a real failure is told, and the typed text is left usable", async () => {
    searchMock.mockRejectedValue(new HttpError(500, "directory search failed"));
    render(<Harness kind="group" />);

    await userEvent.type(field(), "eng-team");
    expect(await screen.findByText(DIRECTORY.LOOKUP_FAILED)).toBeInTheDocument();
    // The whole point of keeping the control free text: the admin types the
    // value themselves, which is exactly what LOOKUP_FAILED tells them to do.
    expect(field()).toHaveValue("eng-team");
    expect(field()).toBeEnabled();
  });
});
