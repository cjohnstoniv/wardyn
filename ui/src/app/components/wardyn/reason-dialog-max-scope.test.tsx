/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { ReasonDialog } from "./reason-dialog";
import { OperatorProvider } from "./operator-context";
import { DECIDERS_INBOX } from "./copy/deciders-inbox";
import type { ApprovalScope } from "../../lib/types";

const open = (maxScope?: ApprovalScope, securityOperator = true) =>
  render(
    <OperatorProvider operator={false} securityOperator={securityOperator}>
      <ReasonDialog
        prompt={{ id: "ap_1", action: "approve", kind: "egress_domain", ...(maxScope ? { maxScope } : {}) }}
        hasWorkspace
        onClose={() => {}}
        onSubmit={vi.fn().mockResolvedValue(true)}
      />
    </OperatorProvider>,
  );

const radio = (name: RegExp) => screen.getByRole("radio", { name });
const radios = () => screen.getAllByRole("radio").map((r) => ({ text: r.textContent, disabled: (r as HTMLButtonElement).disabled, checked: r.getAttribute("aria-checked") }));

describe("ReasonDialog — scope under a rule's cap", () => {
  it("under until, This run and Always are disabled with the cap reason", () => {
    open("until");
    const reason = DECIDERS_INBOX.SCOPE_CAP("Until");
    expect(reason).toBe("This unit's rule allows nothing wider than Until.");
    expect(radio(/^Once/)).toBeEnabled();
    expect(radio(/^Until/)).toBeEnabled();
    expect(radio(/^This run/)).toBeDisabled();
    expect(radio(/^This run/)).toHaveTextContent(reason);
    expect(radio(/^Always/)).toBeDisabled();
    // The cap's reason wins over the Always reasons that exist today.
    expect(radio(/^Always/)).toHaveTextContent(reason);
  });

  it("under run, only Always is disabled, with the This run wording", () => {
    open("run");
    expect(radio(/^This run/)).toBeEnabled();
    expect(radio(/^Always/)).toBeDisabled();
    expect(radio(/^Always/)).toHaveTextContent("This unit's rule allows nothing wider than This run.");
  });

  it("under once, everything wider is disabled and the dialog opens on Once", async () => {
    open("once");
    expect(radio(/^Until/)).toBeDisabled();
    expect(radio(/^This run/)).toBeDisabled();
    expect(radio(/^Always/)).toBeDisabled();
    expect(radio(/^Until/)).toHaveTextContent("This unit's rule allows nothing wider than Once.");
    await vi.waitFor(() => expect(radio(/^Once/)).toHaveAttribute("aria-checked", "true"));
  });

  it("under until, the dialog never opens on a disabled option", async () => {
    open("until");
    await vi.waitFor(() => expect(radio(/^Until/)).toHaveAttribute("aria-checked", "true"));
    expect(radio(/^This run/)).toHaveAttribute("aria-checked", "false");
  });

  it("with no cap it renders as today", () => {
    open(undefined);
    expect(radios()).toEqual([
      { text: "OnceThis one connection. The next attempt asks again.", disabled: false, checked: "false" },
      { text: "This runEvery attempt until this run ends. (default)", disabled: false, checked: "true" },
      { text: "Until…Every attempt until a time you pick, or this run ends.", disabled: false, checked: "false" },
      { text: "AlwaysAlso saves it to the workspace — future runs start with it.", disabled: false, checked: "false" },
    ]);
  });
});
