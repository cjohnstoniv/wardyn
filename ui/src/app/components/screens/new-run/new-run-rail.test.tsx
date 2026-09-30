/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Appendix A finding 1: the rail's two unconditional security claims.
//
// The Credentials line did not look at where the run's model credential lands
// and the Recording line did not look at whether recording is enabled. Both are
// pinned here against the two facts the SERVER resolves, through the constants
// rather than through literals (a canon swap must not silently rewrite what
// these pin).
//
// Three cases are regression pins and say so: the sentence is read only off a
// preflight verdict (F1), a run with no model credential gets no sentence at
// all (F4), and an unread /healthz makes no promise either way (F3).
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

function harnessRow(): SetupHarnessTool {
  return { id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true };
}

function preflightWith(cred: ModelCredential): PreflightResult {
  return { setup_items: [], enforced_confinement_class: "CC1", model_credential: cred };
}

function renderRail(props: { agentRow?: SetupHarnessTool; preflightResult?: PreflightResult; showModelWarning?: boolean }) {
  return render(
    <MemoryRouter>
      <RunRail
        cc="CC1"
        showModelWarning={props.showModelWarning ?? false}
        startup="It starts."
        showHoldNote={false}
        toolRules={null}
        unattended={false}
        launch={{
          onLaunch: () => {},
          disabled: false,
          spinning: false,
          inFlight: false,
          problem: null,
          error: null,
          errorSeq: 0,
          credentialRefused: false,
        }}
        preflight={{ error: null, errorSeq: 0, result: props.preflightResult ?? null }}
        agentRow={props.agentRow}
        adoDialog={{
          open: false,
          connecting: false,
          org: "",
          blockedUrl: null,
          onConfirm: () => {},
          onFallbackClick: () => {},
          onCancel: () => {},
        }}
      />
    </MemoryRouter>,
  );
}

// The sentences, each keyed on the residency PREFLIGHT resolved — the only
// fact the rail may read.
const credentialArms: { name: string; cred: ModelCredential; want: string }[] = [
  { name: "proxy", cred: { residency: "proxy", provider: "anthropic", kind: "anthropic_api_key" }, want: RAIL_CREDENTIAL.PROXY },
  {
    name: "sandbox, Bedrock",
    cred: { residency: "sandbox", provider: "bedrock-sso", kind: "bedrock_sso" },
    want: RAIL_CREDENTIAL.SANDBOX_BEDROCK,
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

  // Whose AWS sign-in sits in the sandbox: the person's own, stated with the
  // sentence; a proxy lane carries no chip.
  it("a sandbox verdict carries the per-person chip; a proxy verdict does not", async () => {
    const { unmount } = renderRail({ agentRow: harnessRow(), preflightResult: preflightWith(credentialArms[1].cred) });
    expect(await screen.findByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_PER_USER)).toBeInTheDocument();
    unmount();
    renderRail({ agentRow: harnessRow(), preflightResult: preflightWith(credentialArms[0].cred) });
    expect(await screen.findByText(RAIL_CREDENTIAL.PROXY)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_PER_USER)).toBeNull();
  });

  // F1 regression pin. The agent row carries no residency: only a preflight
  // verdict of this exact body settles where the credential lands, so a row
  // alone says nothing beyond "not resolved yet".
  it("an agent row with no preflight verdict says only that it is not resolved yet", async () => {
    renderRail({ agentRow: harnessRow() });
    expect(await screen.findByText(RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH)).toBeInTheDocument();
    expect(screen.getByText(RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK)).toBeNull();
    expect(screen.queryByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_PER_USER)).toBeNull();
  });

  // The negative control: nothing resolved — the state every user is in
  // before pressing anything — and the proxy sentence must never render.
  it("with neither source the proxy sentence never renders", async () => {
    renderRail({ agentRow: harnessRow() });
    expect(await screen.findByText(RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_CREDENTIAL.PROXY)).toBeNull();
  });

  // F4 regression pin. A shell command gets no model credential, so the screen
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
      preflightResult: preflightWith({ residency: "proxy", provider: "anthropic", kind: "anthropic_api_key" }),
    });
    expect(await screen.findByText(RAIL_CREDENTIAL.PROXY)).toBeInTheDocument();
    expect(screen.getByText(/No model provider is connected/)).toBeInTheDocument();
  });

  // F3 regression pin. An unread /healthz is not evidence that recording is on,
  // and this rail is where the promise about it gets made.
  it("recording says nothing until /healthz has actually answered", async () => {
    recordingSelected.value = undefined;
    renderRail({ agentRow: harnessRow(), preflightResult: preflightWith(credentialArms[1].cred) });
    expect(await screen.findByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK)).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText(RECORDING_ON)).toBeNull());
    expect(screen.queryByText(RECORDING_DISABLED_TITLE)).toBeNull();
    // U-15: and the HEADING goes with it — a bare "Recording" over nothing reads
    // as a section that failed to load, on a rail read as a list of what the run
    // can do. Credentials already withholds its own heading the same way.
    expect(screen.queryByText("Recording")).toBeNull();
  });
});
