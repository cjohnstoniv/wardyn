/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Appendix A finding 1: the rail's two unconditional security claims.
//
// The Credentials line did not look at which mechanism the run's agent uses and
// the Recording line did not look at whether recording is enabled — so both are
// asserted here against the two facts the SERVER resolves, in the 6 x 2 matrix
// the fix has to cover, through the constants rather than through literals (a
// canon swap must not silently rewrite what these pin).
//
// The negative control is the whole point of the lane: with NEITHER source the
// proxy sentence must not render. That sentence was the old default, and it is a
// false assurance on every resident lane.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

const recordingSelected = vi.hoisted(() => ({ value: "fs" }));
vi.mock("../../../lib/api/health", () => ({
  health: {
    health: () =>
      Promise.resolve({ components: { recording: { selected: recordingSelected.value } } }),
  },
}));

import { RunRail } from "./new-run-rail";
import { RAIL_CREDENTIAL, RECORDING_DISABLED_TITLE } from "../../wardyn/copy";
import type {
  ModelCredential,
  PreflightResult,
  RunPolicySpec,
  SetupHarnessTool,
} from "../../../lib/types";

const RECORDING_ON = "Every keystroke and every outbound connection.";

function harnessRow(cred: Partial<SetupHarnessTool>): SetupHarnessTool {
  return { id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, ...cred };
}

function renderRail(props: {
  agentRow?: SetupHarnessTool;
  preflightResult?: PreflightResult | null;
  savedPolicy?: { name: string; spec: RunPolicySpec };
}) {
  return render(
    <MemoryRouter>
      <RunRail
        cc="CC1"
        showModelWarning={false}
        startup="It starts."
        showHoldNote={false}
        toolRules={null}
        savedPolicy={props.savedPolicy}
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

// The rail reads only eligible_grants off the saved policy; the rest is the
// minimum a RunPolicySpec must carry for the Policy section above it to render.
function policyGranting(kind: string): { name: string; spec: RunPolicySpec } {
  return {
    name: "e2e policy",
    spec: {
      allowed_domains: [],
      first_use_approval: "deny_with_review",
      min_confinement_class: "CC1",
      eligible_grants: [{ kind, requires_approval: false }],
    },
  };
}

// THE SIX SENTENCES, each keyed on what the server graded. `mechanism` is the
// RESOLVED lane, never the roster's declared one.
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
  {
    name: "image",
    cred: { residency: "image", mechanism: "none" },
    want: RAIL_CREDENTIAL.IMAGE,
  },
  {
    name: "unknown",
    cred: { residency: "unknown" },
    want: RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH,
  },
];

describe("New run rail — the two facts it used to assert (Appendix A finding 1)", () => {
  beforeEach(() => {
    recordingSelected.value = "fs";
  });

  for (const arm of credentialArms) {
    for (const recording of ["on", "disabled"] as const) {
      it(`${arm.name} credential, recording ${recording}`, async () => {
        recordingSelected.value = recording === "disabled" ? "none" : "fs";
        renderRail({
          agentRow: harnessRow({
            credential_residency: arm.cred.residency,
            mechanism: arm.cred.mechanism,
            credential_source: arm.cred.credential_source,
            staged_placeholder: arm.cred.staged_placeholder,
          }),
        });

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

  // WHOSE credential the sandbox holds — the per_user/shared split, which is the
  // difference between "my own AWS session is in there" and "the admin's is".
  it("the sandbox-Bedrock arm names whose sign-in it is", async () => {
    renderRail({
      agentRow: harnessRow({
        credential_residency: "sandbox",
        mechanism: "bedrock_sso",
        credential_source: "per_user",
      }),
    });
    expect(
      await screen.findByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_PER_USER),
    ).toBeInTheDocument();

    cleanup();
    renderRail({
      agentRow: harnessRow({
        credential_residency: "sandbox",
        mechanism: "bedrock_aws_dir",
        credential_source: "shared",
      }),
    });
    expect(
      await screen.findByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_SHARED),
    ).toBeInTheDocument();
  });

  // The subscription arm is resident too, but there is no AWS sign-in to name.
  it("the sandbox-subscription arm carries no AWS chip", async () => {
    renderRail({
      agentRow: harnessRow({ credential_residency: "sandbox", mechanism: "anthropic_subscription" }),
    });
    expect(await screen.findByText(RAIL_CREDENTIAL.SANDBOX_SUBSCRIPTION)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_SHARED)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_PER_USER)).toBeNull();
  });

  // Preflight graded the body about to be launched; the status row graded the
  // deployment default. When both exist the specific one wins.
  it("a current preflight verdict overrides the status row", async () => {
    renderRail({
      agentRow: harnessRow({ credential_residency: "proxy", mechanism: "anthropic_api_key" }),
      preflightResult: {
        setup_items: [],
        enforced_confinement_class: "CC1",
        model_credential: { residency: "sandbox", mechanism: "bedrock_sso", credential_source: "shared" },
      },
    });
    expect(await screen.findByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.PROXY)).toBeNull();
  });

  // THE NEGATIVE CONTROL. No status row, no preflight — the state every user is
  // in before anything resolves, and the state the old copy answered with "never
  // written into the sandbox".
  it("with neither source the proxy sentence never renders", async () => {
    renderRail({});
    expect(await screen.findByText(RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.PROXY)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.PROXY_STAGED)).toBeNull();
  });

  // The scoped heading's exception: a policy carrying env_secret/ssh_key grants
  // puts a live credential in the sandbox whatever the model credential does.
  it("a policy granting resident secrets says so", async () => {
    renderRail({
      agentRow: harnessRow({ credential_residency: "proxy", mechanism: "anthropic_api_key" }),
      savedPolicy: policyGranting("env_secret"),
    });
    expect(await screen.findByText(RAIL_CREDENTIAL.POLICY_GRANTS_SECRETS)).toBeInTheDocument();
  });

  it("a policy granting only brokered credentials does not", async () => {
    renderRail({
      agentRow: harnessRow({ credential_residency: "proxy", mechanism: "anthropic_api_key" }),
      savedPolicy: policyGranting("github_token"),
    });
    expect(await screen.findByText(RAIL_CREDENTIAL.PROXY)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.POLICY_GRANTS_SECRETS)).toBeNull();
  });
});
