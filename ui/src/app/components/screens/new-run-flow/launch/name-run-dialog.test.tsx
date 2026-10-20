/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { HttpError } from "../../../../lib/api/core";
import { runs } from "../../../../lib/api/runs";
import { scmAccess } from "../../../../lib/api/scm-access";
import { SCENARIO_PREFLIGHT } from "../../../../lib/new-run-fixtures/scenarios/preflight";
import { ADO } from "../../../../lib/ado-entra-copy";
import { ADO_PAT } from "../../../../lib/ado-pat-copy";
import { POLICY_REMEDY } from "../../../../lib/governance-copy";
import type { PreflightResult } from "../../../../lib/types/preflight";
import { NR_LAUNCH } from "../../../wardyn/copy/new-run/launch";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { OperatorProvider } from "../../../wardyn/operator-context";
import type { RunIntent } from "../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../new-run-route";
import { NameRunDialog, summaryLine } from "./name-run-dialog";

const door = vi.hoisted(() => ({ openDoor: vi.fn(), refresh: vi.fn() }));
const shell = vi.hoisted(() => ({ status: {} as Record<string, unknown> }));
vi.mock("../../../wardyn/model-access-context", () => ({ useModelAccessDoor: () => door, useShellSetupStatus: () => ({ status: shell.status }) }));
// The connect popup, as the dialog sees it: busy while it is out, then the answer.
const adoConnect = vi.hoisted(() => ({ calls: 0, answer: true as boolean | null }));
vi.mock("../../../../lib/hooks/use-ado-connect", async () => {
  const R = await import("react");
  return {
    useAdoConnect: () => {
      const [connecting, setConnecting] = R.useState(false);
      const cancel = R.useCallback(() => {}, []);
      const connect = async () => {
        adoConnect.calls += 1;
        setConnecting(true);
        await Promise.resolve();
        setConnecting(false);
        return adoConnect.answer;
      };
      return { connecting, connect, connectFallback: connect, cancel, blockedUrl: null };
    },
  };
});

const INTENT: RunIntent = {
  shape: "environment", experience: "interactive", no_repositories_or_drives: true, ends_after_sec: 3600,
};
const LIFETIME = { default: { value: 3600 }, cap: { value: 7200 } };
const CLEAR = { ...SCENARIO_PREFLIGHT.member, limits: { lifetime_sec: LIFETIME } } as unknown as PreflightResult;
const preflight = vi.fn();
const create = vi.fn();
const onCreated = vi.fn();
const onOpenChange = vi.fn();

function Seeded({ children }: { children: React.ReactNode }) {
  const dispatch = useNewRunDispatch();
  const draft = useNewRunDraft();
  React.useEffect(() => dispatch({ type: "seed", origin: { kind: "scratch" }, intent: INTENT }), [dispatch]);
  return draft.intent === INTENT ? <>{children}</> : null;
}
function Typed() {
  const { launch } = useNewRunDraft();
  return <p data-testid="typed">{JSON.stringify(launch)}</p>;
}

function open() {
  render(
    <MemoryRouter>
      <OperatorProvider operator={false} principal="alice">
        <NewRunDraftProvider owner="alice">
          <Seeded>
            <NameRunDialog open onOpenChange={onOpenChange} onCreated={onCreated} />
            <Typed />
          </Seeded>
        </NewRunDraftProvider>
      </OperatorProvider>
    </MemoryRouter>,
  );
}
const dialog = () => screen.findByRole("dialog", { name: NR_LAUNCH.TITLE });
const launchButton = () => screen.getByRole("button", { name: new RegExp(`^${NR_LAUNCH.LAUNCH}$`) });
const refusal = (status: number, message: string, reason = "", provider = "") =>
  Object.assign(new HttpError(status, message, reason), { provider });

