/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it, vi } from "vitest";
import { render, screen, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import {
  ModelAccessProvider,
  useClaimModelAccessDoor,
  useModelAccessDoor,
} from "./model-access-context";
import { OperatorProvider } from "./operator-context";
import { baseStatus } from "../../lib/test-fixtures";
import type { SetupHarnessTool, SetupModelAccess, SetupStatus } from "../../lib/types";

const PER_USER_ROW: SetupHarnessTool = {
  id: "claude-code",
  display: "claude-code",
  has_gateway: false,
  has_login: true,
  enabled: true,
  mechanism: "bedrock_sso",
  credential_source: "per_user",
};

function statusWith(access: SetupModelAccess): SetupStatus {
  return baseStatus({ model_access: access, harnesses: [PER_USER_ROW] });
}

/** One consumer, rendering everything the hook answers plus the two controls. */
function Probe({ label = "probe" }: { label?: string }) {
  const door = useModelAccessDoor();
  return (
    <div>
      <span data-testid={`${label}-state`}>{door.state}</span>
      <span data-testid={`${label}-attention`}>{String(door.needsAttention)}</span>
      <span data-testid={`${label}-actionable`}>{String(door.actionable)}</span>
      <span data-testid={`${label}-claimed`}>{String(door.claimed)}</span>
      <span data-testid={`${label}-open`}>{String(door.open)}</span>
      <button type="button" onClick={door.openDoor}>{`${label} open`}</button>
      <button type="button" onClick={door.closeDoor}>{`${label} close`}</button>
      <button type="button" onClick={() => void door.refresh()}>{`${label} refresh`}</button>
    </div>
  );
}

/** A page surface that owns the door while it is mounted (the rail, the failure
 *  block, the held-approval row). */
function Claimer({ active = true }: { active?: boolean }) {
  useClaimModelAccessDoor(active);
  return <span>claimer</span>;
}

describe("useModelAccessDoor with no provider above it", () => {
  // The fail-open default, and the reason it must never be hardened into a
  // throw: every suite that mounts a screen directly, and every screen rendered
  // outside the shell, has to keep rendering exactly what it renders today.
  it("answers a door with nothing to say", () => {
    render(<Probe />);
    expect(screen.getByTestId("probe-state")).toHaveTextContent("");
    expect(screen.getByTestId("probe-attention")).toHaveTextContent("false");
    expect(screen.getByTestId("probe-actionable")).toHaveTextContent("false");
  });
});

describe("ModelAccessProvider grades the shell's status for the CONSUMER's viewer", () => {
  // OperatorProvider lives inside AppShell, BELOW the provider App.tsx mounts —
  // so the grading has to happen where it is consumed, or every caller reads
  // the fail-open operator default.
  it("a member's shared_expired is not actionable; an operator's is", () => {
    const status = statusWith({ state: "shared_expired", action: "ask them to reconnect it" });
    const { unmount } = render(
      <ModelAccessProvider status={status} onRefresh={vi.fn()}>
        <OperatorProvider operator={false} securityOperator={false}>
          <Probe />
        </OperatorProvider>
      </ModelAccessProvider>,
    );
    expect(screen.getByTestId("probe-attention")).toHaveTextContent("true");
    expect(screen.getByTestId("probe-actionable")).toHaveTextContent("false");
    unmount();

    render(
      <ModelAccessProvider status={status} onRefresh={vi.fn()}>
        <OperatorProvider operator securityOperator>
          <Probe />
        </OperatorProvider>
      </ModelAccessProvider>,
    );
    expect(screen.getByTestId("probe-actionable")).toHaveTextContent("true");
  });

  it("refresh reaches the shell's own reader", async () => {
    const onRefresh = vi.fn().mockResolvedValue(undefined);
    render(
      <ModelAccessProvider status={statusWith({ state: "not_configured" })} onRefresh={onRefresh}>
        <Probe />
      </ModelAccessProvider>,
    );
    await userEvent.click(screen.getByRole("button", { name: "probe refresh" }));
    expect(onRefresh).toHaveBeenCalledTimes(1);
  });
});

describe("ONE dialog instance — the open state is shared, never per caller", () => {
  it("a second caller's openDoor opens the same door, and either can close it", async () => {
    render(
      <ModelAccessProvider status={statusWith({ state: "not_configured" })} onRefresh={vi.fn()}>
        <Probe label="strip" />
        <Probe label="rail" />
      </ModelAccessProvider>,
    );
    expect(screen.getByTestId("strip-open")).toHaveTextContent("false");

    await userEvent.click(screen.getByRole("button", { name: "rail open" }));
    expect(screen.getByTestId("strip-open")).toHaveTextContent("true");
    expect(screen.getByTestId("rail-open")).toHaveTextContent("true");

    await userEvent.click(screen.getByRole("button", { name: "strip close" }));
    expect(screen.getByTestId("rail-open")).toHaveTextContent("false");
  });
});

describe("claim() — one PRIMARY recovery action per state per screen", () => {
  it("a mounted page surface owns the door and releases it on navigation away", () => {
    function Screen({ onRail }: { onRail: boolean }) {
      return (
        <ModelAccessProvider status={statusWith({ state: "not_configured" })} onRefresh={vi.fn()}>
          <Probe />
          {onRail && <Claimer />}
        </ModelAccessProvider>
      );
    }
    const { rerender } = render(<Screen onRail={false} />);
    expect(screen.getByTestId("probe-claimed")).toHaveTextContent("false");

    rerender(<Screen onRail />);
    expect(screen.getByTestId("probe-claimed")).toHaveTextContent("true");

    // Navigating away unmounts the claimer — the strip gets its button back.
    rerender(<Screen onRail={false} />);
    expect(screen.getByTestId("probe-claimed")).toHaveTextContent("false");
  });

  it("a surface that renders no sign-in control does not claim", () => {
    render(
      <ModelAccessProvider status={statusWith({ state: "shared_expired" })} onRefresh={vi.fn()}>
        <Probe />
        <Claimer active={false} />
      </ModelAccessProvider>,
    );
    expect(screen.getByTestId("probe-claimed")).toHaveTextContent("false");
  });

  // A COUNT, not a boolean: during a route change React runs the new screen's
  // effect before the old screen's cleanup, so two claims legitimately overlap
  // for a frame — and a boolean would suppress the strip's button forever after
  // the first time it happened.
  it("two overlapping claims release independently", () => {
    function Two({ first, second }: { first: boolean; second: boolean }) {
      return (
        <ModelAccessProvider status={statusWith({ state: "not_configured" })} onRefresh={vi.fn()}>
          <Probe />
          {first && <Claimer />}
          {second && <Claimer />}
        </ModelAccessProvider>
      );
    }
    const { rerender } = render(<Two first second />);
    expect(screen.getByTestId("probe-claimed")).toHaveTextContent("true");
    rerender(<Two first second={false} />);
    expect(screen.getByTestId("probe-claimed")).toHaveTextContent("true");
    rerender(<Two first={false} second={false} />);
    expect(screen.getByTestId("probe-claimed")).toHaveTextContent("false");
  });

  it("claim keeps ONE identity across a status poll, so a claimer's effect does not churn", () => {
    const claims: (() => () => void)[] = [];
    function Recorder() {
      claims.push(useModelAccessDoor().claim);
      return null;
    }
    function Poll({ state }: { state: string }) {
      return (
        <ModelAccessProvider status={statusWith({ state })} onRefresh={vi.fn()}>
          <Recorder />
        </ModelAccessProvider>
      );
    }
    const { rerender } = render(<Poll state="not_configured" />);
    act(() => rerender(<Poll state="expiring" />));
    expect(claims.length).toBeGreaterThan(1);
    expect(new Set(claims).size).toBe(1);
  });
});
