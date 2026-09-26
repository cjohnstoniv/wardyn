/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// AvailabilityControl — every state docs/design/available-to-mock/states.html
// draws, on its own fixture (the stored policy "Read-only research"), plus
// the image family and the creation-form draft. Each visible line is pinned
// to the canon string it renders. The real-backend round trip through an
// editor is providers.spec.ts's Playwright pin.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import * as React from "react";
import { HttpError } from "../../lib/api/core";
import { aheadByHours } from "../../lib/test-clock";

const getAvailabilityMock = vi.fn();
const putAvailabilityMock = vi.fn();
const upsertGrantMock = vi.fn();
const deleteGrantMock = vi.fn();
const listUserTypesMock = vi.fn();
vi.mock("../../lib/api/permissions", async () => {
  const actual = await vi.importActual<typeof import("../../lib/api/permissions")>("../../lib/api/permissions");
  return {
    ...actual,
    permissions: {
      ...actual.permissions,
      getAvailability: (...a: unknown[]) => getAvailabilityMock(...a),
      putAvailability: (...a: unknown[]) => putAvailabilityMock(...a),
      upsertGrant: (...a: unknown[]) => upsertGrantMock(...a),
      deleteGrant: (...a: unknown[]) => deleteGrantMock(...a),
      listUserTypes: (...a: unknown[]) => listUserTypesMock(...a),
    },
  };
});

import { AVAILABILITY } from "../../lib/availability-copy";
import { PERM } from "../../lib/permissions-copy";
import {
  AvailabilityControl,
  AvailabilityDraft,
  writeAvailability,
  type AvailabilityDraftValue,
} from "./availability-control";
import { OperatorProvider } from "./operator-context";

const KIND = "policy";
const ID = "pol-7f3a2c";
const EMPTY_ONLY = "Add at least one person, group or user type before choosing Only, or nobody could use this.";
const UNKNOWN_TYPE = 'The user type "analyst" doesn\'t exist. Create it under User types first.';

const grant = (id: string, subject_type: "user_type" | "group" | "user", subject: string) => ({
  id,
  subject_type,
  subject,
  capability: KIND,
  value: ID,
  effect: "allow" as const,
  created_at: aheadByHours(-24),
});
const DEV = grant("g1", "user_type", "developer");
const PM = grant("g2", "user_type", "portfolio-manager");
const DATA = grant("g3", "group", "Data engineering");
const ALICE = grant("g4", "user", "alice@corp.com");
const view = (restricted: boolean, allowed_by: ReturnType<typeof grant>[] = []) => ({
  kind: KIND,
  value: ID,
  restricted,
  allowed_by,
});

beforeEach(() => {
  getAvailabilityMock.mockReset();
  putAvailabilityMock.mockReset();
  upsertGrantMock.mockReset();
  deleteGrantMock.mockReset();
  listUserTypesMock.mockReset();
  listUserTypesMock.mockResolvedValue([
    { id: "developer", name: "Developer", description: "", priority: 0, built_in: false },
    { id: "portfolio-manager", name: "Portfolio manager", description: "", priority: 0, built_in: false },
  ]);
});

function policyControl() {
  return (
    <AvailabilityControl
      kind={KIND}
      value={ID}
      onlyHint={AVAILABILITY.POLICY_ONLY_HINT}
      note={AVAILABILITY.POLICY_NOTE}
    />
  );
}

const first = () => screen.getByRole("radio", { name: AVAILABILITY.EVERYONE });
const only = () => screen.getByRole("radio", { name: AVAILABILITY.ONLY });

