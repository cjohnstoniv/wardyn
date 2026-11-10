/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The extra header rows: add, remove, the cap at eight, and a row's own value placeholder.
import * as React from "react";
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { setField } from "../../../../test/set-field";
import { PROVIDER_EDITOR as E } from "../../../lib/model-providers-copy";
import type { HeaderDraft } from "./model-provider-draft";
import { ExtraHeaders } from "./model-provider-extra-headers";

function Harness({ start = [], hint }: { start?: HeaderDraft[]; hint?: string }) {
  const [rows, setRows] = React.useState<HeaderDraft[]>(start);
  return (
    <>
      <ExtraHeaders rows={rows} onChange={setRows} valueHint={hint} />
      <output data-testid="rows">{JSON.stringify(rows)}</output>
    </>
  );
}
const rows = () => JSON.parse(screen.getByTestId("rows").textContent ?? "[]") as HeaderDraft[];
const eight = (): HeaderDraft[] => Array.from({ length: 8 }, (_, i) => ({ name: `x-h-${i}`, value: `v${i}` }));

describe("ExtraHeaders", () => {
  it("starts empty: the label, the hint and an enabled Add header", () => {
    render(<Harness />);
    expect(screen.getByText(E.HEADERS_LABEL)).toBeInTheDocument();
    expect(screen.getByText(E.HEADERS_HINT)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: E.HEADERS_ADD })).toBeEnabled();
    expect(screen.queryByPlaceholderText(E.HEADER_NAME_PLACEHOLDER)).toBeNull();
    expect(screen.queryByText(E.HEADERS_CAP)).toBeNull();
  });

  it("adds a row, takes a name and a value, and removes it again", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.click(screen.getByRole("button", { name: E.HEADERS_ADD }));
    setField(screen.getByPlaceholderText(E.HEADER_NAME_PLACEHOLDER), "x-team");
    setField(screen.getByPlaceholderText(E.HEADER_VALUE_PLACEHOLDER), "blue");
    expect(rows()).toEqual([{ name: "x-team", value: "blue" }]);

    await user.click(screen.getByRole("button", { name: E.HEADERS_ADD }));
    expect(rows()).toHaveLength(2);
    await user.click(screen.getAllByRole("button", { name: E.HEADERS_REMOVE })[0]);
    expect(rows()).toEqual([{ name: "", value: "" }]);
  });

  it("names each row's name and value fields by their position", () => {
    render(<Harness start={[{ name: "a", value: "1" }, { name: "b", value: "2" }]} />);
    expect(screen.getByRole("textbox", { name: "Header 1 name" })).toHaveValue("a");
    expect(screen.getByRole("textbox", { name: "Header 2 value" })).toHaveValue("2");
  });

  it("at eight rows Add header is disabled, with At most 8 headers. beside it; removing one lifts it", async () => {
    const user = userEvent.setup();
    render(<Harness start={eight()} />);
    expect(screen.getByRole("button", { name: E.HEADERS_ADD })).toBeDisabled();
    expect(screen.getByText("At most 8 headers.")).toBeInTheDocument();
    expect(screen.getByText(E.HEADERS_CAP)).toBeInTheDocument();
    await user.click(screen.getAllByRole("button", { name: E.HEADERS_REMOVE })[3]);
    expect(screen.getByRole("button", { name: E.HEADERS_ADD })).toBeEnabled();
    expect(screen.queryByText(E.HEADERS_CAP)).toBeNull();
  });

  it("a row's own value placeholder shows in place of Value, and is never part of what typing changes", () => {
    render(<Harness start={[{ name: "x-portkey-provider", value: "", placeholder: E.PORTKEY_SLUG_PLACEHOLDER }, { name: "x-b", value: "" }]} />);
    expect(screen.getByPlaceholderText("@<slug>")).toHaveValue("");
    expect(screen.getAllByPlaceholderText(E.HEADER_VALUE_PLACEHOLDER)).toHaveLength(1);
    setField(screen.getByPlaceholderText("@<slug>"), "@team");
    expect(rows()[0]).toEqual({ name: "x-portkey-provider", value: "@team", placeholder: E.PORTKEY_SLUG_PLACEHOLDER });
  });

  it("shows the value hint under the rows when given one", () => {
    render(<Harness start={[{ name: "x-a", value: "" }]} hint={E.PORTKEY_SLUG_HINT} />);
    expect(screen.getByText(E.PORTKEY_SLUG_HINT)).toBeInTheDocument();
  });
});
