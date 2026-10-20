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
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import * as React from "react";
import { HttpError, PendingChangeError } from "../../lib/api/core";
import { CHANGES } from "../../lib/governance-copy";
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
    // #3065: the picker belongs to Only these; under Everyone there is nothing to add to.
    expect(screen.queryByTestId("availability-audience")).toBeNull();
    expect(screen.queryByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER)).toBeNull();
    expect(screen.queryByRole("button", { name: AVAILABILITY.ADD_CTA })).toBeNull();
  });

  it("state 1b: Only these is an indented sub-row with the adder, per #919", async () => {
    getAvailabilityMock.mockResolvedValue(view(true, [DEV]));
    render(policyControl());

    const sub = await screen.findByTestId("availability-audience");
    for (const seg of [PERM.SUBJECT_USER_TYPE, PERM.SUBJECT_GROUP, PERM.SUBJECT_USER]) {
      expect(within(sub).getByRole("button", { name: seg })).toBeInTheDocument();
    }
    expect(within(sub).getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER)).toHaveAccessibleName(AVAILABILITY.HINT_USER_TYPE);
    expect(within(sub).getByRole("button", { name: AVAILABILITY.ADD_CTA })).toBeDisabled();
    expect(within(sub).getByText("Developer")).toBeInTheDocument();
  });

  // #3065: hiding the audiences under Everyone never drops them; going back to Only these shows the same ones.
  it("Everyone hides stored audiences without deleting them, and Only these brings them back", async () => {
    getAvailabilityMock.mockResolvedValue(view(true, [DEV, DATA]));
    putAvailabilityMock.mockImplementation((_k: string, _v: string, restricted: boolean) => Promise.resolve(view(restricted, [DEV, DATA])));
    const user = userEvent.setup();
    render(policyControl());
    await screen.findByTestId("availability-audience");

    await user.click(first());
    await waitFor(() => expect(putAvailabilityMock).toHaveBeenLastCalledWith(KIND, ID, false));
    await waitFor(() => expect(screen.queryByTestId("availability-audience")).toBeNull());

    await user.click(only());
    await waitFor(() => expect(putAvailabilityMock).toHaveBeenLastCalledWith(KIND, ID, true));
    const sub = await screen.findByTestId("availability-audience");
    expect(within(sub).getByText("Developer")).toBeInTheDocument();
    expect(within(sub).getByText(/Data engineering/)).toBeInTheDocument();
    expect(deleteGrantMock).not.toHaveBeenCalled();
  });

  it("a draft saved on Everyone still writes every audience it holds", async () => {
    upsertGrantMock.mockResolvedValue({});
    await writeAvailability(KIND, ID, { restricted: false, audiences: [{ subject_type: "group", subject: "Data engineering" }] } as AvailabilityDraftValue);
    expect(upsertGrantMock).toHaveBeenCalledWith(expect.objectContaining({ subject_type: "group", subject: "Data engineering", effect: "allow" }));
    expect(putAvailabilityMock).not.toHaveBeenCalled();
  });

  // #2874: the placeholder is prose in the sans face and the input is wide enough to show it whole; typed ids stay mono.
  it("sets the adder's placeholder in sans inside a mono input that fits it", async () => {
    getAvailabilityMock.mockResolvedValue(view(true, [DEV]));
    render(policyControl());
    const input = await screen.findByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER);
    expect(input).toHaveClass("font-mono", "placeholder:font-sans", "max-w-[280px]");
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

  it("state 2: the first Add under a pending Only these writes the allow row, then restricts", async () => {
    const calls: string[] = [];
    getAvailabilityMock.mockResolvedValue(view(false));
    upsertGrantMock.mockImplementation(async () => {
      calls.push("grant");
      return { grant: DEV, updated: false };
    });
    putAvailabilityMock.mockImplementation(async () => {
      calls.push("only");
      return view(true, [DEV]);
    });
    render(policyControl());
    await screen.findByText(AVAILABILITY.LABEL);

    await userEvent.click(only());
    await userEvent.type(screen.getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER), "developer");
    await userEvent.click(screen.getByRole("button", { name: AVAILABILITY.ADD_CTA }));

    await waitFor(() => expect(putAvailabilityMock).toHaveBeenCalledWith(KIND, ID, true));
    expect(upsertGrantMock).toHaveBeenCalledWith({
      subject_type: "user_type",
      subject: "developer",
      capability: KIND,
      value: ID,
      effect: "allow",
    });
    expect(calls).toEqual(["grant", "only"]);
    await waitFor(() => expect(screen.queryByTestId("availability-pending")).toBeNull());
    expect(only()).toBeChecked();
  });

  it("state 3: Only these with nobody listed is a pending state, not a refusal: nothing is sent", async () => {
    getAvailabilityMock.mockResolvedValue(view(false));
    render(policyControl());
    await screen.findByText(AVAILABILITY.LABEL);

    await userEvent.click(only());

    expect(await screen.findByTestId("availability-pending")).toHaveTextContent(AVAILABILITY.PENDING_NOTE);
    expect(only()).toBeChecked();
    expect(putAvailabilityMock).not.toHaveBeenCalled();
    expect(screen.queryByText(EMPTY_ONLY)).not.toBeInTheDocument();

    await userEvent.click(first());
    expect(screen.queryByTestId("availability-audience")).toBeNull();
    expect(putAvailabilityMock).not.toHaveBeenCalled();
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
    getAvailabilityMock.mockResolvedValue(view(true, [DEV]));
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
    await screen.findByText(AVAILABILITY.LABEL);

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
    await userEvent.click(only());

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
    getAvailabilityMock.mockResolvedValue(view(true, [DEV]));
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
    await userEvent.click(screen.getByRole("radio", { name: AVAILABILITY.ONLY }));
    await userEvent.type(screen.getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER), "portfolio-manager");
    await userEvent.click(screen.getByRole("button", { name: AVAILABILITY.ADD_CTA }));
    expect(await screen.findByText("Portfolio manager")).toBeInTheDocument();

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

// 0.8.6 four-eyes: a change the server holds for a second person (202) is "submitted", never applied.
describe("AvailabilityControl — a 202 is submitted, never saved", () => {
  const held = () =>
    new PendingChangeError({
      id: "c1",
      target_kind: "capability_availability",
      op: "set",
      target_key: `${KIND}/${ID}`,
      state: "pending",
      proposed_by: "ana",
      proposed_at: aheadByHours(-1),
      expires_at: aheadByHours(20),
      diff: { changed: ["restricted"] },
    });

  it("a choice held for approval shows the note, no error, and stays on Everyone", async () => {
    getAvailabilityMock.mockResolvedValue(view(false, [DEV]));
    putAvailabilityMock.mockRejectedValue(held());
    render(policyControl());
    await screen.findByText(AVAILABILITY.LABEL);

    await userEvent.click(only());

    expect(await screen.findByText(CHANGES.SUBMITTED_TITLE)).toBeInTheDocument();
    expect(first()).toBeChecked();
  });

  it("an add held for approval shows the note and reads the list again", async () => {
    getAvailabilityMock.mockResolvedValue(view(false));
    upsertGrantMock.mockRejectedValue(held());
    render(policyControl());
    await screen.findByText(AVAILABILITY.LABEL);
    getAvailabilityMock.mockClear();
    await userEvent.click(only());

    await userEvent.type(screen.getByPlaceholderText(AVAILABILITY.ADD_PLACEHOLDER), "developer");
    await userEvent.click(screen.getByRole("button", { name: AVAILABILITY.ADD_CTA }));

    expect(await screen.findByText(CHANGES.SUBMITTED_TITLE)).toBeInTheDocument();
    await waitFor(() => expect(getAvailabilityMock).toHaveBeenCalled());
  });

  it("a remove held for approval shows the note", async () => {
    getAvailabilityMock.mockResolvedValue(view(true, [DEV, PM]));
    deleteGrantMock.mockRejectedValue(held());
    render(policyControl());

    await userEvent.click(await screen.findByRole("button", { name: AVAILABILITY.REMOVE_ARIA("Portfolio manager") }));

    expect(await screen.findByText(CHANGES.SUBMITTED_TITLE)).toBeInTheDocument();
  });
});
