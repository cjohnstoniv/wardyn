/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { OperatorProvider } from "../../../../wardyn/operator-context";
import type { UIApp } from "../../../../../lib/types/runs";
import { CallersEditor } from "./callers-editor";
import { groupCaller, personCaller, serviceCaller, userCaller } from "./callers-model";

const api = vi.hoisted(() => ({
  caps: vi.fn(),
  mine: vi.fn(),
  search: vi.fn(),
}));
vi.mock("../../../../../lib/api/permissions", () => ({ permissions: { getMyCapabilities: api.caps } }));
vi.mock("../../../../../lib/api/service-identities", () => ({ serviceIdentities: { listMine: api.mine } }));
vi.mock("../../../../../lib/api/directory", async (orig) => ({
  ...(await orig<typeof import("../../../../../lib/api/directory")>()),
  directory: { search: api.search },
}));

function Harness({ first, security = false }: { first: string[]; security?: boolean }) {
  const [callers, setCallers] = React.useState<string[]>(first);
  const app: UIApp = { name: "api", port: 8080, path: "/", inbound: true, callers: callers.length ? callers : undefined };
  return (
    <OperatorProvider operator={security} securityOperator={security}>
      <CallersEditor app={app} index={0} who="api" onChange={setCallers} />
      <p data-testid="callers">{JSON.stringify(callers)}</p>
    </OperatorProvider>
  );
}
const callersNow = (): string[] => JSON.parse(screen.getByTestId("callers").textContent ?? "[]");
const open = async () => {
  await userEvent.click(screen.getByRole("button", { name: NR_RUN.CALLERS_ADD }));
  return screen.findByText(NR_RUN.CALLERS_MENU_GROUPS);
};

