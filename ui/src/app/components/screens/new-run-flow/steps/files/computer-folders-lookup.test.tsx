/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Your computer" in the Add dialog: only a successful /setup/status read that says false earns the organisation's
// wording. A read that failed is not an answer: the tile stays open, with a "couldn't check" note and Try again.
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, renderHook, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { NR_FILES, NR_FILES_LOOKUP } from "../../../../wardyn/copy/new-run/files";
import type { RunIntent } from "../../draft/run-intent";
import { AddDialog } from "./add-dialog";
import { useComputerFoldersAllowed } from "./computer-folder";

const getSetupStatus = vi.hoisted(() => vi.fn());
vi.mock("../../../../../lib/api/setup", () => ({ setup: { getSetupStatus: (...a: unknown[]) => getSetupStatus(...a) } }));

const status = (computer_folders: boolean) => ({ runner: { driver: "", confinement_classes: [], computer_folders } });

afterEach(() => {
  cleanup();
  getSetupStatus.mockReset();
});

describe("useComputerFoldersAllowed", () => {
  it("a failed read is unknown and failed, not false; retry reads again", async () => {
    getSetupStatus.mockRejectedValueOnce(new Error("HTTP 500"));
    const { result } = renderHook(() => useComputerFoldersAllowed());
    await waitFor(() => expect(result.current.failed).toBe(true));
    expect(result.current.allowed).toBeUndefined();
    getSetupStatus.mockResolvedValue(status(true));
    act(() => result.current.retry());
    await waitFor(() => expect(result.current.allowed).toBe(true));
    expect(result.current.failed).toBe(false);
  });

  it("a successful read says false or true", async () => {
    getSetupStatus.mockResolvedValue(status(false));
    const { result } = renderHook(() => useComputerFoldersAllowed());
    await waitFor(() => expect(result.current.allowed).toBe(false));
    expect(result.current.failed).toBe(false);
  });
});

function Dialog(props: { allowed?: boolean; retry?: () => void }) {
  const intent: RunIntent = { shape: "environment", experience: "interactive" };
  return (
    <MemoryRouter>
      <AddDialog
        open={{ mode: "add" }}
        onClose={() => {}}
        intent={intent}
        src={{ rows: [], folders: [], catalogue: null, attachments: [] }}
        items={[]}
        computerAllowed={props.allowed}
        computerLookupFailed={props.retry}
        operator={false}
        onSave={() => {}}
        reloadDrives={() => {}}
      />
    </MemoryRouter>
  );
}
const computer = () => screen.getByRole("button", { name: new RegExp(`^${NR_FILES.COMPUTER_TITLE}`) });

describe("the Your computer tile", () => {
  it("a read that said false shows the organisation wording and is disabled", () => {
    render(<Dialog allowed={false} />);
    expect(computer()).toBeDisabled();
    expect(within(computer()).getByText(NR_FILES.COMPUTER_BLOCKED)).toBeInTheDocument();
    expect(screen.queryByText(NR_FILES_LOOKUP.COMPUTER_UNCHECKED)).toBeNull();
  });

  it("a failed read never says the organisation forbids it: the tile stays open with a note and Try again", async () => {
    const retry = vi.fn();
    render(<Dialog retry={retry} />);
    expect(computer()).toBeEnabled();
    expect(screen.queryByText(NR_FILES.COMPUTER_BLOCKED)).toBeNull();
    expect(screen.getByRole("status")).toHaveTextContent(NR_FILES_LOOKUP.COMPUTER_UNCHECKED);
    await userEvent.click(screen.getByRole("button", { name: NR_FILES_LOOKUP.RETRY }));
    expect(retry).toHaveBeenCalledTimes(1);
  });

  it("while loading, or when allowed, there is no note and no block", () => {
    const { rerender } = render(<Dialog />);
    expect(screen.queryByRole("status")).toBeNull();
    expect(computer()).toBeEnabled();
    rerender(<Dialog allowed />);
    expect(screen.queryByRole("status")).toBeNull();
    expect(screen.queryByText(NR_FILES.COMPUTER_BLOCKED)).toBeNull();
  });
});