beforeEach(() => {
  preflight.mockReset().mockResolvedValue(CLEAR);
  create.mockReset().mockResolvedValue({ id: "run-1", state: "PENDING" });
  onCreated.mockReset();
  onOpenChange.mockReset();
  door.openDoor.mockReset();
  door.refresh.mockReset();
  shell.status = {};
  adoConnect.calls = 0;
  adoConnect.answer = true;
  vi.spyOn(runs, "preflightRunWire").mockImplementation(preflight);
  vi.spyOn(runs, "previewRunPolicyWire").mockResolvedValue({} as never);
  vi.spyOn(runs, "createRunWire").mockImplementation(create);
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("Name this run: the fresh check (U12)", () => {
  it("opening asks the server again and says Checking until it answers; Title has focus", async () => {
    let answer!: (r: PreflightResult) => void;
    preflight.mockReturnValue(new Promise<PreflightResult>((yes) => { answer = yes; }));
    open();
    const box = await dialog();
    await waitFor(() => expect(preflight).toHaveBeenCalledTimes(1));
    expect(preflight.mock.calls[0][0]).toMatchObject({ mode_contract: "0.9", shape: "environment" });
    expect(preflight.mock.calls[0][0]).not.toHaveProperty("title");
    expect(within(box).getByText(NR_LAUNCH.CHECKING)).toBeInTheDocument();
    expect(within(box).queryByText(NR_LAUNCH.CHECKED_JUST_NOW)).not.toBeInTheDocument();
    expect(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL)).toHaveFocus();
    await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "nightly");
    expect(launchButton()).toBeDisabled();

    await act(async () => answer(CLEAR));
    expect(await within(box).findByText(NR_LAUNCH.CHECKED_JUST_NOW)).toBeInTheDocument();
    expect(within(box).getByText(NR_LAUNCH.SUMMARY("General", "2", "1 hour"))).toBeInTheDocument();
    expect(within(box).getByText("Wall")).toBeInTheDocument();
    expect(within(box).queryByText(NR_LAUNCH.CHECKING)).not.toBeInTheDocument();
    expect(launchButton()).toBeEnabled();
  });

  it("a check that is refused shows its sentence, never Checked just now, and holds Launch", async () => {
    preflight.mockRejectedValue(refusal(422, "General is switched off right now.", "runner_pool_unavailable"));
    open();
    const box = await dialog();
    await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "x");
    expect(await within(box).findByRole("alert")).toHaveTextContent("General is switched off right now.");
    expect(within(box).queryByText(NR_LAUNCH.CHECKED_JUST_NOW)).not.toBeInTheDocument();
    expect(within(box).queryByText(NR_LAUNCH.CHECKING)).not.toBeInTheDocument();
    expect(launchButton()).toBeDisabled();
    await userEvent.click(within(box).getByRole("button", { name: NR_REVIEW.CHECK_AGAIN }));
    await waitFor(() => expect(preflight).toHaveBeenCalledTimes(2));
  });

  it.each([[500, "The server could not answer."], [429, "Too many checks."]])(
    "a check that failed or was rate-limited (%i) leaves Launch available: the create is the authority (FR-0100)",
    async (status, message) => {
      preflight.mockRejectedValue(refusal(status, message));
      open();
      const box = await dialog();
      await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "x");
      await waitFor(() => expect(within(box).queryByText(NR_LAUNCH.CHECKING)).not.toBeInTheDocument());
      expect(launchButton()).toBeEnabled();
    },
  );

  it("Launch needs a title", async () => {
    open();
    const box = await dialog();
    await within(box).findByText(NR_LAUNCH.CHECKED_JUST_NOW);
    expect(launchButton()).toBeDisabled();
    await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "  ");
    expect(launchButton()).toBeDisabled();
  });

  it("Cancel closes without creating", async () => {
    open();
    const box = await dialog();
    await userEvent.click(within(box).getByRole("button", { name: NR_LAUNCH.CANCEL }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(create).not.toHaveBeenCalled();
  });
});

