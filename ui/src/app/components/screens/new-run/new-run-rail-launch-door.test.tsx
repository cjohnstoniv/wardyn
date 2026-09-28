/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Split from new-run-rail.test.tsx (#195): that file was over the 800-line
// test gate. The credential/recording-fact describes (Appendix A finding 1)
// stay there; the Autonomy section and the Azure DevOps connect dialog
// describes — which don't touch those facts — live here. The launch door is
// new-run-rail-provider-door.test.tsx's.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

// undefined = /healthz has not answered (or carried no recording component).
const recordingSelected = vi.hoisted(() => ({ value: "fs" as string | undefined }));
vi.mock("../../../lib/api/health", () => ({
  health: {
    health: () =>
      Promise.resolve({ components: { recording: { selected: recordingSelected.value } } }),
  },
}));

import { RunRail } from "./new-run-rail";
import { ADO } from "../../../lib/ado-entra-copy";
import { AUTONOMY_RAIL, autonomyBoundSentence } from "../../../lib/governance-copy";
import { AUTONOMY_META } from "../../wardyn/autonomy-meta";
import type { AutonomyResolution } from "../../../lib/api/governance";
import type {
  PreflightResult,
  SCMAccess,
  SetupHarnessTool,
} from "../../../lib/types";

function preflightWithAutonomy(autonomy?: AutonomyResolution): PreflightResult {
  return { setup_items: [], enforced_confinement_class: "CC1", autonomy };
}

type RailProps = Parameters<typeof railTree>[0];
function renderRail(props: RailProps) {
  const result = render(railTree(props));
  return { ...result, rerenderWith: (next: Partial<RailProps>) => result.rerender(railTree({ ...props, ...next })) };
}

function railTree(props: {
  agentRow?: SetupHarnessTool;
  preflightResult?: PreflightResult;
  showModelWarning?: boolean;
  governanceProfile?: string;
  showHoldNote?: boolean;
  onLaunch?: () => void;
  launchError?: string | null;
  /** #459: bumped on every failed launch — remounts the alert so a repeated,
   *  identical failure is re-announced. */
  launchErrorSeq?: number;
  preflightError?: string | null;
  preflightErrorSeq?: number;
  /** The server refused the launch for the caller's own model credential. */
  credentialRefused?: boolean;
  gitCredential?: SCMAccess;
  adoDialogOpen?: boolean;
  adoConnecting?: boolean;
  adoOrg?: string;
  adoBlockedUrl?: string | null;
  onAdoConfirm?: () => void;
  onAdoFallbackClick?: () => void;
  onAdoCancel?: () => void;
}) {
  return (
    <MemoryRouter>
      <RunRail
        cc="CC1"
        governanceProfile={props.governanceProfile}
        showModelWarning={props.showModelWarning ?? false}
        startup="It starts."
        showHoldNote={props.showHoldNote ?? false}
        toolRules={null}
        unattended={false}
        launch={{
          onLaunch: props.onLaunch ?? (() => {}),
          disabled: false,
          spinning: false,
          inFlight: false,
          problem: null,
          error: props.launchError ?? null,
          errorSeq: props.launchErrorSeq ?? 0,
          credentialRefused: props.credentialRefused ?? false,
        }}
        preflight={{
          error: props.preflightError ?? null,
          errorSeq: props.preflightErrorSeq ?? 0,
          // gitCredential rides the SAME preflight verdict as model_credential
          // does (RunRail derives both from preflight.result) — a synthetic
          // one when the test names only gitCredential, so the case reads as
          // "a preflight verdict carrying this fact" either way.
          result: props.gitCredential
            ? { setup_items: [], enforced_confinement_class: "CC1", ...props.preflightResult, git_credential: props.gitCredential }
            : (props.preflightResult ?? null),
        }}
        agentRow={props.agentRow}
        adoDialog={{
          open: props.adoDialogOpen ?? false,
          connecting: props.adoConnecting ?? false,
          org: props.adoOrg ?? "",
          blockedUrl: props.adoBlockedUrl ?? null,
          onConfirm: props.onAdoConfirm ?? (() => {}),
          onFallbackClick: props.onAdoFallbackClick ?? (() => {}),
          onCancel: props.onAdoCancel ?? (() => {}),
        }}
      />
    </MemoryRouter>
  );
}

// #93/#96 — the New Run rail's Autonomy section: what resolveRunAutonomy would
// cap this run at, once a preflight verdict is on screen.
describe("New run rail — the Autonomy section", () => {
  it("renders nothing before a preflight verdict is on screen", () => {
    renderRail({});
    expect(screen.queryByText(AUTONOMY_RAIL.HEADING)).toBeNull();
  });

  it("with no profile at all: the no-profile sentence and the no-limit chip", async () => {
    renderRail({ preflightResult: preflightWithAutonomy(undefined) });
    expect(await screen.findByText(AUTONOMY_RAIL.HEADING)).toBeInTheDocument();
    expect(screen.getByText(AUTONOMY_RAIL.NO_PROFILE)).toBeInTheDocument();
    expect(screen.queryByText(AUTONOMY_RAIL.NO_CAP)).toBeNull();
  });

  it("a profile with no rubric: the no-cap sentence, not the no-profile one", async () => {
    renderRail({ preflightResult: preflightWithAutonomy(undefined), governanceProfile: "Engineering" });
    expect(await screen.findByText(AUTONOMY_RAIL.HEADING)).toBeInTheDocument();
    expect(screen.getByText(AUTONOMY_RAIL.NO_CAP)).toBeInTheDocument();
    expect(screen.queryByText(AUTONOMY_RAIL.NO_PROFILE)).toBeNull();
  });

  it("a resolved level renders the level's friendly label and its one-cause sentence", async () => {
    renderRail({
      preflightResult: preflightWithAutonomy({
        level: "L1",
        posture: { egress: "sealed", secrets: "powerful", confinement: "CC1" },
        bound_by: ["secrets_powerful"],
      }),
    });
    expect(await screen.findByText(AUTONOMY_META.L1.label)).toBeInTheDocument();
    expect(screen.getByText(autonomyBoundSentence(["secrets_powerful"]))).toBeInTheDocument();
  });

  // Ruling 1 (#96 review): bound_by is a LIST, and a tie names EVERY cause —
  // the regression this pin exists to prevent is the rail reading bound_by[0]
  // alone and dropping the second (or third) tied row.
  it("a tie at the resolved level names EVERY bound_by cause, not just the first", async () => {
    const boundBy = ["secrets_powerful", "confinement_cc1"] as const;
    renderRail({
      preflightResult: preflightWithAutonomy({
        level: "L1",
        posture: { egress: "sealed", secrets: "powerful", confinement: "CC1" },
        bound_by: [...boundBy],
      }),
    });
    const sentence = autonomyBoundSentence([...boundBy]);
    expect(await screen.findByText(sentence)).toBeInTheDocument();
    expect(sentence).toContain("secrets");
    expect(sentence).toContain("barrier");
  });

  it("names the assigned governance profile beside a resolved level", async () => {
    renderRail({
      preflightResult: preflightWithAutonomy({
        level: "L2",
        posture: { egress: "open", secrets: "baseline", confinement: "CC1" },
        bound_by: ["egress_open"],
      }),
      governanceProfile: "Engineering",
    });
    expect(await screen.findByText(AUTONOMY_RAIL.PROFILE_LINE("Engineering"))).toBeInTheDocument();
  });

  it("a derived hold at L1 states the derived-hold note; a non-L1 level does not", async () => {
    const r = renderRail({
      preflightResult: preflightWithAutonomy({
        level: "L1",
        posture: { egress: "sealed", secrets: "powerful", confinement: "CC1" },
        bound_by: ["secrets_powerful"],
      }),
      showHoldNote: true,
    });
    expect(await screen.findByText(AUTONOMY_RAIL.DERIVED_HOLD_NOTE)).toBeInTheDocument();

    r.rerenderWith({
      preflightResult: preflightWithAutonomy({
        level: "L2",
        posture: { egress: "sealed", secrets: "powerful", confinement: "CC1" },
        bound_by: ["secrets_powerful"],
      }),
      showHoldNote: true,
    });
    expect(screen.queryByText(AUTONOMY_RAIL.DERIVED_HOLD_NOTE)).toBeNull();
  });
});

// #386's launch door — the Azure DevOps twin of the block above, but the
// rail here is presentational (adoDialog is screen-owned, see
// new-run-screen.test.tsx for the auto-open-on-422 + relaunch behaviour).
// These tests cover what the rail itself renders and wires.
describe("the Azure DevOps connect dialog and the git_credential preflight line", () => {
  it("renders nothing extra when there is no git_credential fact", () => {
    renderRail({});
    expect(screen.queryByText(ADO.PREFLIGHT_MISSING)).toBeNull();
    expect(screen.queryByRole("dialog", { name: ADO.LAUNCH_DIALOG_TITLE })).toBeNull();
  });

  // Review finding F4: on a deployment with no per-user Azure DevOps row (or
  // no Azure DevOps row at all), preflight never sends git_credential — a
  // shell run there must render no "Credentials" section, not an empty one.
  it("a shell run with no git_credential fact renders no Credentials heading at all", () => {
    // ticket: F4
    renderRail({});
    expect(screen.queryByText("Credentials")).toBeNull();
  });

  it("states PREFLIGHT_MISSING for a not_configured connection, before Launch is pressed", () => {
    renderRail({ gitCredential: { state: "not_configured" } });
    expect(screen.getByText(ADO.PREFLIGHT_MISSING)).toBeInTheDocument();
    expect(screen.getByText(ADO.PREFLIGHT_MISSING_SUB)).toBeInTheDocument();
  });

  it("says nothing for a live connection — no person name to compose PREFLIGHT_LIVE with", () => {
    renderRail({ gitCredential: { state: "live", source: "org" } });
    expect(screen.queryByText(ADO.PREFLIGHT_MISSING)).toBeNull();
  });

  // Review follow-up N5: a `live` gitCredential fact rendered nothing
  // (above), but showCredentials used to key on `!!gitCredential` — truthy
  // for `live` too — so a shell run with a live Azure DevOps connection and
  // no other credential to describe got an empty "Credentials" heading.
  it("a live shell run with no other credential renders no empty Credentials heading", () => {
    // ticket: N5
    renderRail({ gitCredential: { state: "live", source: "org" } });
    expect(screen.queryByText("Credentials")).toBeNull();
  });

  it("the dialog names the row's org (from the 422 body) and offers Continue to Microsoft / Cancel", () => {
    // ticket: F1
    // No preflight verdict at all — F1: the org comes from the 422 itself,
    // never from a git_credential fact that may not exist yet.
    renderRail({ adoDialogOpen: true, adoOrg: "https://dev.azure.com/contoso" });
    expect(screen.getByRole("heading", { name: ADO.LAUNCH_DIALOG_TITLE })).toBeInTheDocument();
    expect(screen.getByText(ADO.LAUNCH_DIALOG_BODY("https://dev.azure.com/contoso"))).toBeInTheDocument();
    expect(screen.getByRole("button", { name: ADO.CONNECT_CTA })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeInTheDocument();
  });

  it("a blocked popup shows the canon sentence and a plain fallback link to the sign-in URL", () => {
    // ticket: F9
    renderRail({ adoDialogOpen: true, adoBlockedUrl: "/api/v1/scm/azure-devops/signin" });
    expect(screen.getByText(ADO.CONNECT_POPUP_BLOCKED)).toBeInTheDocument();
    const link = screen.getByRole("link", { name: ADO.CONNECT_POPUP_OPEN });
    expect(link).toHaveAttribute("href", "/api/v1/scm/azure-devops/signin");
    expect(link).toHaveAttribute("target", "_blank");
  });

  // Review follow-up N1: clicking the fallback link ALSO starts the poll
  // (alongside its own href navigation), so the dialog advances on return.
  it("clicking the fallback link fires onFallbackClick", async () => {
    // ticket: N1
    const onAdoFallbackClick = vi.fn();
    renderRail({
      adoDialogOpen: true,
      adoBlockedUrl: "/api/v1/scm/azure-devops/signin",
      onAdoFallbackClick,
    });
    await userEvent.click(screen.getByRole("link", { name: ADO.CONNECT_POPUP_OPEN }));
    expect(onAdoFallbackClick).toHaveBeenCalledTimes(1);
  });

  it("Continue to Microsoft calls onAdoConfirm; Cancel calls onAdoCancel", async () => {
    const onAdoConfirm = vi.fn();
    const onAdoCancel = vi.fn();
    renderRail({ adoDialogOpen: true, onAdoConfirm, onAdoCancel });
    await userEvent.click(screen.getByRole("button", { name: ADO.CONNECT_CTA }));
    expect(onAdoConfirm).toHaveBeenCalledTimes(1);
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onAdoCancel).toHaveBeenCalled();
  });

  it("closing the dialog (Escape) calls onAdoCancel too", async () => {
    const onAdoCancel = vi.fn();
    renderRail({ adoDialogOpen: true, onAdoCancel });
    await userEvent.keyboard("{Escape}");
    expect(onAdoCancel).toHaveBeenCalled();
  });

  it("the confirm button shows a spinner and disables while connecting", () => {
    renderRail({ adoDialogOpen: true, adoConnecting: true });
    expect(screen.getByRole("button", { name: ADO.CONNECT_CTA })).toBeDisabled();
  });
});

// #459 — the launch and preflight errors become role="alert" regions,
// announced on arrival, with an sr-only prefix spoken before the server's own
// (unchanged, still-visible) sentence.
