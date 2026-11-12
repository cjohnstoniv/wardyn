/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { PageTabs, pageTabPanelProps } from "./page-tabs";

afterEach(cleanup);

function Harness({ idPrefix }: { idPrefix?: string }) {
  const [tab, setTab] = React.useState<"one" | "two">("one");
  return (
    <>
      <PageTabs
        value={tab}
        onValueChange={setTab}
        idPrefix={idPrefix}
        tabs={[
          { value: "one", label: "One" },
          { value: "two", label: "Two" },
        ]}
      />
      <div {...(idPrefix ? pageTabPanelProps(idPrefix, tab) : {})}>content</div>
    </>
  );
}

describe("PageTabs panel", () => {
  it("names the content under the strip as the selected tab's panel", () => {
    render(<Harness idPrefix="demo" />);
    const panel = screen.getByRole("tabpanel");
    expect(panel).toHaveAccessibleName("One");
    for (const tab of screen.getAllByRole("tab")) expect(tab.getAttribute("aria-controls")).toBe(panel.id);
  });

  it("every tab's aria-controls resolves to an element in the page", () => {
    render(<Harness idPrefix="demo" />);
    for (const tab of screen.getAllByRole("tab")) expect(document.getElementById(tab.getAttribute("aria-controls")!)).not.toBeNull();
  });

  it("the panel follows the selected tab", async () => {
    render(<Harness idPrefix="demo" />);
    const user = (await import("@testing-library/user-event")).default.setup();
    await user.click(screen.getByRole("tab", { name: "Two" }));
    expect(screen.getByRole("tabpanel")).toHaveAccessibleName("Two");
  });
});
