/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ConfinementPostureBanner } from "./confinement-posture";
import { ConfinementChip } from "./primitives";
import { health as api } from "../../lib/api/health";
import { OperatorProvider } from "./operator-context";
import {
  POSTURE_ACKNOWLEDGED_BANNER,
  POSTURE_CHIP_SUFFIX_ACKNOWLEDGED,
  POSTURE_CHIP_SUFFIX_NOT_PROVEN,
  POSTURE_CHIP_SUFFIX_RUNNER_NOT_PROVEN,
  POSTURE_CHIP_SUFFIX_RUNNER_UNENFORCED,
  POSTURE_CHIP_SUFFIX_UNENFORCED,
  POSTURE_NOT_PROVEN_BANNER,
  POSTURE_REFUSED_BANNER,
  POSTURE_RUNNER_ACKNOWLEDGED_BANNER,
  POSTURE_RUNNER_NOT_PROVEN_BANNER,
  POSTURE_RUNNER_REFUSED_BANNER,
  POSTURE_RUNNER_UNENFORCED_BANNER,
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

// A failed re-check never reads as confined on a chip, for members too (the
// band is Admin-view only): "refused" proved egress gets through, so runs
// already going carry the unenforced ring, glyph and suffix; "not_proven"
// carries the softer ring and its own suffix.
describe("ConfinementChip after a failed re-check", () => {
  const titleOf = (container: HTMLElement) => container.querySelector("[title]")?.getAttribute("title") ?? "";

  it("a refused posture marks the chip as unenforced: ring, glyph and suffix", () => {
    const { container } = renderChip("refused");
    expect(screen.getByText("Wall")).toBeInTheDocument();
    expect(container.querySelectorAll("svg").length).toBe(2);
    expect(titleOf(container).endsWith(POSTURE_CHIP_SUFFIX_UNENFORCED)).toBe(true);
    expect(container.querySelector("[title]")?.className).toContain("var(--warning)_55%");
  });

  it("a not-proven posture adds the softer ring and the not-proven suffix", () => {
    const { container } = renderChip("not_proven");
    expect(container.querySelectorAll("svg").length).toBe(1);
    expect(titleOf(container).endsWith(POSTURE_CHIP_SUFFIX_NOT_PROVEN)).toBe(true);
    expect(container.querySelector("[title]")?.className).toContain("var(--warning)_28%");
  });

  it("a member gets the same mark: the chip reads the posture whatever the role", () => {
    const { container } = render(
      <OperatorProvider operator={false} securityOperator={false} confinementPosture="refused">
        <ConfinementChip value="CC2" />
      </OperatorProvider>,
    );
    expect(container.querySelectorAll("svg").length).toBe(2);
    expect(titleOf(container).endsWith(POSTURE_CHIP_SUFFIX_UNENFORCED)).toBe(true);
  });
});

// A failed re-check of the canary is a danger strip, never the mild warning:
// "refused" names the check that produced it (from /healthz), "not_proven"
// says the checks could not run. Both use the setup row's sentences.
describe("ConfinementPostureBanner — failed re-check", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("renders the refused strip as danger, with the check time the shell read", () => {
    const at = new Date();
    const iso = at.toISOString(); // RFC 3339, UTC, as /healthz sends it
    const shown = `${at.getUTCFullYear()}-${String(at.getUTCMonth() + 1).padStart(2, "0")}-${String(at.getUTCDate()).padStart(2, "0")} ${String(at.getUTCHours()).padStart(2, "0")}:${String(at.getUTCMinutes()).padStart(2, "0")} UTC`;
    const health = vi.spyOn(api, "health");
    const { container } = render(
      <MemoryRouter>
        <OperatorProvider operator confinementPosture="refused" confinementCheckedAt={iso}>
          <ConfinementPostureBanner view="admin" />
        </OperatorProvider>
      </MemoryRouter>,
    );
    expect(screen.getByText(POSTURE_REFUSED_BANNER.TITLE)).toBeInTheDocument();
    // The time comes with the verdict from the shell's one /healthz answer, never a second read.
    expect(screen.getByText(POSTURE_REFUSED_BANNER.BODY(shown))).toBeInTheDocument();
    expect(health).not.toHaveBeenCalled();
    expect(screen.getByText(POSTURE_REFUSED_BANNER.ACTION)).toBeInTheDocument();
    expect(container.firstElementChild).toHaveClass("bg-danger-subtle");
  });

  it("renders the not-proven strip as danger", () => {
    const health = vi.spyOn(api, "health");
    const { container } = renderBanner("not_proven");
    expect(screen.getByText(POSTURE_NOT_PROVEN_BANNER.TITLE)).toBeInTheDocument();
    expect(screen.getByText(POSTURE_NOT_PROVEN_BANNER.BODY)).toBeInTheDocument();
    expect(container.firstElementChild).toHaveClass("bg-danger-subtle");
    expect(health).not.toHaveBeenCalled();
  });
});

