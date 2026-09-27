/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen, within } from "@testing-library/react";
import type { ApprovalRequest } from "../../lib/types";
import type { PushContentScope } from "../../lib/types/approvals";
import { isPushContentRequest, PushContentCard, unquoteGitPath, type PushCardRun } from "./push-content-card";
import { PUSH } from "./copy/push";
import { APPROVAL, SECURITY_ONLY_REASON } from "./copy";
import { heldFieldsFor } from "../../lib/test-hold-fixture";

const RUNNING: PushCardRun = { state: "RUNNING" };
const ENDED: PushCardRun = { state: "COMPLETED" };

function scope(over: Partial<PushContentScope> = {}): PushContentScope {
  return {
    repo: "github.com/acme/payments",
    branch: "refs/heads/feature/x",
    acts_as: "github_token:11111111-1111-1111-1111-111111111111",
    paths: ["a.txt", "b.txt"],
    paths_total: 2,
    commits: ["deadbeef".repeat(5)],
    paths_digest: "a".repeat(64),
    acts_as_kind: "github_app",
    acts_as_label: "dana@acme.example",
    ...over,
  };
}

// #1197: held/held_until are now server fields — heldFieldsFor mirrors
// internal/approval.Hold so this fixture keeps constructing rows by
// kind/requested_at/state exactly as it always has.
function push(over: Partial<ApprovalRequest> = {}): ApprovalRequest & { requested_scope: PushContentScope } {
  const base = {
    id: "apr_push_1",
    run_id: "run_1",
    kind: "push_content",
    requested_scope: scope(over.requested_scope as Partial<PushContentScope> | undefined),
    state: "PENDING",
    requested_at: new Date().toISOString(),
    ...over,
  } as ApprovalRequest & { requested_scope: PushContentScope };
  return { ...base, ...heldFieldsFor(base) };
}

