/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { NOTHING, ToolCard, type ToolAccess } from "./tool-card";

afterEach(cleanup);

const ACCESS: ToolAccess = {
  network: [{ host: "one.example.com" }, { host: "two.example.com" }, { host: "three.example.com" }, { host: "four.example.com" }],
  credentials: [{ name: "Team token", delivery: NR_TOOLS.DELIVERY_PROXY }],
  credentialNote: "A note under the credentials.",
  files: [],
  other: "Some other line",
};

function show(access: ToolAccess | undefined) {
  render(
    <ul>
      <ToolCard icon="xx" title="Sample" body="What it is" access={access}>
        {(details) => <>{details ?? <span>no details</span>}<button type="button">Add</button></>}
      </ToolCard>
    </ul>,
  );
}
const breakdown = () => screen.getByRole("list").querySelector("dl") as HTMLElement;

describe("tool card: what it brings", () => {
  it("lists Network, Credentials, Files and drives and Other, in that order, one row each", () => {
    show(ACCESS);
    const labels = [...breakdown().querySelectorAll("dt")].map((n) => n.textContent);
    expect(labels).toEqual([NR_TOOLS.NETWORK_TITLE, NR_TOOLS.CREDENTIALS_TITLE, NR_TOOLS.DETAIL_FILES, NR_TOOLS.DETAIL_OTHER]);
    expect(breakdown().querySelectorAll("dd")).toHaveLength(4);
  });

  it("names the breakdown for a screen reader", () => {
    show(ACCESS);
    expect(within(screen.getByRole("list")).getByText(NR_TOOLS.NETWORK_TITLE).closest("dl")?.getAttribute("aria-label")).toBe(NR_TOOLS.BRINGS_FOR("Sample"));
  });

  it("ends a long Network row with how many more, and shows the credential with its delivery chip", () => {
    show(ACCESS);
    const dd = [...breakdown().querySelectorAll("dd")];
    expect(dd[0].textContent).toBe(`one.example.comtwo.example.com${NR_TOOLS.MORE(2)}`);
    expect(dd[1].textContent).toBe(`Team token${NR_TOOLS.DELIVERY_PROXY}`);
  });

  it("shows an unnamed credential's delivery once, however many credentials share it", () => {
    show({ ...ACCESS, credentials: [{ delivery: NR_TOOLS.DELIVERY_PROXY }, { delivery: NR_TOOLS.DELIVERY_PROXY }, { delivery: NR_TOOLS.DELIVERY_ENV }] });
    expect([...breakdown().querySelectorAll("dd")][1].textContent).toBe(`${NR_TOOLS.DELIVERY_PROXY}${NR_TOOLS.DELIVERY_ENV}`);
  });

  it("says None for an empty row", () => {
    show(ACCESS);
    const dd = [...breakdown().querySelectorAll("dd")];
    expect(dd[2].textContent).toBe(NR_TOOLS.DETAIL_NONE);
    show({ ...ACCESS, network: [], credentials: [], other: "" });
    const empty = [...document.querySelectorAll("dl")[1].querySelectorAll("dd")].map((n) => n.textContent);
    expect(empty).toEqual([NR_TOOLS.DETAIL_NONE, NR_TOOLS.DETAIL_NONE, NR_TOOLS.DETAIL_NONE, NR_TOOLS.DETAIL_NONE]);
  });

  it("shows no breakdown and no Details on a card with no access", () => {
    show(undefined);
    expect(document.querySelector("dl")).toBeNull();
    expect(screen.queryByRole("button", { name: NR_TOOLS.DETAILS_FOR("Sample") })).toBeNull();
    expect(screen.getByText("no details")).toBeTruthy();
  });

  it("offers no Details when every row is None", () => {
    show(NOTHING);
    expect(screen.queryByRole("button", { name: NR_TOOLS.DETAILS_FOR("Sample") })).toBeNull();
    expect([...breakdown().querySelectorAll("dd")].map((n) => n.textContent)).toEqual(Array(4).fill(NR_TOOLS.DETAIL_NONE));
  });
});

describe("tool card: Details", () => {
  it("is named for the tool and opens every host and credential with the same four headings", async () => {
    show(ACCESS);
    await userEvent.click(screen.getByRole("button", { name: NR_TOOLS.DETAILS_FOR("Sample") }));
    const dialog = within(await screen.findByRole("dialog", { name: "Sample" }));
    expect(dialog.getAllByRole("heading", { level: 3 }).map((h) => h.textContent)).toEqual([NR_TOOLS.NETWORK_TITLE, NR_TOOLS.CREDENTIALS_TITLE, NR_TOOLS.DETAIL_FILES, NR_TOOLS.DETAIL_OTHER]);
    for (const h of ["one.example.com", "two.example.com", "three.example.com", "four.example.com"]) expect(dialog.getByText(h)).toBeTruthy();
    expect(dialog.getAllByText(NR_TOOLS.NET_ALLOWED)).toHaveLength(4);
    expect(dialog.getByText("A note under the credentials.")).toBeTruthy();
    expect(dialog.getByText("Some other line")).toBeTruthy();
    expect(dialog.getByText("What it is")).toBeTruthy();
  });

  it("keeps each short part of the Other line whole, on the card and in Details, and lets a list wrap", async () => {
    const namespaces = NR_TOOLS.KUBE_NAMESPACES("web, jobs, billing-reports, data-pipelines, staging-preview");
    const parts = [namespaces, NR_TOOLS.KUBE_READ_ONLY, NR_TOOLS.KUBE_NO_EXEC];
    show({ ...ACCESS, other: parts.reduce((a, b) => NR_SHARED.JOIN(a, b)) });
    const nowrap = (root: Element) => [...root.querySelectorAll(".whitespace-nowrap")].map((n) => n.textContent);
    const wraps = (root: Element) => [...root.querySelectorAll(".break-words")].map((n) => n.textContent);
    const other = [...breakdown().querySelectorAll("dd")][3];
    expect(nowrap(other)).toEqual([NR_TOOLS.KUBE_READ_ONLY, NR_TOOLS.KUBE_NO_EXEC]);
    expect(wraps(other)).toEqual([namespaces]);
    expect(other.textContent).toBe(parts.join(NR_SHARED.JOIN("", "")));
    await userEvent.click(screen.getByRole("button", { name: NR_TOOLS.DETAILS_FOR("Sample") }));
    const dialog = await screen.findByRole("dialog", { name: "Sample" });
    const section = within(dialog).getByRole("heading", { name: NR_TOOLS.DETAIL_OTHER }).closest("section")!;
    expect(nowrap(section)).toEqual([NR_TOOLS.KUBE_READ_ONLY, NR_TOOLS.KUBE_NO_EXEC]);
    expect(wraps(section)).toEqual([namespaces]);
  });

  it("closes from Close", async () => {
    show(ACCESS);
    await userEvent.click(screen.getByRole("button", { name: NR_TOOLS.DETAILS_FOR("Sample") }));
    const dialog = await screen.findByRole("dialog", { name: "Sample" });
    await userEvent.click(within(dialog).getAllByRole("button", { name: "Close" })[0]);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
