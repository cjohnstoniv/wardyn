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
import { RAIL_CREDENTIAL, RECORDING_DISABLED_TITLE } from "../../wardyn/copy";
import type { ModelCredential, PreflightResult, SetupHarnessTool } from "../../../lib/types";

const RECORDING_ON = "Every keystroke and every outbound connection.";

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

function renderRail(props: { agentRow?: SetupHarnessTool; preflightResult?: PreflightResult }) {
  return render(
    <MemoryRouter>
      <RunRail
        cc="CC1"
        showModelWarning={false}
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

  // F3 REGRESSION PIN. An unread /healthz is not evidence that recording is on,
  // and this rail is where the promise about it gets made.
  it("recording says nothing until /healthz has actually answered", async () => {
    recordingSelected.value = undefined;
    renderRail({ agentRow: harnessRow("sandbox") });
    expect(await screen.findByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK)).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText(RECORDING_ON)).toBeNull());
    expect(screen.queryByText(RECORDING_DISABLED_TITLE)).toBeNull();
  });
});