describe("AvailabilityControl — states.html", () => {
  it("state 1: Everyone by default, with the policy's only-these line and note, the adder per #919", async () => {
    getAvailabilityMock.mockResolvedValue(view(false));
    render(policyControl());

    expect(await screen.findByText(AVAILABILITY.LABEL)).toBeInTheDocument();
    expect(getAvailabilityMock).toHaveBeenCalledWith(KIND, ID);
    expect(first()).toBeChecked();
    expect(only()).not.toBeChecked();
    // The only-these option reads as the canon string whole.
    expect(screen.getByTestId("availability-only").textContent).toBe(AVAILABILITY.POLICY_ONLY_HINT);
    expect(screen.getByText(AVAILABILITY.POLICY_NOTE)).toBeInTheDocument();
    for (const seg of [PERM.SUBJECT_USER_TYPE, PERM.SUBJECT_GROUP, PERM.SUBJECT_USER]) {
      expect(screen.getByRole("button", { name: seg })).toBeInTheDocument();
    }
    expect(screen.getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER)).toHaveAccessibleName(AVAILABILITY.HINT_USER_TYPE);
    expect(screen.getByRole("button", { name: AVAILABILITY.ADD_CTA })).toBeDisabled();
  });

  it("state 2: chips name a type, mark a group and show a person's email, each removable by name", async () => {
    getAvailabilityMock.mockResolvedValue(view(true, [DEV, PM, DATA, ALICE]));
    render(policyControl());

    for (const chip of ["Developer", "Portfolio manager", "Data engineering (group)", "alice@corp.com"]) {
      expect(await screen.findByText(chip)).toBeInTheDocument();
      expect(screen.getByRole("button", { name: AVAILABILITY.REMOVE_ARIA(chip) })).toBeEnabled();
    }
    expect(only()).toBeChecked();
    expect(screen.queryByText(AVAILABILITY.LAST_AUDIENCE_LOCKED)).not.toBeInTheDocument();
  });

  it("state 2: the list is written while Everyone, then choosing Only these turns it on", async () => {
    getAvailabilityMock.mockResolvedValueOnce(view(false)).mockResolvedValueOnce(view(false, [DEV]));
    upsertGrantMock.mockResolvedValue({ grant: DEV, updated: false });
    putAvailabilityMock.mockResolvedValue(view(true, [DEV]));
    render(policyControl());
    await screen.findByText(AVAILABILITY.LABEL);

    await userEvent.type(screen.getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER), "developer");
    await userEvent.click(screen.getByRole("button", { name: AVAILABILITY.ADD_CTA }));
    await waitFor(() =>
      expect(upsertGrantMock).toHaveBeenCalledWith({
        subject_type: "user_type",
        subject: "developer",
        capability: KIND,
        value: ID,
        effect: "allow",
      }),
    );
    expect(await screen.findByText("Developer")).toBeInTheDocument();
    expect(first()).toBeChecked();

    await userEvent.click(only());
    expect(putAvailabilityMock).toHaveBeenCalledWith(KIND, ID, true);
    await waitFor(() => expect(only()).toBeChecked());
  });

  it("state 3: Only these with nobody listed shows the server's sentence as sent, and stays on Everyone", async () => {
    getAvailabilityMock.mockResolvedValue(view(false));
    putAvailabilityMock.mockRejectedValue(new HttpError(400, EMPTY_ONLY));
    render(policyControl());
    await screen.findByText(AVAILABILITY.LABEL);

    await userEvent.click(only());

    expect(await screen.findByText(EMPTY_ONLY)).toBeInTheDocument();
    expect(first()).toBeChecked();
  });

  it("state 4: the last one on the list is locked while Only these is on, with the reason twice", async () => {
    getAvailabilityMock.mockResolvedValue(view(true, [DEV]));
    render(policyControl());

    const remove = await screen.findByRole("button", { name: AVAILABILITY.REMOVE_ARIA("Developer") });
    expect(remove).toBeDisabled();
    expect(remove).toHaveAttribute("title", AVAILABILITY.LAST_AUDIENCE_LOCKED);
    expect(screen.getByText(AVAILABILITY.LAST_AUDIENCE_LOCKED)).toBeInTheDocument();
    await userEvent.click(remove);
    expect(deleteGrantMock).not.toHaveBeenCalled();
  });

  it("state 5: taking a type off writes at once, with no confirmation, and the one left locks", async () => {
    getAvailabilityMock.mockResolvedValueOnce(view(true, [DEV, PM])).mockResolvedValueOnce(view(true, [DEV]));
    deleteGrantMock.mockResolvedValue(undefined);
    render(policyControl());

    await userEvent.click(await screen.findByRole("button", { name: AVAILABILITY.REMOVE_ARIA("Portfolio manager") }));

    await waitFor(() => expect(deleteGrantMock).toHaveBeenCalledWith("g2"));
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByRole("button", { name: AVAILABILITY.REMOVE_ARIA("Developer") })).toBeDisabled(),
    );
    expect(screen.getByText(AVAILABILITY.LAST_AUDIENCE_LOCKED)).toBeInTheDocument();
  });

  it("state 6: a change in flight disables the whole control at once; only Add spins, after ~200ms", async () => {
    getAvailabilityMock.mockResolvedValue(view(false, [DEV]));
    let resolveAdd: () => void = () => {};
    upsertGrantMock.mockReturnValue(new Promise<void>((r) => (resolveAdd = r)));
    const { container } = render(policyControl());
    await screen.findByText("Developer");

    await userEvent.type(screen.getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER), "portfolio-manager");
    vi.useFakeTimers();
    try {
      act(() => screen.getByRole("button", { name: AVAILABILITY.ADD_CTA }).click());
      expect(first()).toBeDisabled();
      expect(only()).toBeDisabled();
      expect(screen.getByRole("button", { name: AVAILABILITY.REMOVE_ARIA("Developer") })).toBeDisabled();
      expect(screen.getByRole("button", { name: PERM.SUBJECT_GROUP })).toBeDisabled();
      expect(screen.getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER)).toBeDisabled();
      expect(screen.getByRole("button", { name: AVAILABILITY.ADD_CTA })).toBeDisabled();
      expect(container.querySelector(".animate-spin")).toBeNull();
      act(() => {
        vi.advanceTimersByTime(200);
      });
      // The label stays: the spinner sits beside it, no "Adding…" swap.
      const add = screen.getByRole("button", { name: AVAILABILITY.ADD_CTA });
      expect(add.querySelector(".animate-spin")).not.toBeNull();
    } finally {
      vi.useRealTimers();
    }
    await act(async () => resolveAdd());
  });

  it("state 6: a radio change shows the disabled state only, never a spinner", async () => {
    getAvailabilityMock.mockResolvedValue(view(false, [DEV]));
    putAvailabilityMock.mockReturnValue(new Promise(() => {}));
    const { container } = render(policyControl());
    await screen.findByText("Developer");

    await userEvent.click(only());
    await new Promise((r) => setTimeout(r, 250));
    expect(first()).toBeDisabled();
    expect(container.querySelector(".animate-spin")).toBeNull();
  });

  it("state 7: an add the server refuses shows its sentence under the adder, and what was typed stays", async () => {
    getAvailabilityMock.mockResolvedValue(view(false));
    upsertGrantMock.mockRejectedValue(new HttpError(400, UNKNOWN_TYPE));
    const { container } = render(policyControl());
    await screen.findByText(AVAILABILITY.LABEL);

    await userEvent.type(screen.getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER), "analyst");
    await userEvent.click(screen.getByRole("button", { name: AVAILABILITY.ADD_CTA }));

    const err = await screen.findByText(UNKNOWN_TYPE);
    expect(screen.getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER)).toHaveValue("analyst");
    // Under the adder, above the note — not a toast.
    const input = screen.getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER);
    const note = screen.getByText(AVAILABILITY.POLICY_NOTE);
    expect(input.compareDocumentPosition(err) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(err.compareDocumentPosition(note) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(container.querySelector("[data-sonner-toast]")).toBeNull();
  });

  it("state 7: a read that fails is one line where the control would be", async () => {
    getAvailabilityMock.mockRejectedValue(new HttpError(500, "boom"));
    const { container } = render(policyControl());

    expect(await screen.findByText(AVAILABILITY.LOAD_FAILED)).toBeInTheDocument();
    expect(container.textContent).toBe(AVAILABILITY.LOAD_FAILED);
  });

  // A person-plantable image ref like "../policy/<id>" must never reach the
  // router, which would clean it into another resource's availability.
  it("state 7: a value with a dot segment is one couldn't-load line, and no request goes out", async () => {
    const actual = await vi.importActual<typeof import("../../lib/api/permissions")>("../../lib/api/permissions");
    getAvailabilityMock.mockImplementation((k: string, v: string) => actual.permissions.getAvailability(k, v));
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    try {
      const { container } = render(<AvailabilityControl kind="image" value={`../policy/${ID}`} adminsOnly />);

      expect(await screen.findByText(AVAILABILITY.LOAD_FAILED)).toBeInTheDocument();
      expect(container.textContent).toBe(AVAILABILITY.LOAD_FAILED);
      expect(fetchMock).not.toHaveBeenCalled();
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("state 8: a security admin gets the working control", async () => {
    getAvailabilityMock.mockResolvedValue(view(true, [DEV, PM]));
    render(
      <OperatorProvider operator={false} securityOperator={true}>
        {policyControl()}
      </OperatorProvider>,
    );

    expect(await screen.findByText("Portfolio manager")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: AVAILABILITY.REMOVE_ARIA("Developer") })).toBeEnabled();
  });

  // GET /permissions/availability is securityOps: for anyone else it is a 403
  // and an authz.denied audit row, so a user-tier caller never reads it.
  it("state 8: anyone else gets nothing at all, and nothing is read", async () => {
    getAvailabilityMock.mockResolvedValue(view(false));
    const { container } = render(
      <OperatorProvider operator={false} securityOperator={false}>
        {policyControl()}
      </OperatorProvider>,
    );

    await Promise.resolve();
    expect(getAvailabilityMock).not.toHaveBeenCalled();
    expect(listUserTypesMock).not.toHaveBeenCalled();
    expect(container).toBeEmptyDOMElement();
  });

  it("a user type's id stands in for its name when the type list can't be read", async () => {
    listUserTypesMock.mockRejectedValue(new HttpError(500, "boom"));
    getAvailabilityMock.mockResolvedValue(view(false, [DEV]));
    render(policyControl());

    expect(await screen.findByText("developer")).toBeInTheDocument();
  });
});