beforeEach(() => {
  api.caps.mockResolvedValue({ session_groups: ["Payments", "platform", "oncall"] });
  api.mine.mockResolvedValue([
    { name: "billing", state: "active" },
    { name: "ledger-sync", state: "active" },
    { name: "old-job", state: "retired" },
  ]);
  api.search.mockResolvedValue([]);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// The approved mock (r17 T2-a): strings spelled out so a reword fails.
describe("callers canon strings", () => {
  it("are the board's", () => {
    expect(NR_RUN.CALLERS_LABEL).toBe("Who can call it");
    expect(NR_RUN.CALLERS_HELP).toBe("You can always call it. Add the people, groups and service identities that may call it too.");
    expect(NR_RUN.CALLERS_NONE).toBe("Only you can call it.");
    expect(NR_RUN.CALLERS_ADD).toBe("Add a caller");
    expect([NR_RUN.CALLER_PERSON, NR_RUN.CALLER_GROUP, NR_RUN.CALLER_SERVICE]).toEqual(["Person", "Group", "Service identity"]);
    expect([NR_RUN.CALLERS_MENU_DIRECTORY, NR_RUN.CALLERS_MENU_GROUPS, NR_RUN.CALLERS_MENU_SERVICES, NR_RUN.CALLERS_MENU_PERSON]).toEqual([
      "Search the directory", "Your groups", "Services you sponsor", "A person",
    ]);
    expect(NR_RUN.CALLERS_NO_GROUPS).toBe("Your sign-in doesn't list any groups.");
    expect(NR_RUN.CALLERS_NO_SERVICES).toBe("You don't sponsor any service identities.");
    expect(NR_RUN.CALLER_EMAIL_INVALID).toBe("Enter a full email address, like ana@example.com.");
    expect(NR_RUN.CALLER_REMOVE_ARIA("payments")).toBe("Remove payments");
    expect(NR_RUN.PORT_INBOUND_REFUSED).toBe("Your organisation hasn't turned on inbound access for you.");
  });
});

describe("callers model", () => {
  it("writes the server's grammar: lower-case, prefixed, and nothing it would refuse", () => {
    expect(personCaller(" Li.Wei@Example.com ")).toBe("user:li.wei@example.com");
    expect(personCaller("Ana@Example.com")).toBe("user:ana@example.com");
    expect(personCaller(" Ünal@example.com ")).toBe("user:Ünal@example.com");
    expect(personCaller("ana@")).toBeUndefined();
    expect(personCaller("ana example@x.com")).toBeUndefined();
    expect(groupCaller("Payments Team")).toBe("group:payments team");
    expect(groupCaller("équipe")).toBeUndefined();
    expect(serviceCaller("billing")).toBe("service:billing");
    expect(serviceCaller("ab")).toBeUndefined();
    expect(userCaller("PAULA.NG@example.com")).toBe("user:paula.ng@example.com");
    expect(userCaller("Ünal@example.com")).toBe("user:Ünal@example.com");
    expect(userCaller("two words")).toBeUndefined();
  });
});

describe("CallersEditor", () => {
  it("says only the owner can call it when no caller is chosen, and shows the help once one is", async () => {
    render(<Harness first={[]} />);
    expect(screen.getByText(NR_RUN.CALLERS_LABEL)).toBeInTheDocument();
    expect(screen.getByText(NR_RUN.CALLERS_NONE)).toBeInTheDocument();
    await open();
    await userEvent.type(screen.getByLabelText(NR_RUN.CALLERS_MENU_PERSON), "li.wei@example.com");
    await userEvent.click(screen.getByRole("button", { name: NR_RUN.CALLER_ADD_PERSON }));
    expect(screen.getByText(NR_RUN.CALLERS_HELP)).toBeInTheDocument();
    expect(screen.queryByText(NR_RUN.CALLERS_NONE)).not.toBeInTheDocument();
  });

  it("draws a chip per caller with its kind and removes one", async () => {
    render(<Harness first={["group:payments", "service:billing", "user:li.wei@example.com"]} />);
    for (const value of ["payments", "billing", "li.wei@example.com"]) expect(screen.getByText(value)).toBeInTheDocument();
    for (const kind of [NR_RUN.CALLER_GROUP, NR_RUN.CALLER_SERVICE, NR_RUN.CALLER_PERSON]) expect(screen.getByText(kind)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: NR_RUN.CALLER_REMOVE_ARIA("billing") }));
    expect(callersNow()).toEqual(["group:payments", "user:li.wei@example.com"]);
  });

  it("lists the person's groups and the services they sponsor, active ones only, and adds with the prefix", async () => {
    render(<Harness first={["group:payments"]} />);
    await open();
    await waitFor(() => expect(screen.getByRole("button", { name: "platform" })).toBeInTheDocument());
    expect(within(screen.getByRole("dialog")).getByRole("button", { name: /payments/ })).toBeDisabled();
    expect(screen.getByText(NR_RUN.CALLER_ADDED)).toBeInTheDocument();
    expect(screen.queryByText("old-job")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "platform" }));
    await userEvent.click(screen.getByRole("button", { name: "ledger-sync" }));
    expect(callersNow()).toEqual(["group:payments", "group:platform", "service:ledger-sync"]);
  });

  it("says so when the sign-in lists no groups and nothing is sponsored", async () => {
    api.caps.mockResolvedValue({ session_groups: [] });
    api.mine.mockResolvedValue([]);
    render(<Harness first={[]} />);
    await open();
    expect(await screen.findByText(NR_RUN.CALLERS_NO_GROUPS)).toBeInTheDocument();
    expect(await screen.findByText(NR_RUN.CALLERS_NO_SERVICES)).toBeInTheDocument();
  });

  it("says nothing about a list that failed to load", async () => {
    api.caps.mockRejectedValue(new Error("down"));
    render(<Harness first={[]} />);
    await open();
    await waitFor(() => expect(api.mine).toHaveBeenCalled());
    expect(screen.queryByText(NR_RUN.CALLERS_NO_GROUPS)).not.toBeInTheDocument();
  });

  it("adds a person by email as user:, lower-cased, and refuses a half address with the board's sentence", async () => {
    render(<Harness first={[]} />);
    await open();
    const field = screen.getByLabelText(NR_RUN.CALLERS_MENU_PERSON);
    await userEvent.type(field, "ana@");
    await userEvent.click(screen.getByRole("button", { name: NR_RUN.CALLER_ADD_PERSON }));
    expect(screen.getByRole("alert")).toHaveTextContent(NR_RUN.CALLER_EMAIL_INVALID);
    expect(field).toHaveAttribute("aria-invalid", "true");
    expect(callersNow()).toEqual([]);
    await userEvent.type(field, "Example.com");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    await userEvent.type(field, "{Enter}");
    expect(callersNow()).toEqual(["user:ana@example.com"]);
  });

  it("does not add a person twice", async () => {
    render(<Harness first={["user:ana@example.com"]} />);
    await open();
    await userEvent.type(screen.getByLabelText(NR_RUN.CALLERS_MENU_PERSON), "ana@example.com{Enter}");
    expect(callersNow()).toEqual(["user:ana@example.com"]);
  });

  it("stops at 32 callers", async () => {
    render(<Harness first={Array.from({ length: 32 }, (_, i) => `group:g${i}`)} />);
    expect(screen.getByRole("button", { name: NR_RUN.CALLERS_ADD })).toBeDisabled();
  });

  it("gives a member no directory search", async () => {
    render(<Harness first={[]} />);
    await open();
    expect(screen.queryByText(NR_RUN.CALLERS_MENU_DIRECTORY)).not.toBeInTheDocument();
    expect(api.search).not.toHaveBeenCalled();
  });

  it("gives a security admin a directory search, shows a result by its name and stores its claim value", async () => {
    api.search.mockResolvedValue([
      { display_name: "Payments Team", claim_value: "3F2A-GUID", kind: "group" },
      { display_name: "Paula Ng", claim_value: "Paula.Ng@example.com", kind: "user", detail: "paula.ng@example.com" },
    ]);
    render(<Harness first={[]} security />);
    await open();
    await userEvent.type(screen.getByLabelText(NR_RUN.CALLERS_MENU_DIRECTORY), "pay");
    const team = await screen.findByRole("button", { name: /Payments Team/ });
    expect(api.search).toHaveBeenCalledWith("pay");
    await userEvent.click(team);
    await userEvent.click(screen.getByRole("button", { name: /Paula Ng/ }));
    expect(callersNow()).toEqual(["group:3f2a-guid", "user:paula.ng@example.com"]);
    const chip = document.querySelector("[data-caller='group:3f2a-guid']") as HTMLElement;
    expect(within(chip).getByText("Payments Team")).toBeInTheDocument();
  });

  it("draws no directory section on a deployment with no directory", async () => {
    api.search.mockResolvedValue(null);
    render(<Harness first={[]} security />);
    await open();
    await userEvent.type(screen.getByLabelText(NR_RUN.CALLERS_MENU_DIRECTORY), "pay");
    await waitFor(() => expect(screen.queryByText(NR_RUN.CALLERS_MENU_DIRECTORY)).not.toBeInTheDocument());
  });
});