// A verdict from a connected Kubernetes runner (network_policy_source "runner") names that runner,
// never "this cluster": the same tones, the runner's wording, on the band and the chip.
describe("ConfinementPostureBanner and ConfinementChip — a runner's verdict", () => {
  const renderRunner = (posture: ConfinementPosture, checkedAt = "") =>
    render(
      <MemoryRouter>
        <OperatorProvider operator confinementPosture={posture} confinementCheckedAt={checkedAt} confinementSource="runner">
          <ConfinementPostureBanner view="admin" />
          <ConfinementChip value="CC2" />
        </OperatorProvider>
      </MemoryRouter>,
    );
  const chipTitle = (container: HTMLElement) =>
    [...container.querySelectorAll("[title]")].at(-1)?.getAttribute("title") ?? "";

  it("refused names the runner and the check time", () => {
    const at = new Date();
    const iso = at.toISOString();
    const shown = `${iso.slice(0, 10)} ${iso.slice(11, 16)} UTC`;
    const { container } = renderRunner("refused", iso);
    expect(screen.getByText(POSTURE_RUNNER_REFUSED_BANNER.TITLE)).toBeInTheDocument();
    expect(screen.getByText(POSTURE_RUNNER_REFUSED_BANNER.BODY(shown))).toBeInTheDocument();
    expect(screen.queryByText(POSTURE_REFUSED_BANNER.BODY(shown))).toBeNull();
    expect(container.firstElementChild).toHaveClass("bg-danger-subtle");
    expect(chipTitle(container).endsWith(POSTURE_CHIP_SUFFIX_RUNNER_UNENFORCED)).toBe(true);
  });

  it("not_proven, unenforced and acknowledged name the runner", () => {
    const notProven = renderRunner("not_proven");
    expect(screen.getByText(POSTURE_RUNNER_NOT_PROVEN_BANNER.TITLE)).toBeInTheDocument();
    expect(screen.getByText(POSTURE_RUNNER_NOT_PROVEN_BANNER.BODY)).toBeInTheDocument();
    expect(chipTitle(notProven.container).endsWith(POSTURE_CHIP_SUFFIX_RUNNER_NOT_PROVEN)).toBe(true);
    notProven.unmount();

    const unenforced = renderRunner("unenforced");
    expect(screen.getByText(POSTURE_RUNNER_UNENFORCED_BANNER.TITLE)).toBeInTheDocument();
    expect(screen.getByText(POSTURE_RUNNER_UNENFORCED_BANNER.BODY)).toBeInTheDocument();
    expect(screen.queryByText(POSTURE_UNENFORCED_BANNER.TITLE)).toBeNull();
    unenforced.unmount();

    const acknowledged = renderRunner("acknowledged");
    expect(screen.getByText(POSTURE_RUNNER_ACKNOWLEDGED_BANNER.BODY)).toBeInTheDocument();
    expect(chipTitle(acknowledged.container).endsWith(POSTURE_CHIP_SUFFIX_ACKNOWLEDGED)).toBe(true);
  });

  it("without a source the band keeps this cluster's wording", () => {
    renderBanner("not_proven");
    expect(screen.getByText(POSTURE_NOT_PROVEN_BANNER.BODY)).toBeInTheDocument();
    expect(screen.queryByText(POSTURE_RUNNER_NOT_PROVEN_BANNER.BODY)).toBeNull();
  });
});