describe("AvailabilityControl — the image family (decision 2)", () => {
  it("reads Admins only / Only these, with the image hint under the choice and the image lock", async () => {
    getAvailabilityMock.mockResolvedValue({ ...view(true, [DEV]), kind: "image", value: "ghcr.io/acme/dev-toolbox:1.4" });
    render(<AvailabilityControl kind="image" value="ghcr.io/acme/dev-toolbox:1.4" adminsOnly note={AVAILABILITY.IMAGE_NOTE} />);

    const remove = await screen.findByRole("button", { name: AVAILABILITY.REMOVE_ARIA("Developer") });
    expect(screen.getByRole("radio", { name: AVAILABILITY.ADMINS_ONLY })).not.toBeChecked();
    expect(screen.queryByRole("radio", { name: AVAILABILITY.EVERYONE })).not.toBeInTheDocument();
    expect(screen.getByTestId("availability-only").textContent).toBe(AVAILABILITY.ONLY);
    expect(screen.getByText(AVAILABILITY.IMAGE_HINT)).toBeInTheDocument();
    expect(screen.getByText(AVAILABILITY.IMAGE_NOTE)).toBeInTheDocument();
    expect(remove).toHaveAttribute("title", AVAILABILITY.LAST_AUDIENCE_LOCKED_IMAGE);
    expect(screen.getByText(AVAILABILITY.LAST_AUDIENCE_LOCKED_IMAGE)).toBeInTheDocument();
    expect(screen.queryByText(AVAILABILITY.LAST_AUDIENCE_LOCKED)).not.toBeInTheDocument();
    expect(getAvailabilityMock).toHaveBeenCalledWith("image", "ghcr.io/acme/dev-toolbox:1.4");
  });
});

