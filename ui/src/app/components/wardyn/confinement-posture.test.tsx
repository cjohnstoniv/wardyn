/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ConfinementPostureBanner } from "./confinement-posture";
import { ConfinementChip } from "./primitives";
import { OperatorProvider } from "./operator-context";
import {
  POSTURE_ACKNOWLEDGED_BANNER,
  POSTURE_CHIP_SUFFIX_ACKNOWLEDGED,
  POSTURE_CHIP_SUFFIX_UNENFORCED,
  POSTURE_UNENFORCED_BANNER,
  POSTURE_UNKNOWN_BANNER,
} from "./confinement-posture-copy";
import type { ConfinementPosture } from "./operator-context";

function renderBanner(posture: ConfinementPosture, view: "admin" | "user" = "admin", operator = true) {
  return render(
    <MemoryRouter>
      <OperatorProvider operator={operator} confinementPosture={posture}>
        <ConfinementPostureBanner view={view} />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

// The mock-approval's five-case table (issue #162's comment) — "enforced" and
// "" both render nothing, "acknowledged"/"unenforced"/"unknown" each render
// their own frozen sentence, byte-for-byte. Rendered in the Admin view: §4.2
// (M-3) makes the band Admin-view only, pinned in its own describe block below.
describe("ConfinementPostureBanner", () => {
  it("renders nothing for an empty posture (Docker / not applicable)", () => {
    const { container } = renderBanner("");
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing for 'enforced'", () => {
    const { container } = renderBanner("enforced");
    expect(container).toBeEmptyDOMElement();
  });

  it("renders the frozen acknowledged strip", () => {
    renderBanner("acknowledged");
    expect(screen.getByText(POSTURE_ACKNOWLEDGED_BANNER.TITLE)).toBeInTheDocument();
    expect(screen.getByText(POSTURE_ACKNOWLEDGED_BANNER.BODY)).toBeInTheDocument();
    expect(screen.getByText(POSTURE_ACKNOWLEDGED_BANNER.ACTION)).toBeInTheDocument();
  });

  it("renders the frozen unenforced strip", () => {
    renderBanner("unenforced");
    expect(screen.getByText(POSTURE_UNENFORCED_BANNER.TITLE)).toBeInTheDocument();
    expect(screen.getByText(POSTURE_UNENFORCED_BANNER.BODY)).toBeInTheDocument();
    expect(screen.getByText(POSTURE_UNENFORCED_BANNER.ACTION)).toBeInTheDocument();
  });

  // Ruling 2 — unknown/skew warns, it does not stay silent.
  it("renders the frozen unknown strip — ruling 2, could not confirm still warns", () => {
    renderBanner("unknown");
    expect(screen.getByText(POSTURE_UNKNOWN_BANNER.TITLE)).toBeInTheDocument();
    expect(screen.getByText(POSTURE_UNKNOWN_BANNER.BODY)).toBeInTheDocument();
    expect(screen.getByText(POSTURE_UNKNOWN_BANNER.ACTION)).toBeInTheDocument();
  });

  // #510-F7 — the action button sends /setup?step=environment, which a member
  // cannot reach (App.tsx's SetupRoute renders MemberGettingStarted for a
  // non-operator and ignores `step`). The strip stays informational for a
  // member: title and body still show, the action does not.
  it("drops the action button for a non-operator, keeps the strip informational", () => {
    renderBanner("unenforced", "admin", false);
    expect(screen.getByText(POSTURE_UNENFORCED_BANNER.TITLE)).toBeInTheDocument();
    expect(screen.getByText(POSTURE_UNENFORCED_BANNER.BODY)).toBeInTheDocument();
    expect(screen.queryByText(POSTURE_UNENFORCED_BANNER.ACTION)).not.toBeInTheDocument();
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });
});

// §4.2 (M-3) — the band is Admin-view only; the User view gets no band for any
// posture, because ConfinementChip already carries it on the person's own runs
// and the New Run rail.
describe("ConfinementPostureBanner — view (§4.2, M-3)", () => {
  it.each(["acknowledged", "unenforced", "unknown"] as ConfinementPosture[])(
    "renders nothing in the User view for posture %s",
    (posture) => {
      const { container } = renderBanner(posture, "user");
      expect(container).toBeEmptyDOMElement();
    },
  );

  it("defaults to the User view (no band) when no view is passed", () => {
    const { container } = render(
      <MemoryRouter>
        <OperatorProvider operator confinementPosture="unenforced">
          <ConfinementPostureBanner />
        </OperatorProvider>
      </MemoryRouter>,
    );
    expect(container).toBeEmptyDOMElement();
  });
});

function renderChip(posture: ConfinementPosture) {
  return render(
    <OperatorProvider operator confinementPosture={posture}>
      <ConfinementChip value="CC2" />
    </OperatorProvider>,
  );
}

// Ruling 1 — the unenforced chip carries a glyph as well as the ring; colour
// alone is never the signal (CONSOLE-RULES §2). The visible label and the
// screen-reader name stay exactly "Wall" (D4) — unchanged by any posture.
describe("ConfinementChip posture treatment", () => {
  it("an unenforced posture adds a glyph, and the label/accessible name are unchanged", () => {
    const { container } = renderChip("unenforced");
    expect(screen.getByText("Wall")).toBeInTheDocument();
    // Two icons now: the barrier icon (BrickWall) and the warning glyph.
    expect(container.querySelectorAll("svg").length).toBe(2);
  });

  it("an acknowledged posture adds the ring but not a second glyph", () => {
    const { container } = renderChip("acknowledged");
    expect(screen.getByText("Wall")).toBeInTheDocument();
    expect(container.querySelectorAll("svg").length).toBe(1);
  });

  it("an enforced or empty posture adds neither ring nor glyph", () => {
    for (const posture of ["enforced", ""] as ConfinementPosture[]) {
      const { container, unmount } = renderChip(posture);
      expect(container.querySelectorAll("svg").length).toBe(1);
      unmount();
    }
  });

  it("appends the frozen posture suffix to the chip's title, never to its accessible content", () => {
    const unenforced = renderChip("unenforced");
    const unenforcedTitle = unenforced.container.querySelector("[title]")?.getAttribute("title") ?? "";
    expect(unenforcedTitle.endsWith(POSTURE_CHIP_SUFFIX_UNENFORCED)).toBe(true);
    unenforced.unmount();

    const acknowledged = renderChip("acknowledged");
    const acknowledgedTitle = acknowledged.container.querySelector("[title]")?.getAttribute("title") ?? "";
    expect(acknowledgedTitle.endsWith(POSTURE_CHIP_SUFFIX_ACKNOWLEDGED)).toBe(true);
  });
});
