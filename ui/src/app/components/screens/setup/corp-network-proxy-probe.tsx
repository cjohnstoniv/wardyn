/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Loader2 } from "lucide-react";
import type { ProxyTestResult } from "../../../lib/api/health";
import { T } from "../../../lib/integrations";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { cn } from "../../ui/utils";
import { Field } from "../../wardyn/form-primitives";
import { Chip } from "../../wardyn/primitives";
import type { ProbeUiState } from "./corp-network-egress";

// A verdict's own headline and tone sit beside the verdict itself, closing the
// gap where a non-reached verdict would otherwise render as a bare chip inside
// the proxy panel while its headline and advice hung off the step footer
// (setup-layout's gate row): a runner problem — a probe sandbox that never
// started, or one that started and never reported back — must not read under
// the proxy's heading with the proxy's instruction. An operator sent to
// reconfigure a proxy that was never tested is being sent to fix the wrong
// thing.
//
// Zero new copy: every string below already exists in lib/integrations.ts, and
// the mapping is the same ladder corpNetworkGate walks (steps.ts) — the two are
// deliberately identical so the panel and the footer can never name two
// different owners for one verdict. Only a real `blocked` (intercepted
// included) is warning-toned, because only it names something fixable on this
// step; the two runner verdicts unlock the gate and stay neutral.
export function proxyVerdictBrief(
  result: ProxyTestResult,
): { head: string; note: string; tone: "warning" | "neutral" } | null {
  switch (result.state) {
    case "reached":
      // Nothing broke. A custom pass is weaker, and CUSTOM_CAVEAT already says
      // so inline — a headline over a pass would read as a fault.
      return null;
    case "no_runner":
      return { head: T.GATE_HEAD_NORUNNER, note: T.TEST_NORUNNER, tone: "neutral" };
    case "not_run":
      return { head: T.GATE_HEAD_NOT_RUN, note: T.NOT_RUN_NOTE, tone: "neutral" };
    case "timed_out":
      // The wire detail rendered above this note carries the sandbox's state at
      // the deadline AND the budget it blew — including the driver's own "agent
      // exec never started" where it proved that (probeFailureDetail folds it
      // into the detail string; there is no separate wire flag to read). The
      // panel quotes that verbatim, which is the honest naming of the cause;
      // this note only says whose problem it is.
      return { head: T.GATE_HEAD_TIMED_OUT, note: T.TIMED_OUT_NOTE, tone: "neutral" };
    case "blocked":
      return result.intercepted
        ? { head: T.GATE_HEAD_INTERCEPTED, note: T.GATE_INTERCEPTED, tone: "warning" }
        : { head: T.GATE_HEAD_BLOCKED, note: T.GATE_BLOCKED, tone: "warning" };
    default:
      // `bypass`, and any state a newer server grows. corpNetworkGate treats
      // every other non-reached state as "still untested" — same answer here,
      // rather than inventing a headline this build cannot stand behind.
      return { head: T.GATE_HEAD_UNTESTED, note: T.GATE_UNTESTED, tone: "warning" };
  }
}

// The verdict, per the mock's proxyTestBlock kinds: a builtin reached says
// which path it proved; a custom pass DELIBERATELY avoids the success
// treatment ("Request completed", info tone — it must never read as a
// verified one); intercepted is a blocked flavor rendered apart, because it
// sends the operator to a different person than a refused connection does.
function ProxyVerdict({ result }: { result: ProxyTestResult }) {
  if (result.state === "reached") {
    if (result.custom) {
      return (
        <span className="flex flex-wrap items-center gap-2">
          <Chip tone="info">Request completed</Chip>
          <span className="text-meta text-muted-foreground">custom endpoint — not verified against a known payload</span>
        </span>
      );
    }
    return (
      <Chip tone="success" dot>
        {result.via === "direct" ? "Reached · direct" : "Reached · via proxy"}
      </Chip>
    );
  }
  if (result.state === "no_runner") return <Chip tone="neutral">Can&apos;t test here</Chip>;
  // Without this arm, not_run falls through to the plain warning chip below
  // and renders "Blocked" — exactly the misleading claim this state exists
  // to avoid (the probe never ran; nothing about the network was observed).
  if (result.state === "not_run") return <Chip tone="neutral">Never ran</Chip>;
  // Without this arm, timed_out falls through to the plain warning chip below
  // and renders "Blocked" — the sandbox started and ran, it just never
  // reported completion; that's a runner/recording-upload fact, not a
  // network verdict.
  if (result.state === "timed_out") return <Chip tone="neutral">Probe never reported back</Chip>;
  if (result.intercepted) {
    return (
      <span className="flex flex-wrap items-center gap-2">
        <Chip tone="warning" dot>Blocked · intercepted</Chip>
        <span className="text-meta text-muted-foreground">answered 200 OK — with someone else&apos;s page</span>
      </span>
    );
  }
  return (
    <Chip tone="warning" dot>
      {result.state === "bypass" ? "Redirect not enforced" : "Blocked"}
    </Chip>
  );
}