describe("Name this run: summary", () => {
  it("leaves out a part the server did not give", () => {
    expect(summaryLine({ ...CLEAR, runner_pool: undefined } as never, INTENT)).toBe("2 CPU · 1 hour");
    expect(summaryLine({ ...CLEAR, resources_applied: undefined } as never, INTENT)).toBe("General · 1 hour");
  });

  it("never falls back to what the draft asked for", () => {
    const asked: RunIntent = { ...INTENT, resources: { cpu_millis: 8000 }, ends_after_sec: undefined };
    const bare = { ...CLEAR, resources_applied: undefined, limits: undefined } as never;
    expect(summaryLine(bare, asked)).toBe("General");
    expect(summaryLine(bare, INTENT)).toBe("General");
  });

  it("holds the asked lifetime to the answer's cap, and uses the answer's default when nothing was asked", () => {
    expect(summaryLine(CLEAR, { ...INTENT, ends_after_sec: 36_000 })).toBe("General · 2 CPU · 2 hours");
    expect(summaryLine(CLEAR, { ...INTENT, ends_after_sec: undefined })).toBe("General · 2 CPU · 1 hour");
  });

  it("says No end when it was asked for and the answer allows it, and the finite default when it does not (DR-0245)", () => {
    const allowed = { ...CLEAR, limits: { lifetime_sec: LIFETIME, no_end_allowed: true } } as unknown as PreflightResult;
    expect(summaryLine(allowed, { ...INTENT, ends_after_sec: undefined, no_end: true })).toBe("General · 2 CPU · No end");
    expect(summaryLine(CLEAR, { ...INTENT, ends_after_sec: undefined, no_end: true })).toBe("General · 2 CPU · 1 hour");
  });
});

describe("Name this run: why Launch is held", () => {
  it("points the held button at the Required tag, then at Checking, then at the refusal", async () => {
    let answer!: (r: PreflightResult) => void;
    preflight.mockReturnValue(new Promise<PreflightResult>((yes) => { answer = yes; }));
    open();
    const box = await dialog();
    const described = () => document.getElementById(launchButton().getAttribute("aria-describedby") ?? "");
    expect(described()).toHaveTextContent("Required");
    await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "x");
    expect(described()).toHaveTextContent(NR_LAUNCH.CHECKING);
    await act(async () => answer(CLEAR));
    await within(box).findByText(NR_LAUNCH.CHECKED_JUST_NOW);
    expect(launchButton()).toBeEnabled();
    expect(launchButton()).not.toHaveAttribute("aria-describedby");
  });

  it("points a refusal-held button at the refusal sentence", async () => {
    preflight.mockRejectedValue(refusal(422, "General is switched off right now.", "runner_pool_unavailable"));
    open();
    const box = await dialog();
    await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "x");
    await within(box).findByRole("alert");
    expect(document.getElementById(launchButton().getAttribute("aria-describedby") ?? "")).toHaveTextContent("General is switched off right now.");
  });
});

describe("Name this run: Launch", () => {
  it("creates once with the title and description, and hands the run on", async () => {
    open();
    const box = await dialog();
    await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "  nightly ");
    await userEvent.type(within(box).getByLabelText(NR_LAUNCH.DESCRIPTION_LABEL), "for the team");
    await within(box).findByText(NR_LAUNCH.CHECKED_JUST_NOW);
    await userEvent.click(launchButton());
    await waitFor(() => expect(onCreated).toHaveBeenCalledWith({ id: "run-1", state: "PENDING" }));
    expect(create).toHaveBeenCalledTimes(1);
    expect(create.mock.calls[0][0]).toMatchObject({ mode_contract: "0.9", shape: "environment", title: "nightly", description: "for the team" });
  });
});