describe("PushContentCard — held (#181)", () => {
  it("shows the frozen title, the repository, branch and acts_as_label — never the raw acts_as", () => {
    render(<PushContentCard item={push()} securityOperator run={RUNNING} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />);
    const card = screen.getByTestId("push-content-card");
    expect(within(card).getByText(PUSH.CARD_TITLE)).toBeInTheDocument();
    expect(within(card).getByText("github.com/acme/payments")).toBeInTheDocument();
    expect(within(card).getByText("refs/heads/feature/x")).toBeInTheDocument();
    expect(within(card).getByText("dana@acme.example")).toBeInTheDocument();
    // never the raw acts_as (a credential reference, not a principal)
    expect(within(card).queryByText(/github_token:11111111/)).not.toBeInTheDocument();
  });

  it("lists up to ten paths, then '+N more' — no expanding control", () => {
    const paths = Array.from({ length: 10 }, (_, i) => `dir/file-${i}.txt`);
    render(
      <PushContentCard
        item={push({ requested_scope: { paths, paths_total: 14 } as Partial<PushContentScope> })}
        securityOperator
        run={RUNNING}
        busy={null}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = screen.getByTestId("push-content-card");
    for (const p of paths) expect(within(card).getByText(p)).toBeInTheDocument();
    expect(within(card).getByText(PUSH.PATHS_MORE(4))).toBeInTheDocument();
    expect(within(card).getByText(PUSH.PATHS_NOTE)).toBeInTheDocument();
    // No expand/"view all" affordance at all (Q181-2) — the card carries no
    // raw-scope disclosure control; the full list lives in the audit trail.
    expect(within(card).queryByRole("button", { name: /more|expand|all/i })).not.toBeInTheDocument();
  });

  it("shows exactly the ten names paths_total does not exceed — no '+N more' when everything fits", () => {
    render(
      <PushContentCard
        item={push({ requested_scope: { paths: ["a.txt"], paths_total: 1 } as Partial<PushContentScope> })}
        securityOperator
        run={RUNNING}
        busy={null}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    expect(screen.queryByText(/\+\d+ more/)).not.toBeInTheDocument();
  });

  it("never labels scope.commits as commits, including for an ADO REST push's body digest", () => {
    render(
      <PushContentCard
        item={push({
          requested_scope: {
            acts_as_kind: "ado_entra",
            commits: ["b".repeat(64)], // a SHA-256 of the request body, not an object id
          } as Partial<PushContentScope>,
        })}
        securityOperator
        run={RUNNING}
        busy={null}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = screen.getByTestId("push-content-card");
    // No labelled "Commits" field anywhere on the card.
    expect(within(card).queryByText(/^Commits$/)).not.toBeInTheDocument();
  });

  // Review finding 7 — a broader guard than the text-only check above:
  // acts_as must not leak through ANY attribute (title, aria-label, ...) on
  // ANY element in the card, not just as visible text. innerHTML serializes
  // every attribute along with text content, so one substring check covers
  // both.
  it("never leaks the raw acts_as through any attribute (title/aria-label) anywhere in the card", () => {
    render(<PushContentCard item={push()} securityOperator run={RUNNING} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />);
    const card = screen.getByTestId("push-content-card");
    expect(card.innerHTML).not.toContain("github_token:11111111-1111-1111-1111-111111111111");
  });

  // Review finding 7 — the ADO REST digest must not render as "Commit"/
  // "Commits" in ANY form (a label, a title attribute, an aria-label), not
  // just as a <dt> field.
  it("never renders 'Commit'/'Commits' in any form for an ADO REST push's body digest", () => {
    render(
      <PushContentCard
        item={push({
          requested_scope: {
            acts_as_kind: "ado_entra",
            commits: ["b".repeat(64)], // a SHA-256 of the request body, not an object id
          } as Partial<PushContentScope>,
        })}
        securityOperator
        run={RUNNING}
        busy={null}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = screen.getByTestId("push-content-card");
    expect(card.innerHTML).not.toMatch(/commit/i);
  });

  it("reuses the approval banner labels for What/Blast, with PUSH.WHAT/BLAST", () => {
    render(<PushContentCard item={push()} securityOperator run={RUNNING} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />);
    const card = screen.getByTestId("push-content-card");
    expect(within(card).getByText(PUSH.WHAT("github.com/acme/payments", "dana@acme.example"))).toBeInTheDocument();
    expect(within(card).getByText(PUSH.BLAST)).toBeInTheDocument();
  });

  it("carries the held note and, for a decider, the approve note", () => {
    render(<PushContentCard item={push()} securityOperator run={RUNNING} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />);
    expect(screen.getByText(PUSH.HELD_NOTE)).toBeInTheDocument();
    expect(screen.getByText(PUSH.APPROVE_NOTE)).toBeInTheDocument();
  });
});

// Review finding 2 — HELD_NOTE only holds while the proxy's own bounded
// window (isHeld's push_content arm, lib/types/approvals.ts) is still open;
// past it the card flips to HELD_EXPIRED on its own, via a timer, not only on
// the next poll tick.
describe("PushContentCard — the held note flips to HELD_EXPIRED at the window end", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("shows HELD_NOTE for a fresh row and HELD_EXPIRED once the 600s ceiling passes, with no poll/re-render forced from outside", () => {
    render(<PushContentCard item={push()} securityOperator run={RUNNING} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />);
    expect(screen.getByText(PUSH.HELD_NOTE)).toBeInTheDocument();
    expect(screen.queryByText(PUSH.HELD_EXPIRED)).not.toBeInTheDocument();

    act(() => {
      vi.advanceTimersByTime(600_001);
    });

    expect(screen.getByText(PUSH.HELD_EXPIRED)).toBeInTheDocument();
    expect(screen.queryByText(PUSH.HELD_NOTE)).not.toBeInTheDocument();
  });

  it("shows HELD_EXPIRED immediately for a row already past the window", () => {
    const past = new Date(Date.now() - 601_000).toISOString();
    render(
      <PushContentCard
        item={push({ requested_at: past })}
        securityOperator
        run={RUNNING}
        busy={null}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    expect(screen.getByText(PUSH.HELD_EXPIRED)).toBeInTheDocument();
  });

  it("clears its timer on unmount (no act() warning, no leaked timer)", () => {
    const { unmount } = render(
      <PushContentCard item={push()} securityOperator run={RUNNING} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />,
    );
    unmount();
    expect(() => vi.advanceTimersByTime(600_001)).not.toThrow();
  });
});

describe("PushContentCard — deciding", () => {
  it("disables both buttons while deciding, and only Approve spins", () => {
    render(<PushContentCard item={push()} securityOperator run={RUNNING} busy="approve" onApprove={vi.fn()} onDeny={vi.fn()} />);
    const approve = screen.getByRole("button", { name: /Approve/ });
    const deny = screen.getByRole("button", { name: /Deny/ });
    expect(approve).toBeDisabled();
    expect(deny).toBeDisabled();
    expect(approve.querySelector(".animate-spin")).not.toBeNull();
    expect(deny.querySelector(".animate-spin")).toBeNull();
  });

  it("only Deny spins while a deny is in flight", () => {
    render(<PushContentCard item={push()} securityOperator run={RUNNING} busy="deny" onApprove={vi.fn()} onDeny={vi.fn()} />);
    expect(screen.getByRole("button", { name: /Approve/ }).querySelector(".animate-spin")).toBeNull();
    expect(screen.getByRole("button", { name: /Deny/ }).querySelector(".animate-spin")).not.toBeNull();
  });

  it("Approve/Deny call straight through with no dialog", async () => {
    const onApprove = vi.fn();
    const onDeny = vi.fn();
    render(<PushContentCard item={push()} securityOperator run={RUNNING} busy={null} onApprove={onApprove} onDeny={onDeny} />);
    screen.getByRole("button", { name: /Approve/ }).click();
    expect(onApprove).toHaveBeenCalledTimes(1);
    screen.getByRole("button", { name: /Deny/ }).click();
    expect(onDeny).toHaveBeenCalledTimes(1);
    // No reason textbox, no scope control — this card decides directly.
    expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
    expect(screen.queryByRole("radiogroup")).not.toBeInTheDocument();
  });
});

describe("PushContentCard — cancelled (run ended)", () => {
  it("withdraws the decision pair and shows the reused APPROVAL.CANCELLED_BODY", () => {
    render(<PushContentCard item={push()} securityOperator run={ENDED} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />);
    expect(screen.queryByRole("button", { name: /Approve/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Deny/ })).not.toBeInTheDocument();
    expect(screen.getByText(APPROVAL.CANCELLED_BODY)).toBeInTheDocument();
  });
});

describe("PushContentCard — member watching, decides nothing", () => {
  it("shows the admin-only reason instead of Approve/Deny for a non-security-operator", () => {
    render(<PushContentCard item={push()} securityOperator={false} run={RUNNING} busy={null} onApprove={vi.fn()} onDeny={vi.fn()} />);
    expect(screen.queryByRole("button", { name: /Approve/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Deny/ })).not.toBeInTheDocument();
    expect(screen.getByText(SECURITY_ONLY_REASON)).toBeInTheDocument();
    // Still sees the full requested scope — "sees everything, decides nothing".
    expect(screen.getByText("github.com/acme/payments")).toBeInTheDocument();
  });
});

// unquoteGitPath — the inverse of internal/egress/proxy/push_hold.go's
// quotePath (git's own core.quotePath=true / quote_c_style). Owner note on
// #555, 2026-09-26: since #1066/#1087 every held-push path arrives quoted
// this way, so a review-matched "café.yml" reaches the console as
// `"caf\303\251.yml"` — display it unquoted, but never touch the stored,
// exported or approved (digest) form.
//
// SECURITY (coordinator finding, 2026-09-27): this card is an APPROVAL
// surface, so decoding must never let a path spoof what it shows the
// approver. \a\b\t\n\v\f\r are never decoded (the raw form stays on
// screen), and the fully decoded string is refused — falling back to raw —
// if it holds any control (Cc) or Unicode format/bidi (Cf) character, since
// an octal escape alone can assemble one of those with no shorthand at all
// (a bare control byte, or U+202E RIGHT-TO-LEFT OVERRIDE turning
// "evil<RLO>txt.sh" into something that reads as "evilhs.txt").
describe("unquoteGitPath", () => {
  it("passes an unquoted ASCII path through unchanged — quotePath itself never wraps one", () => {
    expect(unquoteGitPath("dir/file.txt")).toBe("dir/file.txt");
    expect(unquoteGitPath("")).toBe("");
  });

  it("decodes the octal-escaped UTF-8 case exactly as quotePath produced it", () => {
    // "é" is U+00E9 = UTF-8 bytes 0xC3 0xA9 = octal 303 251 — quotePath's own
    // worked example (docs/design/held-push-canon.md).
    expect(unquoteGitPath('"caf\\303\\251.yml"')).toBe("café.yml");
  });

  it("decodes \\\" and \\\\, the only two shorthand escapes left", () => {
    expect(unquoteGitPath('"a\\"b"')).toBe('a"b');
    expect(unquoteGitPath('"a\\\\b"')).toBe("a\\b");
  });

  it("SECURITY: never decodes the abtnvfr control-byte shorthand — the raw form stays on screen", () => {
    expect(unquoteGitPath('"a\\tb"')).toBe('"a\\tb"');
    expect(unquoteGitPath('"a\\nb"')).toBe('"a\\nb"');
    expect(unquoteGitPath('"a\\rb"')).toBe('"a\\rb"');
  });

  it("SECURITY: a raw octal control byte (ESC, \\033) refuses the whole decode", () => {
    expect(unquoteGitPath('"a\\033b"')).toBe('"a\\033b"');
  });

  it("SECURITY: a Unicode bidi-override (RLO, U+202E) refuses the whole decode — never renders the character that could spoof the name", () => {
    // U+202E = UTF-8 0xE2 0x80 0xAE = octal 342 200 256.
    expect(unquoteGitPath('"evil\\342\\200\\256txt.sh"')).toBe('"evil\\342\\200\\256txt.sh"');
  });

  it("SECURITY: a zero-width space (U+200B) refuses the whole decode", () => {
    // U+200B = UTF-8 0xE2 0x80 0x8B = octal 342 200 213.
    expect(unquoteGitPath('"\\342\\200\\213"')).toBe('"\\342\\200\\213"');
  });

  it("falls back to the raw string on a malformed escape, an unterminated quote, or invalid UTF-8 bytes", () => {
    expect(unquoteGitPath('"a\\qb"')).toBe('"a\\qb"'); // \q is not an escape quotePath ever emits
    expect(unquoteGitPath('"unterminated')).toBe('"unterminated'); // no closing quote
    expect(unquoteGitPath('"\\300\\300"')).toBe('"\\300\\300"'); // two lead bytes, not valid UTF-8
  });
});

describe("isPushContentRequest (#181)", () => {
  it("is true only for kind push_content", () => {
    expect(isPushContentRequest(push())).toBe(true);
    expect(isPushContentRequest({ kind: "tool_call", requested_scope: {} })).toBe(false);
    expect(isPushContentRequest({ kind: "egress_domain", requested_scope: {} })).toBe(false);
  });
});

describe("PushContentCard — a quoted path renders unquoted (owner note, 2026-09-26)", () => {
  it("shows café.yml, never the wire's \\303\\251 escape form", () => {
    render(
      <PushContentCard
        item={push({ requested_scope: { paths: ['"caf\\303\\251.yml"'], paths_total: 1 } as Partial<PushContentScope> })}
        securityOperator
        run={RUNNING}
        busy={null}
        onApprove={vi.fn()}
        onDeny={vi.fn()}
      />,
    );
    const card = screen.getByTestId("push-content-card");
    expect(within(card).getByText("café.yml")).toBeInTheDocument();
    expect(within(card).queryByText(/\\303\\251/)).not.toBeInTheDocument();
  });
});