describe("AvailabilityDraft + writeAvailability — a creation form (decision 4)", () => {
  function Harness({ onDraft }: { onDraft: (d: AvailabilityDraftValue) => void }) {
    const [draft, setDraft] = React.useState<AvailabilityDraftValue>({ restricted: false, audiences: [] });
    return (
      <AvailabilityDraft
        draft={draft}
        onChange={(d) => {
          setDraft(d);
          onDraft(d);
        }}
        adminsOnly
      />
    );
  }

  it("holds the choice locally and writes nothing until the resource exists", async () => {
    const onDraft = vi.fn();
    render(<Harness onDraft={onDraft} />);

    expect(screen.getByRole("radio", { name: AVAILABILITY.ADMINS_ONLY })).toBeChecked();
    await userEvent.type(screen.getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER), "portfolio-manager");
    await userEvent.click(screen.getByRole("button", { name: AVAILABILITY.ADD_CTA }));
    expect(await screen.findByText("Portfolio manager")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("radio", { name: AVAILABILITY.ONLY }));

    expect(onDraft).toHaveBeenLastCalledWith({
      restricted: true,
      audiences: [{ subject_type: "user_type", subject: "portfolio-manager" }],
    });
    expect(upsertGrantMock).not.toHaveBeenCalled();
    expect(putAvailabilityMock).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: AVAILABILITY.REMOVE_ARIA("Portfolio manager") })).toBeDisabled();
  });

  it("writes the list first, then Only these", async () => {
    const calls: string[] = [];
    upsertGrantMock.mockImplementation(async (g: { subject: string }) => calls.push(`grant ${g.subject}`));
    putAvailabilityMock.mockImplementation(async () => calls.push("only"));

    await writeAvailability("image", "ghcr.io/acme/x:1", {
      restricted: true,
      audiences: [
        { subject_type: "user_type", subject: "developer" },
        { subject_type: "group", subject: "Data engineering" },
      ],
    });

    expect(calls).toEqual(["grant developer", "grant Data engineering", "only"]);
    expect(putAvailabilityMock).toHaveBeenCalledWith("image", "ghcr.io/acme/x:1", true);
  });

  it("leaves the first choice alone when Only these wasn't chosen", async () => {
    upsertGrantMock.mockResolvedValue(undefined);
    await writeAvailability("image", "ghcr.io/acme/x:1", {
      restricted: false,
      audiences: [{ subject_type: "user", subject: "alice@corp.com" }],
    });
    expect(upsertGrantMock).toHaveBeenCalledTimes(1);
    expect(putAvailabilityMock).not.toHaveBeenCalled();
  });
});
