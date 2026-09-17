/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Appendix A finding 1: the rail's two unconditional security claims.
//
// The Credentials line did not look at which mechanism the run's agent uses and
// the Recording line did not look at whether recording is enabled. Both are
// pinned here against the two facts the SERVER resolves, through the constants
// rather than through literals (a canon swap must not silently rewrite what
// these pin).
//
// Three cases are regression pins for the fix pass's review findings and say so:
// the sentence must never be chosen from the roster's DECLARED mechanism (F1), a
// run with no model credential gets no sentence at all (F4), and an unread
// /healthz makes no promise either way (F3).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
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
import { RAIL_CREDENTIAL, RAIL_RECORDING_ON, RECORDING_DISABLED_TITLE } from "../../wardyn/copy";
import type { ModelCredential, PreflightResult, SetupHarnessTool } from "../../../lib/types";

// U-15: through the constant, never a fourth typed copy of the sentence.
const RECORDING_ON = RAIL_RECORDING_ON;

// The server publishes credential_residency for ONE row shape, so these fixtures
// carry the declared mechanism the real wire carries — precisely so that a
// sentence picked off it would show up here. That it did not is how F1 shipped.
function harnessRow(residency?: "sandbox"): SetupHarnessTool {
  return {
    id: "claude-code",
    display: "Claude Code",
    has_gateway: true,
    has_login: true,
    mechanism: "bedrock_sso",
    credential_source: "per_user",
    credential_residency: residency,
  };
}

function preflightWith(cred: ModelCredential): PreflightResult {
  return { setup_items: [], enforced_confinement_class: "CC1", model_credential: cred };
}

function renderRail(props: {
  agentRow?: SetupHarnessTool;
  preflightResult?: PreflightResult;
  showModelWarning?: boolean;
}) {
  return render(
    <MemoryRouter>
      <RunRail
        cc="CC1"
        showModelWarning={props.showModelWarning ?? false}
        startup="It starts."
        showHoldNote={false}
        toolRules={null}
        launch={{
          onLaunch: () => {},
          disabled: false,
          spinning: false,
          inFlight: false,
          problem: null,
          error: null,
          warnings: [],
          onOpenRun: null,
        }}
        preflight={{ error: null, result: props.preflightResult ?? null }}
        agentRow={props.agentRow}
      />
    </MemoryRouter>,
  );
}

// THE SIX SENTENCES, each keyed on what PREFLIGHT resolved. `mechanism` here is
// the RESOLVED lane, which is the only mechanism the rail may ever read.
const credentialArms: { name: string; cred: ModelCredential; want: string }[] = [
  {
    name: "proxy",
    cred: { residency: "proxy", mechanism: "anthropic_api_key" },
    want: RAIL_CREDENTIAL.PROXY,
  },
  {
    name: "proxy with a staged placeholder",
    cred: { residency: "proxy", mechanism: "anthropic_subscription", staged_placeholder: true },
    want: RAIL_CREDENTIAL.PROXY_STAGED,
  },
  {
    name: "sandbox, Bedrock",
    cred: { residency: "sandbox", mechanism: "bedrock_sso", credential_source: "per_user" },
    want: RAIL_CREDENTIAL.SANDBOX_BEDROCK,
  },
  {
    name: "sandbox, subscription",
    cred: { residency: "sandbox", mechanism: "anthropic_subscription" },
    want: RAIL_CREDENTIAL.SANDBOX_SUBSCRIPTION,
  },
  { name: "image", cred: { residency: "image", mechanism: "none" }, want: RAIL_CREDENTIAL.IMAGE },
  { name: "unknown", cred: { residency: "unknown" }, want: RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH },
];

