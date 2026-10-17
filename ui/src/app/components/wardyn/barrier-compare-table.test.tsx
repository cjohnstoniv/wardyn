/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// U16 — the barrier comparison is ONE radiogroup in the accessibility tree at
// every width. vitest does not compile Tailwind, so each test installs the two
// display rules the component's classes rely on, in Tailwind's source order.
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { isInaccessible, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { BarrierCompareTable, type BarrierOption } from "./barrier-compare-table";
import { NR_RUN } from "./copy/new-run/run";
import { CAVEAT_PREFIX, CC_MATRIX_ROWS, CC_META } from "./cc-meta";

const NARROW = ".hidden{display:none}";
const WIDE = ".hidden{display:none}.min-\\[521px\\]\\:block{display:block}.min-\\[521px\\]\\:hidden{display:none}";

let sheet: HTMLStyleElement | undefined;
function viewport(css: string) {
  sheet = document.createElement("style");
  sheet.textContent = css;
  document.head.append(sheet);
}
afterEach(() => sheet?.remove());

// Text queries ignore CSS display, so keep only what the accessibility tree holds.
const live = (els: HTMLElement[]) => els.filter((el) => !isInaccessible(el));

const OPTIONS: BarrierOption[] = [
  { cc: "CC1", available: false, reason: "Your organisation requires at least Wall." },
  { cc: "CC2", available: true },
  { cc: "CC3", available: true },
];

function renderTable(props: Partial<React.ComponentProps<typeof BarrierCompareTable>> = {}) {
  const onChange = vi.fn();
  render(
    <BarrierCompareTable
      label="Barrier"
      options={OPTIONS}
      value="CC2"
      onChange={onChange}
      showWhere
      {...props}
    />,
  );
  return onChange;
}

// Real state, so a re-render after a click shows what the browser would: the
// table and card radios must not share one native group.
function Stateful() {
  const [value, setValue] = React.useState<"CC1" | "CC2" | "CC3">("CC2");
  return <BarrierCompareTable label="Barrier" options={OPTIONS} value={value} onChange={setValue} showWhere />;
}

describe.each([
  ["521 px and wider", WIDE],
  ["below 521 px", NARROW],
])("BarrierCompareTable %s", (_name, css) => {
  it("has one radiogroup holding one live radio set", () => {
    viewport(css);
    renderTable();
    expect(screen.getAllByRole("radiogroup")).toHaveLength(1);
    const radios = screen.getAllByRole("radio");
    expect(radios.map((r) => r.getAttribute("value"))).toEqual(["CC1", "CC2", "CC3"]);
    expect(screen.getByRole("radio", { name: "Wall" })).toBeChecked();
  });

  it("describes a disabled barrier by its visible reason", () => {
    viewport(css);
    renderTable();
    const fence = screen.getByRole("radio", { name: "Fence" });
    expect(fence).toBeDisabled();
    expect(fence).toHaveAccessibleDescription("Your organisation requires at least Wall.");
  });

  it("reads each caveat as Except: plus the residual risk", () => {
    viewport(css);
    renderTable();
    // Wall caveats the kernel and break-in rows, Vault the break-in row.
    const caveats = live(screen.getAllByText(new RegExp(`^${CAVEAT_PREFIX}`)));
    expect(caveats).toHaveLength(3);
    expect(caveats[0]).toHaveTextContent(`${CAVEAT_PREFIX} ${CC_META.CC2.doesntProtect}`);
    expect(caveats[2]).toHaveTextContent(`${CAVEAT_PREFIX} ${CC_META.CC3.doesntProtect}`);
  });

  it("selects an available barrier and ignores a disabled one", async () => {
    viewport(css);
    const onChange = renderTable();
    await userEvent.click(screen.getByRole("radio", { name: "Vault" }));
    expect(onChange).toHaveBeenCalledExactlyOnceWith("CC3");
    await userEvent.click(screen.getByRole("radio", { name: "Fence" }));
    expect(onChange).toHaveBeenCalledTimes(1);
  });

  it("shows the picked barrier as the one checked live radio after a click", async () => {
    viewport(css);
    render(<Stateful />);
    await userEvent.click(screen.getByRole("radio", { name: "Vault" }));
    expect(screen.getByRole("radio", { name: "Vault" })).toBeChecked();
    expect(screen.getByRole("radio", { name: "Wall" })).not.toBeChecked();
    await userEvent.click(screen.getByRole("radio", { name: "Wall" }));
    expect(screen.getByRole("radio", { name: "Wall" })).toBeChecked();
    expect(screen.getByRole("radio", { name: "Vault" })).not.toBeChecked();
  });

  it("opens an info popover per barrier", async () => {
    viewport(css);
    renderTable();
    await userEvent.click(screen.getByRole("button", { name: "About Vault" }));
    expect(screen.getByText(CC_META.CC3.mechanism)).toBeInTheDocument();
    // FR-0119: the three headings are the canon strings.
    for (const head of [NR_RUN.PROTECTS_HEAD, NR_RUN.NOT_PROTECTS_HEAD, NR_RUN.HOW_HEAD]) expect(screen.getByText(head)).toBeInTheDocument();
  });

  it("shows the badges and hides Where it runs when asked", () => {
    viewport(css);
    renderTable({ defaultBadge: { cc: "CC2", text: "Default" }, badges: { CC3: "Experimental" }, showWhere: false });
    expect(live(screen.getAllByText("Default"))).toHaveLength(1);
    expect(live(screen.getAllByText("Experimental"))).toHaveLength(1);
    expect(screen.queryByText("Any host")).not.toBeInTheDocument();
  });
});

describe("BarrierCompareTable layouts", () => {
  it("is a real table with column and row headers at 521 px and wider", () => {
    viewport(WIDE);
    renderTable();
    const table = screen.getByRole("table", { name: "Compare barriers" });
    expect(within(table).getAllByRole("columnheader")).toHaveLength(4);
    expect(within(table).getAllByRole("rowheader").map((h) => h.textContent)).toEqual([
      ...CC_MATRIX_ROWS.map((r) => r.label),
      "Where it runs",
    ]);
    expect(within(table).getAllByRole("radio")).toHaveLength(3);
  });

  it("is a self-labelled card per barrier below 521 px, with no table", () => {
    viewport(NARROW);
    renderTable();
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
    for (const row of CC_MATRIX_ROWS) expect(live(screen.getAllByText(row.label))).toHaveLength(3);
    expect(live(screen.getAllByText("Where it runs"))).toHaveLength(3);
  });
});
