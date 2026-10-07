/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import {
  type ApprovalRequest,
  type AuditEvent,
  type CredentialGrant,
  type EgressDecision,
  type Recording,
  type RunDetail,
} from "../../../lib/types";
import { isTerminalRunState } from "../../../lib/types";
import { runs as runsApi } from "../../../lib/api/runs";
import { approvals as approvalsApi } from "../../../lib/api/approvals";
import { audit as auditApi, egressFromAudit, runEndingFromAudit } from "../../../lib/api/audit";
import { heldCredentials } from "../../../lib/held-credentials";
import { recordings as recordingsApi } from "../../../lib/api/recordings";
import { useRecordingDisabled } from "../../../lib/hooks/use-recording-disabled";
import { usePoll } from "../../../lib/use-poll";

// Live refresh cadence for a non-terminal run's detail.
const DETAIL_POLL_MS = 4000;
// How long after a KILLED run ended its page keeps asking whether the kill row has landed.
const KILL_SETTLE_MS = 120_000;

export type Tab = "overview" | "approvals" | "policy" | "audit" | "recording" | "output";

export function useRunDetail(id: string) {
  const [run, setRun] = React.useState<RunDetail | null | undefined>(undefined);
  const [grants, setGrants] = React.useState<CredentialGrant[]>([]);
  const [egress, setEgress] = React.useState<EgressDecision[]>([]);
  const [approvals, setApprovals] = React.useState<ApprovalRequest[]>([]);
  const [audit, setAudit] = React.useState<AuditEvent[]>([]);
  // The run's session.recording(.write) events (RECORDING_ACTIONS), via a
  // SEPARATE action_prefix fetch so the general trail's cap can't crowd them out.
  const [recordingAudit, setRecordingAudit] = React.useState<AuditEvent[]>([]);
  // F6-F2: run.complete/run.kill/run.autostop are the LATEST events on a run's
  // trail — the first ones the 1000-row cap on `audit` above pushes off —
  // scoped-fetched the same way session.recording.write is, so the exit code and
  // ending derivation stay known past that cap.
  const [endingAudit, setEndingAudit] = React.useState<AuditEvent[]>([]);
  // #1487: this run's credential.mint rows, fetched with their OWN action filter
  // (never read off the 1000-row trail above) so a kill outcome can name what
  // the run held. undefined = not fetched yet; null = the fetch failed. Joined
  // to `grants` for the kind and host by held-credentials.ts.
  const [endingState, setEndingState] = React.useState<"loading" | "ready" | "failed">("loading");
  const [mintAudit, setMintAudit] = React.useState<AuditEvent[] | null | undefined>(undefined);
  const [grantsReadable, setGrantsReadable] = React.useState(false);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [tab, setTab] = React.useState<Tab>("overview");
  // Recording is fetched lazily the first time the Recording tab opens.
  const [recording, setRecording] = React.useState<Recording | null>(null);
  const [recState, setRecState] = React.useState<"idle" | "loading" | "error" | "ready">("idle");
  // Which cast to replay: the run's own (stored under the bare run id) or one
  // interactive attach session (the composite `<run-id>~<session-uuid>` key).
  const [recKey, setRecKey] = React.useState(id);
  // Now the shared hook: the same /healthz read the Recordings
  // library and the New Run rail make. See use-recording-disabled.ts.
  const recordingDisabled = useRecordingDisabled() === true;

  // How a terminal run ended, read off its own scoped audit rows (#1487): the
  // facts that decide a KILLED run's outcome. The run page renders without them;
  // the KILLED outcome block and the Kill-again offer wait for them rather than
  // guess ("no kill record" before the rows landed is a claim, not a loading
  // state). A generation counter drops an older read that answers after a newer
  // one. Best-effort: a failed read leaves the last-good rows, and a failed mint
  // read is "couldn't read which credentials".
  const settleSince = React.useRef<number | null>(null);
  const endingGen = React.useRef(0);
  const endingInFlight = React.useRef(false);
  const loadEnding = React.useCallback(() => {
    // One read at a time: a poll that keeps asking while a slow read is still out
    // would stack four more calls every tick.
    if (endingInFlight.current) return;
    endingInFlight.current = true;
    const gen = ++endingGen.current;
    void Promise.allSettled([
      auditApi.listAudit(id, { action: "run.complete" }),
      auditApi.listAudit(id, { action: "run.kill" }),
      auditApi.listAudit(id, { action: "run.autostop" }),
      auditApi.listAudit(id, { action: "credential.mint" }),
    ]).then(([done, kill, autostop, minted]) => {
      endingInFlight.current = false;
      if (endingGen.current !== gen) return;
      if (done.status === "fulfilled" && kill.status === "fulfilled" && autostop.status === "fulfilled") {
        setEndingAudit([done.value, kill.value, autostop.value].flat());
        setEndingState("ready");
      } else {
        setEndingState((s) => (s === "ready" ? s : "failed"));
      }
      setMintAudit(minted.status === "fulfilled" ? minted.value : null);
    });
  }, [id]);

  // Core fetch — run + its grants, egress, approvals, and audit trail.
  const load = React.useCallback(
    (foreground: boolean) => {
      if (!id) return;
      if (foreground) setStatus("loading");
      // RETURNED, not fired and forgotten: usePoll's in-flight guard waits on
      // this promise, so a control plane slower than DETAIL_POLL_MS costs one
      // outstanding set of requests instead of a new set every 4s (R4-F074).
      // Egress is derived from the same audit events we already fetch here — call
      // egressFromAudit(a) instead of api.getEgress (which would re-fetch /audit).
      // allSettled, NOT all. The PAGE is the run; the other four are panes on
      // it — a single subsidiary rejection under Promise.all would replace the
      // whole cockpit (run state, live terminal, approvals strip and the KILL
      // button for a RUNNING run) with ErrorState's "we couldn't reach the
      // Wardyn control plane", an outage claim that is false when GET
      // /runs/{id} just returned 200. The rejection is routine, not
      // hypothetical: handleListApprovals answers 500 "approval listing is not
      // scoped for members on this backend" on a backend without
      // ApprovalsByRunCreatorPager (internal/api/approvals.go), and a degraded
      // audit store fails listAudit. Each pane keeps its last-good value and
      // the page stays operable.
      return Promise.allSettled([
        runsApi.getRun(id),
        runsApi.getGrants(id),
        // Scoped SERVER-side (?run_id=). Filtering this list in the browser
        // instead drops the run's own approvals once the fleet has more than
        // LIST_LIMIT lifetime rows — see approvals.ts's listApprovals comment
        // and internal/api/approvals.go:56-61.
        approvalsApi.listApprovals("", id),
        auditApi.listAudit(id),
        // "session.recording" names the ACTION FAMILY, prefixing both the old
        // and new action names — not itself a legacy action name (Conductor ruling, #1062).
        auditApi.listAudit(id, { actionPrefix: "session.recording" }),
      ])
        .then(([r, g, runApprovals, a, recA]) => {
          // A run answer for a different id than this page shows is dropped:
          // the page never renders, or acts on, a run it was not asked for.
          // Case-insensitive: a non-canonical UUID in the URL is still this run.
          if (r.status === "fulfilled" && r.value && r.value.id.toLowerCase() !== id.toLowerCase()) return;
          if (r.status === "rejected") {
            // The run itself is the one fetch this page cannot render without.
            // Foreground load shows the error state; a background poll blip
            // keeps last-good data silently (matches the Runs board) rather
            // than replacing a live cockpit every DETAIL_POLL_MS during a
            // control-plane hiccup.
            if (foreground) setStatus("error");
            return;
          }
          setRun(r.value ?? null);
          setGrantsReadable(g.status === "fulfilled");
          if (g.status === "fulfilled") setGrants(g.value);
          if (a.status === "fulfilled") {
            setEgress(egressFromAudit(a.value));
            setAudit(a.value);
          }
          // The ?run_id= above is what makes this list this run's; the filter
          // is a belt-and-braces no-op kept so a backend that ignored the
          // predicate cannot leak another run's rows onto this page.
          if (runApprovals.status === "fulfilled")
            setApprovals(runApprovals.value.filter((x) => x.run_id.toLowerCase() === id.toLowerCase()));
          if (recA.status === "fulfilled") setRecordingAudit(recA.value);
          setStatus("ready");
          // R-5: run.complete/run.kill/run.autostop cannot exist for a run that
          // ISN'T terminal — fetching them every DETAIL_POLL_MS tick on a live
          // run would be wasted round-trips, forever. Gated on THIS tick's own
          // fresh state, so the exact tick a run turns terminal is the one that
          // catches it. DETACHED: the run has already rendered, and this poll's
          // in-flight guard must not wait on four more calls (a hang costs 60s).
          if (r.value && isTerminalRunState(r.value.state)) loadEnding();
        })
        .catch(() => {
          // allSettled never rejects, so this is a bug in the block above, not
          // a network answer. Same foreground rule.
          if (foreground) setStatus("error");
        });
    },
    [id, loadEnding],
  );

  React.useEffect(() => {
    setRun(undefined);
    setStatus("loading");
    // Reset recording state on run-id change too: without it, the Recording
    // tab would keep showing the PREVIOUS run's cast (labelled as this run)
    // until something else touched recState — the lazy-load effect below
    // only fetches when recState === "idle", so a stale "ready"/"error" from
    // the last run id would block the refetch entirely.
    setRecording(null);
    setRecState("idle");
    setRecKey(id);
    void load(true);
  }, [id, load]);

  const terminal = run ? isTerminalRunState(run.state) : true;

  // Lazy recording load on first Recording-tab open (and on each session pick,
  // which resets recState to "idle").
  //
  // ALSO for a finished run sitting on Overview: its terminal pane replays the
  // cast in place (design board 2d's fourth state), so the fetch can no longer
  // be keyed on the Recording tab alone. Still lazy — a LIVE run on Overview
  // fetches nothing, which is the common case.
  const wantsRecording = tab === "recording" || (tab === "overview" && terminal && !!run);
  // F1-F2: without an ordering guard, a slow fetch for an earlier-selected
  // cast (recKey A) could resolve AFTER a later selection (recKey B) and
  // overwrite it, or setState after unmount. NOT a plain `let alive` + cleanup (the
  // sibling pattern in run-context-row.tsx/run-detail-ssh.tsx): this effect's
  // own setRecState("loading") is itself a dependency-array member, so a
  // cleanup tied to every re-run would invalidate the very request it just
  // started. A generation counter only advances when a NEW fetch actually
  // starts, so it survives the effect's own idle->loading->ready churn.
  const recRequest = React.useRef(0);
  React.useEffect(() => {
    if (!wantsRecording || !id || recState !== "idle") return;
    const thisRequest = ++recRequest.current;
    setRecState("loading");
    recordingsApi
      .getRecording(id, recKey || id)
      .then((rec) => {
        if (recRequest.current !== thisRequest) return;
        setRecording(rec ?? null);
        setRecState("ready");
      })
      .catch(() => {
        if (recRequest.current === thisRequest) setRecState("error");
      });
  }, [wantsRecording, id, recKey, recState]);
  // F1-F2's other half: setState after unmount. The counter must also
  // advance on teardown, not only when a new fetch starts — -1 never
  // matches a real (>=1) generation, so any in-flight fetch's callback is
  // permanently a no-op once this component is gone.
  React.useEffect(() => () => { recRequest.current = -1; }, []);

  // ----- top-level states -----
  const pending = approvals.filter((a) => a.state === "PENDING");
  // F6-F2: run.complete/run.kill/run.autostop rows land here even when the
  // capped `audit` trail above dropped them — duplicates are harmless, both
  // derivations below keep the last/first matching row regardless.
  const endingEvents = [...audit, ...endingAudit];
  // #1487: what a killed run held that Wardyn cannot take back. undefined until
  // the terminal-run fetch has answered, so nothing says "couldn't read" for the
  // beat before it lands.
  const held =
    mintAudit === undefined ? undefined : heldCredentials(grantsReadable ? grants : undefined, mintAudit ?? undefined);
  // Kill again is offered on a KILLED run whose trail does not PROVE the
  // teardown: the server re-kills a KILLED run (runs_lifecycle.go), and the
  // cascade is safe to repeat — the one action that settles the doubt.
  const killEvidence = run?.state === "KILLED" ? runEndingFromAudit(run.state, endingEvents)?.evidence : undefined;
  // Kill again follows the evidence: offered once the ending facts have settled
  // (or failed to read) and the trail does not PROVE the teardown. A failed read
  // does not hide a row the main trail already holds, so a proven kill stays off.
  const killAgain = run?.state === "KILLED" && endingState !== "loading" && killEvidence !== "confirmed";
  // The outcome block of a KILLED run waits for its facts (see loadEnding). When
  // the read failed it still shows what the main trail proves, and nothing when
  // that holds no kill row either (no row would read as "no kill record", which a
  // failed read cannot claim).
  const outcomeReady = run?.state !== "KILLED" || endingState === "ready" || (endingState === "failed" && killEvidence !== "unknown");
  // The server marks a run KILLED BEFORE it writes the run.kill row
  // (runs_lifecycle.go), so a read can land in between. Keep polling a KILLED run
  // whose trail does not yet PROVE the teardown for two minutes, counted on THIS
  // page's clock from when it first saw such a run (a ref, so it resets on
  // remount): the server's ended_at against our Date.now() would depend on skew.
  // The row arrives, or the wait gives up and the person can kill again. A failed
  // ending read is retried on the same polls (load re-calls loadEnding).
  const unconfirmedKill = run?.state === "KILLED" && killEvidence !== "confirmed";
  if (unconfirmedKill && settleSince.current === null) settleSince.current = Date.now();
  const killSettling = unconfirmedKill && Date.now() - (settleSince.current ?? Date.now()) < KILL_SETTLE_MS;
  usePoll(() => load(false), DETAIL_POLL_MS, terminal && !killSettling);

  return {
    run, grants, egress, approvals, audit, recordingAudit, status, tab, setTab,
    recording, recState, setRecState, recKey, setRecKey, recordingDisabled,
    load, terminal, pending, endingEvents, held, killAgain, outcomeReady,
  };
}