describe("New run rail — the two facts it used to assert (Appendix A finding 1)", () => {
  beforeEach(() => {
    recordingSelected.value = "fs";
  });

  for (const arm of credentialArms) {
    for (const recording of ["on", "disabled"] as const) {
      it(`${arm.name} credential, recording ${recording}`, async () => {
        recordingSelected.value = recording === "disabled" ? "none" : "fs";
        renderRail({ agentRow: harnessRow(), preflightResult: preflightWith(arm.cred) });

        expect(await screen.findByText(arm.want)).toBeInTheDocument();
        // Every OTHER sentence is absent: one residency, one claim.
        for (const other of credentialArms) {
          if (other.want !== arm.want) expect(screen.queryByText(other.want)).toBeNull();
        }
        await waitFor(() => {
          if (recording === "disabled") {
            expect(screen.getByText(RECORDING_DISABLED_TITLE)).toBeInTheDocument();
            expect(screen.queryByText(RECORDING_ON)).toBeNull();
          } else {
            expect(screen.getByText(RECORDING_ON)).toBeInTheDocument();
            expect(screen.queryByText(RECORDING_DISABLED_TITLE)).toBeNull();
          }
        });
      });
    }
  }

  // F1 REGRESSION PIN. The status row settles residency only for the per-user
  // Bedrock SSO shape; everywhere else it is absent while `mechanism` is still
  // populated. The rail used to pick its sentence off that DECLARED field, so a
  // compose deployment with a ~/.claude mount read "AWS credentials sign inside
  // the sandbox" over a Claude sign-in. A row that settles nothing says nothing.
  it("a row with no graded residency says only that it is not resolved yet", async () => {
    renderRail({ agentRow: harnessRow() });
    expect(await screen.findByText(RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH)).toBeInTheDocument();
    expect(screen.getByText(RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_PER_USER)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_SHARED)).toBeNull();
  });

  // F1's other half: the subscription arm must win off the RESOLVED mechanism,
  // even though `sandbox` is overwhelmingly a Bedrock answer.
  it("a resolved subscription lane never renders the Bedrock sentence", async () => {
    renderRail({
      agentRow: harnessRow(),
      preflightResult: preflightWith({ residency: "sandbox", mechanism: "anthropic_subscription" }),
    });
    expect(await screen.findByText(RAIL_CREDENTIAL.SANDBOX_SUBSCRIPTION)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_PER_USER)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_SHARED)).toBeNull();
  });

  // The ROW-FIXED case — the only thing /setup/status publishes, and the field
  // report's own estate: a per-user AWS sign-in, stated with NO click.
  it("the row-fixed per_user Bedrock SSO row states residency with no Preflight", async () => {
    renderRail({ agentRow: harnessRow("sandbox") });
    expect(await screen.findByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK)).toBeInTheDocument();
    expect(screen.getByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_PER_USER)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT)).toBeNull();
  });

  // Preflight graded the body about to be launched; the row graded a shape. The
  // specific one wins, chip included.
  it("a current preflight verdict overrides the row-fixed row", async () => {
    renderRail({
      agentRow: harnessRow("sandbox"),
      preflightResult: preflightWith({ residency: "proxy", mechanism: "bedrock_bearer" }),
    });
    expect(await screen.findByText(RAIL_CREDENTIAL.PROXY)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_PER_USER)).toBeNull();
  });

  // THE NEGATIVE CONTROL. Nothing resolved — the state every user is in before
  // pressing anything, and the state the old copy answered with "never written
  // into the sandbox".
  it("with neither source the proxy sentence never renders", async () => {
    renderRail({ agentRow: harnessRow() });
    expect(await screen.findByText(RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.PROXY)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.PROXY_STAGED)).toBeNull();
  });

  // F4 REGRESSION PIN. A shell command gets no model credential, so the screen
  // withholds the agent row and there is nothing to say — not even "not resolved
  // yet", which would imply one is coming.
  it("a run with no model credential renders no Credentials section at all", async () => {
    renderRail({});
    await waitFor(() => expect(screen.getByText(RECORDING_ON)).toBeInTheDocument());
    for (const arm of credentialArms) expect(screen.queryByText(arm.want)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT)).toBeNull();
    expect(screen.queryByText("Credentials")).toBeNull();
  });

  // U-4 (W6 blind lens). "Run Preflight to see where this run's model credential
  // will live." is a promise that is false the moment it is followed: a CURRENT
  // verdict that carries no `model_credential` (always so against a 0.7.4 daemon,
  // and on 0.7.5 whenever the roster read failed) leaves the rail telling the
  // reader to press the button they just pressed. The hint renders only while
  // there is no verdict at all.
  it("a current preflight verdict with no model_credential drops the Preflight hint", async () => {
    renderRail({
      agentRow: harnessRow(),
      preflightResult: { setup_items: [], enforced_confinement_class: "CC1" },
    });
    expect(await screen.findByText(RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT)).toBeNull();
  });

  // U-5 (W6 blind lens). A stock install before any key is added: llm_ready false
  // AND a roster row. One section said "No model provider is connected… its first
  // model call fails." AND "Resolved at launch." AND "Run Preflight…" — nothing
  // resolves at launch when nothing is connected.
  it("the no-provider warning replaces the credential facts rather than sitting beside them", async () => {
    renderRail({ agentRow: harnessRow(), showModelWarning: true });
    expect(await screen.findByText(/No model provider is connected/)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT)).toBeNull();
  });

  // U-5's bound: a RESOLVED credential still states its residency beside the
  // warning — that sentence is read off the verdict, not guessed.
  it("…but a resolved credential is still stated beside the warning", async () => {
    renderRail({
      agentRow: harnessRow(),
      showModelWarning: true,
      preflightResult: preflightWith({ residency: "proxy", mechanism: "anthropic_api_key" }),
    });
    expect(await screen.findByText(RAIL_CREDENTIAL.PROXY)).toBeInTheDocument();
    expect(screen.getByText(/No model provider is connected/)).toBeInTheDocument();
  });

  // F3 REGRESSION PIN. An unread /healthz is not evidence that recording is on,
  // and this rail is where the promise about it gets made.
  it("recording says nothing until /healthz has actually answered", async () => {
    recordingSelected.value = undefined;
    renderRail({ agentRow: harnessRow("sandbox") });
    expect(await screen.findByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK)).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText(RECORDING_ON)).toBeNull());
    expect(screen.queryByText(RECORDING_DISABLED_TITLE)).toBeNull();
    // U-15: and the HEADING goes with it — a bare "Recording" over nothing reads
    // as a section that failed to load, on a rail read as a list of what the run
    // can do. Credentials already withholds its own heading the same way.
    expect(screen.queryByText("Recording")).toBeNull();
  });
});