describe("Name this run: a create the check did not foresee", () => {
  const BREACH = "this run needs 2 CPU, more than quota runs-quota has left. Stop a run, or ask your admin to raise the quota.";

  it("shows the server's sentence in the dialog, keeps the typed text and lets Launch be pressed again", async () => {
    create.mockRejectedValueOnce(refusal(422, BREACH, "namespace_quota_exceeded"));
    open();
    const box = await dialog();
    await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "quota refusal");
    await userEvent.type(within(box).getByLabelText(NR_LAUNCH.DESCRIPTION_LABEL), "why");
    await within(box).findByText(NR_LAUNCH.CHECKED_JUST_NOW);
    await userEvent.click(launchButton());
    expect(await within(box).findByRole("alert")).toHaveTextContent(BREACH);
    expect(within(box).queryByTestId("ado-launch-note")).not.toBeInTheDocument();
    expect(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL)).toHaveValue("quota refusal");
    expect(within(box).getByLabelText(NR_LAUNCH.DESCRIPTION_LABEL)).toHaveValue("why");
    expect(screen.getByTestId("typed")).toHaveTextContent('"title":"quota refusal"');
    expect(onCreated).not.toHaveBeenCalled();
    expect(launchButton()).toBeEnabled();

    await userEvent.click(launchButton());
    await waitFor(() => expect(onCreated).toHaveBeenCalledTimes(1));
    expect(within(box).queryByRole("alert")).not.toBeInTheDocument();
  });

  it("a refusal that names its policy says who owns it and where to ask, under the sentence", async () => {
    const policy = { source: "profile", layer: "profile", name: "Contractors", owner: "Platform Security", request_url: "https://example.com/access" };
    create.mockRejectedValueOnce(Object.assign(refusal(422, BREACH, "run_quota"), { policy }));
    open();
    const box = await dialog();
    await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "policy refusal");
    await within(box).findByText(NR_LAUNCH.CHECKED_JUST_NOW);
    await userEvent.click(launchButton());
    const alert = await within(box).findByRole("alert");
    expect(alert).toHaveTextContent(BREACH);
    const card = within(alert).getByTestId("policy-remedy");
    expect(card).toHaveTextContent("Set byContractors walls");
    expect(card).toHaveTextContent("OwnerPlatform Security");
    expect(card).not.toHaveTextContent("Can change");
    expect(within(alert).getByRole("link", { name: new RegExp(POLICY_REMEDY.LINK) })).toHaveAttribute("href", policy.request_url);
  });

  it("a model_credential refusal that names its provider opens that provider's sign-in, which re-checks", async () => {
    create.mockRejectedValueOnce(refusal(422, "You are not signed in.", "model_credential", "bedrock-e2e"));
    open();
    const box = await dialog();
    await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "model access");
    await within(box).findByText(NR_LAUNCH.CHECKED_JUST_NOW);
    await userEvent.click(launchButton());
    await waitFor(() => expect(door.openDoor).toHaveBeenCalledTimes(1));
    const opened = door.openDoor.mock.calls[0][0];
    expect(opened.for).toEqual({ provider: "bedrock-e2e" });
    expect(await within(box).findByRole("alert")).toHaveTextContent("You are not signed in.");

    preflight.mockClear();
    await act(async () => opened.onSignedIn());
    expect(preflight).toHaveBeenCalledTimes(1);
    expect(create).toHaveBeenCalledTimes(1);
  });

  it("a refusal that names no provider opens no sign-in", async () => {
    create.mockRejectedValueOnce(refusal(422, "this run's policy does not allow it"));
    open();
    const box = await dialog();
    await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "plain");
    await within(box).findByText(NR_LAUNCH.CHECKED_JUST_NOW);
    await userEvent.click(launchButton());
    expect(await within(box).findByRole("alert")).toHaveTextContent("this run's policy does not allow it");
    expect(door.openDoor).not.toHaveBeenCalled();
  });

  it("a launch the organisation's token policy refuses says so under the sentence, with no Connect", async () => {
    create.mockRejectedValueOnce(refusal(403, BREACH, "ado_pat_policy_blocked"));
    open();
    const box = await dialog();
    await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "ado blocked");
    await within(box).findByText(NR_LAUNCH.CHECKED_JUST_NOW);
    expect(within(box).queryByTestId("ado-launch-note")).not.toBeInTheDocument();
    await userEvent.click(launchButton());
    expect(await within(box).findByTestId("ado-launch-note")).toHaveTextContent(ADO_PAT.LAUNCH_POLICY_REFUSED);
    expect(within(box).queryByRole("button", { name: ADO_PAT.MEMBER_CONNECT })).not.toBeInTheDocument();
  });

  it("when the server's sentence is the blocked note's own, the dialog says it once, as the refusal", async () => {
    create.mockRejectedValueOnce(refusal(403, ADO_PAT.LAUNCH_POLICY_REFUSED, "ado_pat_policy_blocked"));
    open();
    const box = await dialog();
    await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "ado blocked once");
    await within(box).findByText(NR_LAUNCH.CHECKED_JUST_NOW);
    await userEvent.click(launchButton());
    expect(await within(box).findByTestId("ado-launch-note")).toHaveTextContent(ADO_PAT.LAUNCH_POLICY_REFUSED);
    expect(within(box).getAllByText(ADO_PAT.LAUNCH_POLICY_REFUSED)).toHaveLength(1);
    expect(within(box).getAllByRole("alert")).toHaveLength(1);
  });

  it("a launch refused for want of a usable sign-in asks to connect again, and connecting asks the server again", async () => {
    create.mockRejectedValueOnce(refusal(403, BREACH, "ado_pat_consent_needed"));
    open();
    const box = await dialog();
    await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "ado consent");
    await within(box).findByText(NR_LAUNCH.CHECKED_JUST_NOW);
    await userEvent.click(launchButton());
    const note = await within(box).findByTestId("ado-launch-note");
    expect(within(note).getByText(ADO_PAT.LAUNCH_NOT_CONNECTED)).toBeInTheDocument();
    const asked = preflight.mock.calls.length;
    await userEvent.click(within(note).getByRole("button", { name: ADO_PAT.MEMBER_CONNECT }));
    expect(adoConnect.calls).toBe(1);
    await waitFor(() => expect(preflight.mock.calls.length).toBe(asked + 1));
    // Connecting never launches for the person.
    expect(create).toHaveBeenCalledTimes(1);
  });

  it("a git_credential refusal opens the Azure DevOps door over the dialog", async () => {
    create.mockRejectedValueOnce(Object.assign(refusal(422, "Connect Azure DevOps.", "git_credential"), { org: "https://dev.azure.com/acme" }));
    open();
    const box = await dialog();
    await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "ado");
    await within(box).findByText(NR_LAUNCH.CHECKED_JUST_NOW);
    await userEvent.click(launchButton());
    expect(await screen.findByText(ADO.LAUNCH_DIALOG_TITLE)).toBeInTheDocument();
    expect(door.openDoor).not.toHaveBeenCalled();
  });

  // #2146: with own tokens for several organisations, an Azure DevOps credential refusal names the ones missing a token;
  // any other refusal says nothing about tokens.
  describe("the own-token note", () => {
    const own = (org: string, state: string) => ({ state, token_mode: "own_pat", org: `https://dev.azure.com/${org}` });
    const launchRefused = async (err: unknown) => {
      vi.spyOn(scmAccess, "getMine").mockResolvedValue([own("acme", "live"), own("beta", "not_configured")] as never);
      create.mockRejectedValueOnce(err);
      open();
      const box = await dialog();
      await userEvent.type(within(box).getByLabelText(NR_LAUNCH.TITLE_LABEL), "own tokens");
      await within(box).findByText(NR_LAUNCH.CHECKED_JUST_NOW);
      await userEvent.click(launchButton());
      await within(box).findByRole("alert");
      return box;
    };

    it("a git_credential refusal names the organisation missing a token", async () => {
      const box = await launchRefused(Object.assign(refusal(422, "Add your token.", "git_credential"), { cause: "token_expired", org: "https://dev.azure.com/beta" }));
      expect(await within(box).findByTestId("ado-own-token-note")).toHaveTextContent("Add your Azure DevOps token for beta first.");
    });

    it("a refusal that is not about Azure DevOps shows no token note", async () => {
      const box = await launchRefused(refusal(422, "this run needs 2 CPU, more than quota runs-quota has left.", "namespace_quota_exceeded"));
      expect(within(box).queryByTestId("ado-own-token-note")).not.toBeInTheDocument();
    });
  });
});