export function ProxyTestBlock({
  state,
  onTest,
  onTestCustom,
  operator,
  probeLine,
  customReject,
  customDraft,
  onCustomDraftChange,
  hideButton,
}: {
  state: ProbeUiState;
  /** The default multi-target check ("Test connectivity" / "Test again"). */
  onTest: () => void;
  /** The custom-endpoint probe — fired by Enter in the custom field here; the
   *  gate row's relabeled button is the primary launch point (steps.ts). */
  onTestCustom: () => void;
  operator: boolean;
  /** What the builtin probe is about to do, endpoints and chain named — shown while running. */
  probeLine: string;
  /** A rejected custom URL's server message, rendered inline in the custom block (never a toast — T.CUSTOM_REJECT_WHY). */
  customReject: string | null;
  /** Lifted to the orchestrator (CorpNetworkState.customDraft): the gate's
   *  action label derives from it ("Test this URL" once non-empty). */
  customDraft: string;
  onCustomDraftChange: (v: string) => void;
  /** One Test button per screen (the mock's stepTest): while the gate row
   *  below carries the action, the panel's own button is suppressed and this
   *  block shows only the result/hint. Once the gate has moved on to Next,
   *  the button returns here — as "Test again" — so re-testing stays
   *  reachable. */
  hideButton: boolean;
}) {
  const running = state.kind === "running";
  const hasResult = state.kind === "done";
  const customPass = state.kind === "done" && state.result.state === "reached" && state.result.custom;
  // The verdict's own headline, note and tone. Null on a pass.
  const brief = state.kind === "done" ? proxyVerdictBrief(state.result) : null;
  return (
    <div
      className={cn(
        // overflow-wrap inherits: probe results name real endpoints
        // (www.msftconnecttest.com/connecttest.txt) — unbreakable tokens whose
        // min-content width exceeds any narrow container.
        "flex items-start gap-3 rounded-lg border p-3 [overflow-wrap:anywhere]",
        // The mock's okcustom container: a dashed info frame, so even the box
        // around a custom pass reads differently from a verified one.
        customPass
          ? "border-dashed border-info/40"
          : // The frame carries the verdict's tone, so a blocked probe
            // looks different from a probe that never ran. Colour never says it
            // alone — the headline and the toned Chip inside say it in words.
            brief?.tone === "warning"
            ? "border-warning/30"
            : "border-border",
      )}
    >
      {!hideButton && (
        <div className="shrink-0 space-y-1">
          <Button size="sm" variant="outline" disabled={running || !operator} onClick={onTest}>
            {running ? <Loader2 className="size-3.5 animate-spin" /> : null}
            {running ? "Testing…" : hasResult ? "Test again" : "Test connectivity"}
          </Button>
          {/* #459: standing alone (not beside another control in a row) —
              helper text under it, not a title tooltip. */}
          {!operator && <p className="text-meta text-muted-foreground">{T.VIEWER_HINT}</p>}
        </div>
      )}
      <div className="min-w-0 flex-1 space-y-1">
        {state.kind === "idle" && (
          <>
            <span className="text-meta text-muted-foreground">Not tested</span>
            <p className="text-meta leading-snug text-muted-foreground">{T.TEST_PROXY_HINT}</p>
          </>
        )}
        {state.kind === "running" && (
          <>
            <p className="text-xs text-info">Starting a throwaway sandbox — {state.elapsedSec}s</p>
            {!state.custom && <p className="text-meta leading-snug text-muted-foreground">{probeLine}</p>}
          </>
        )}
        {state.kind === "done" && (
          // F3-F9: the probe verdict is announced to a screen reader the moment
          // it lands — the ticker above (elapsedSec, "running" branch) stays
          // outside this region on purpose, or a live announcement would fire
          // every second while the sandbox is out.
          <div role="status" aria-live="polite">
            {/* Headline first, then the chip, then the wire detail, then
                this verdict's own note. The order is the argument — the
                operator reads what happened before they read who owns it. */}
            {brief && <p className="text-body font-medium text-foreground">{brief.head}</p>}
            <ProxyVerdict result={state.result} />
            {/* no_runner is the one state with no wire detail worth quoting:
                nothing launched, and its note says what to DO (configure a
                barrier), which the server's own line doesn't. */}
            {state.result.state !== "no_runner" && (
              <p className="text-xs leading-snug text-foreground">{state.result.detail}</p>
            )}
            {brief && <p className="text-meta leading-snug text-muted-foreground">{brief.note}</p>}
            {/* Everything past here is state-specific colour on top of the
                head/detail/note spine above — never a second verdict. */}
            {state.result.state !== "no_runner" &&
              state.result.state !== "not_run" &&
              state.result.state !== "timed_out" && (
              <>
                {state.result.custom && state.result.state === "reached" && (
                  <p className="text-xs leading-snug text-info">{T.CUSTOM_CAVEAT}</p>
                )}
                {/* reached-only: the probe itself passed, but this run's
                    recording never reached the control plane — same caution
                    styling as HostProxyCheckNote's warn state / CRED_URL_NOTE
                    above, reused rather than a new component. Never changes
                    the gate: reached still unlocks Next either way. */}
                {state.result.state === "reached" && state.result.warning && (
                  <div className="rounded-lg border border-warning/30 bg-warning-subtle p-2">
                    <p className="text-meta leading-snug text-warning">{state.result.warning}</p>
                  </div>
                )}
                {state.result.intercepted && (
                  <>
                    <div className="rounded-md border border-border bg-muted/40 px-2.5 py-2">
                      <p className="text-xs leading-snug text-foreground">{T.INTERCEPT_MEANS}</p>
                    </div>
                    <p className="max-w-[620px] text-meta leading-snug text-muted-foreground">{T.PROBE_ENDPOINTS}</p>
                  </>
                )}
                <p className="text-meta leading-snug text-muted-foreground">{T.TEST_STANDING}</p>
              </>
            )}
            {/* Revealed only after a failure — never on arrival, or everyone
                reaches for it instead of fixing the proxy and the gate goes
                decorative. A recovery affordance, not configuration. Covers
                intercepted too (it is a blocked flavor). No button of its own
                while the gate row owns the action: the gate relabels itself
                to "Test this URL" the moment this field is non-empty; Enter
                here fires the same probe. */}
            {state.result.state === "blocked" && (
              <div className="mt-1.5 space-y-2.5 rounded-lg border border-dashed border-border-strong p-3">
                <div className="space-y-1">
                  <p className="text-body font-medium text-foreground">No public endpoint will answer here?</p>
                  <p className="max-w-[560px] text-meta leading-snug text-muted-foreground">{T.CUSTOM_URL_WHY}</p>
                </div>
                <div className="flex items-end gap-2">
                  <Field label="Test against a URL of your own" htmlFor="corp-custom-url" hint={T.CUSTOM_URL_HINT} className="min-w-0 flex-1">
                    <Input
                      id="corp-custom-url"
                      value={customDraft}
                      onChange={(e) => onCustomDraftChange(e.target.value)}
                      onKeyDown={(e) => {
                        if (e.key === "Enter" && customDraft.trim() && operator) onTestCustom();
                      }}
                      placeholder="https://nexus.corp.internal/repository/health"
                      className="font-mono"
                    />
                  </Field>
                  {!hideButton && (
                    <Button
                      size="sm"
                      variant="outline"
                      className="shrink-0"
                      disabled={!operator || !customDraft.trim()}
                      onClick={onTestCustom}
                    >
                      Test this URL
                    </Button>
                  )}
                </div>
                {customReject && (
                  <div className="space-y-1.5">
                    <div className="rounded-md border border-danger/30 bg-danger-subtle px-2.5 py-2">
                      <p className="text-xs leading-snug text-danger">{customReject}</p>
                    </div>
                    <p className="text-meta leading-snug text-muted-foreground">{T.CUSTOM_REJECT_WHY}</p>
                  </div>
                )}
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  );
}
