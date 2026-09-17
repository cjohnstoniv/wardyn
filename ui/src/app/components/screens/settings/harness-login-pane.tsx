/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// HarnessLoginPane — "Connect via container login" for a Claude subscription in
// deployments with no host ~/.claude (compose/team). It launches an interactive
// login sandbox IMMEDIATELY (opening the pane IS the intent — no extra button),
// embeds the AttachTerminal, AUTO-TYPES `claude setup-token`, AUTO-OPENS the
// printed OAuth URL in a new browser tab, and AUTO-CAPTURES the printed
// long-lived token straight off the terminal stream — then stores it and injects
// it proxy-side into every later run; the sandbox never holds a live credential.
// The only unavoidable human step is approving the OAuth in the browser and
// pasting the callback code back into the terminal. A manual paste field remains
// as a fallback if auto-capture misses. Renders inline (never routes away).
//
// The AWS flow adds one step BEFORE the sandbox launches: it asks for the
// organization's access portal (start) URL, because `aws sso login` reads
// sso_start_url + sso_region from ~/.aws/config and Wardyn stores no start URL
// (the region is daemon boot config). The server seeds both into the sandbox as
// a credential-free ~/.aws/config, which is what makes the auto-typed
// `aws sso login --sso-session wardyn …` run unattended.
import * as React from "react";
import { Loader2, ShieldCheck, TriangleAlert, KeyRound, Square, ExternalLink, CornerDownLeft } from "lucide-react";
import { harnessAuth as harnessAuthApi } from "../../../lib/api/harness-auth";
import { runs as runsApi } from "../../../lib/api/runs";
import { setup as setupApi } from "../../../lib/api/setup";
import { isTerminalRunState, type SetupStatus } from "../../../lib/types";
import { usePoll } from "../../../lib/use-poll";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { AttachTerminal, type AttachTerminalHandle } from "../../attach-terminal";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";

// DRAFT (M2 canon pending) — S-13 (blind security review, lens-S.md): the PTY
// success marker is sandbox-forgeable by construction (a replaced/malicious
// login image can print it without ever completing a real capture), so the
// pane no longer trusts the marker alone. On doneMarker it re-fetches
// /setup/status and only claims a capture when the server independently
// agrees (serverConfirmsCapture below) — the server is the one copy of the
// truth the sandbox cannot write. This is the sentence shown when the two
// disagree.
//
// 0.7.4 field report, finding 7: the old sentence ("— sign in again.") told a
// person whose interrupted first attempt had just been retried SUCCESSFULLY to
// do the thing they had just done. The two causes need different actions from
// the human and the pane cannot tell them apart, so the sentence names both and
// what each one costs: retry (now safe — a new sign-in closes the old one,
// harnesscred_supersede.go) or escalate. Exported so ui/e2e and the tests assert
// THROUGH the constant rather than re-typing it.
export const CAPTURE_NOT_CORROBORATED =
  "The sandbox reported a capture the server does not have. If your last attempt was interrupted, sign in again from " +
  "Getting Started — starting a new sign-in closes the old one. If it keeps happening, tell your admin: the sandbox's " +
  "report and the server disagree.";
// DRAFT (M2 canon pending) — R-3: getSetupStatus RESOLVES a synthetic
// `unreachable` payload for a 5xx or a dropped socket, it does not throw. An
// honest capture that DID land would then be accused of not existing. One
// retry, then this: the check failed, not the sign-in.
const CAPTURE_CHECK_UNREACHABLE = "Wardyn couldn't reach the server to verify this sign-in — try again.";
// DRAFT (M2 canon pending) — R-8: the corroboration is a round trip the
// operator otherwise experiences as a terminal that stopped scrolling. Says
// what is happening WITHOUT claiming the capture the server has not confirmed.
const CAPTURE_VERIFYING = "Checking with Wardyn that the session was stored…";
// DRAFT (M2 canon pending) — U2-05 (blind round 2, lens-U2): the refusal
// sentence below the lead-in is the SANDBOX's prose, printed by
// cmd/wardyn-aws-sso — the very binary a forged login image replaces (the S-13
// threat model, applied to the success path). Rendered bare inside Wardyn's
// own warning box it read as Wardyn's finding. This fixed, Wardyn-authored
// lead-in names the speaker; FAIL_SENTENCE_MAX bounds what the speaker gets to
// say, client-side, rather than trusting the helper's own 300-rune cap.
export const SANDBOX_REFUSAL_LEAD_IN = "The login sandbox reported:";
// Same bound wardyn-aws-sso applies (main.go), re-applied where a replaced
// image cannot reach it.
const FAIL_SENTENCE_MAX = 300;
// RV-03: and the same ANSI strip, for the same reason — CSI (colour, cursor),
// OSC (title/hyperlink, terminated by BEL or ST) and the bare Fe escapes. A
// sandbox that can print its own sentence can print escape bytes around it;
// stripping them here means the 300-char budget is spent on characters the
// operator actually reads, and the alert renders text rather than control
// codes. Runs BEFORE the cap.
// eslint-disable-next-line no-control-regex
const ANSI_ESCAPES = /\u001b(?:\][^\u0007\u001b]*(?:\u0007|\u001b\\)?|\[[0-9;:?]*[ -/]*[@-~]|[@-Z\\-_])/g;

// One retry of an unreachable corroboration read, then give up. Not a
// propagation wait: the helper prints its marker only after the server has
// answered 204, so the store write strictly precedes the marker byte. This
// covers transport only.
const CONFIRM_RETRY_MS = 500;

// …and up to three re-reads of a status that ANSWERED but does not yet show
// this run's capture (finding 7). The marker strictly follows the 204, so the
// write precedes the read in real time — but "precedes" is not "is visible to":
// a read served by a lagging replica, or a supersede's kill landing between the
// two, produces a status that is honest and stale. Refusing on the first such
// read is what told a person whose retry WORKED to sign in again. Three reads
// over 1.5s is longer than any such gap and short enough that a genuinely forged
// marker (which never becomes true) still ends in the refusal.
const CAPTURE_CONFIRM_RETRIES = 3;

// "intro" is the consent gate: nothing launches until the operator has read
// what is about to happen and clicked Start. The pane used to fire on mount —
// a dialog, then suddenly a terminal, then suddenly a browser auth prompt,
// with nothing saying what was coming or what would be asked of you.
type Phase = "intro" | "prompt" | "launching" | "starting" | "attached" | "saving" | "done" | "error";

// How often the pane asks whether the login sandbox is up yet. Same cadence
// demo-runner's own launch→starting→live machine uses, and for the same reason:
// it is a human watching a pull, not a control loop.
const RUN_POLL_MS = 2000;

// How many CONSECUTIVE unreadable polls end the wait. A single failed read is a
// blip and must not end a sign-in that is working; a PERSISTENT one (the daemon
// restarted mid-pull, the run was pruned, a roster edit made the read a 403)
// otherwise leaves "Starting the sign-in sandbox…" on screen forever with
// nothing but Cancel to end it. 15 ticks ≈ 30s of silence — far longer than any
// restart, far shorter than the sandbox's 30-minute idle cap. Reset by any
// successful read, so a flaky link never accumulates its way to a false ending.
const RUN_POLL_MAX_CONSECUTIVE_FAILURES = 15;

// DRAFT (M2 canon pending) — P5: POST /setup/harness-login now answers with the
// run id BEFORE the sandbox exists (internal/api/harnesscred_launch.go), so the
// pane has a real wait to narrate. It used to have none: it set "attached" on
// the POST's resolve and mounted the terminal on a run that was still PENDING,
// which handleAttachTicket 409s — and a mint failure is TERMINAL in
// AttachTerminal, so the operator's only signal was a dead panel. Names the
// cold pull, because that is what the wait usually is.
const LOGIN_SANDBOX_STARTING =
  "Starting the sign-in sandbox — the first start after an upgrade pulls the image and can take a couple of minutes.";
// DRAFT (M2 canon pending) — the same wait ending badly on a run that carries
// no failure_hint of its own (a kill, a stop). Says only what is known: the
// sandbox is gone and nothing was captured.
const LOGIN_SANDBOX_ENDED = "The sign-in sandbox stopped before it was ready — nothing was captured. Try again.";
// DRAFT (M2 canon pending) — the wait ending because Wardyn can no longer READ
// the run (a daemon restart mid-pull, a pruned run, a 403 after a roster edit).
// Distinct from the sentence above on purpose: that one asserts the sandbox
// stopped, which this pane has not established — all it knows is that it stopped
// being able to ask.
const LOGIN_SANDBOX_UNREADABLE =
  "Wardyn stopped being able to read the sign-in sandbox, so it can't say whether it came up. Try again.";

// DRAFT (M2 canon pending) — THE SANDBOX SIGNS ITSELF IN NOW. The aws-sso image
// starts the chained command in its own tmux session BEFORE its prep
// (deploy/images/aws-sso/agent-run → signin-pane.sh), and every attach path —
// this pane, the Runs list, `wardyn attach`, ssh — joins that one session. So the
// pane no longer hands AttachTerminal an `autoRun` for aws: that unconditional
// type would land on the running login's stdin and run a SECOND wardyn-aws-sso in
// the same run, which the server refuses as already_captured — a fail marker on a
// sign-in that worked.
//
// It cannot simply stop typing either. The aws-sso tag is version-locked on the
// ghcr default, but an operator WARDYN_AGENT_IMAGES pin (what private estates use)
// makes a console-N+1 / image-N pairing real, and on an image-N sandbox nothing
// types the pair at all — finding 4 again. So: wait out a grace window and type
// ONLY if the sandbox has not announced itself. The image prints this marker as
// its FIRST act, before its own prep wait, precisely so it beats this timer; the
// grace is long enough for a slow first paint and far shorter than the device
// code's ~600 s life.
//
// The timer lives HERE, never in AttachTerminal: the Runs-list mount
// (run-detail/terminal-notice.tsx) shares that component, and a terminal that
// types on its own is how a read-only viewer would start a second sign-in.
export const SELFRUN_MARKER = "wardyn: sign-in running";
const SELFRUN_GRACE_MS = 12_000;

// Per-provider login conventions. Adding a provider is a new row here (mirrors
// the server-side agentHarnessLogin table), not a forked component.
//
// The two flows differ in HOW the credential comes back:
//   · anthropic — `claude setup-token` PRINTS the token, so we scrape it off the
//     PTY and PUT it (capture: "scrape").
//   · aws — `aws sso login` writes its token to ~/.aws/sso/cache/*.json and
//     prints only a short-lived device code + verification URL. The in-sandbox
//     `wardyn-aws-sso` helper uploads the file through the brokered internal
//     endpoint, so the pane never sees (and must never scrape) a credential —
//     it just watches for the helper's success marker (capture: "helper").
type CaptureMode = "scrape" | "helper";
type LoginFlow = {
  cmd: string;
  title: string;
  blurb: React.ReactNode;
  capture: CaptureMode;
  // What the "done" phase's success line names as connected — provider-specific
  // so an AWS SSO capture never claims a Claude subscription (or vice versa).
  doneLabel: string;
  // Marker the in-sandbox helper prints on success (capture: "helper" only).
  doneMarker?: string;
  // Marker the in-sandbox helper prints on a refused capture (capture: "helper"
  // only). Unused for now — cmd/wardyn-aws-sso's TestFailMarker_UIParity reads
  // this literal by source parse, the same way TestSuccessMarker_UIParity reads
  // doneMarker above, so it stays byte-identical across the two languages.
  failMarker?: string;
  // The flow cannot start until the operator supplies their AWS access-portal
  // start URL: `aws sso login` reads sso_start_url + sso_region from the
  // sandbox's ~/.aws/config, and Wardyn stores no start URL anywhere (the region
  // is boot config; the start URL is per-organization and asked for here). The
  // server seeds both into the sandbox before the command is auto-typed.
  needsStartUrl?: boolean;
  // "What happens next" — shown BEFORE anything launches (the intro phase, or
  // above the AWS start-URL form), so the terminal and the browser auth prompt
  // arrive announced. Includes what is required of the operator.
  expects: React.ReactNode[];
};

// isLikelyStartUrl mirrors the server's validateSSOStartURL (harnesscred.go) so
// the operator sees the problem before a round trip. Deliberately loose — the
// server is the authority, and the egress policy, not this check, decides what
// the sandbox may dial. Exported for tests.
export function isLikelyStartUrl(s: string): boolean {
  const v = s.trim();
  return /^https:\/\/[^\s/]+/.test(v);
}

const LOGIN_FLOWS: Record<string, LoginFlow> = {
  anthropic: {
    cmd: "claude setup-token",
    title: "Connect a Claude subscription via container login",
    capture: "scrape",
    doneLabel: "your Claude subscription is connected",
    expects: [
      <>
        A sandboxed login run starts and a terminal appears here, running{" "}
        <code className="rounded bg-background/70 px-1 py-0.5 font-mono">claude setup-token</code>. Nothing on this
        machine is touched.
      </>,
      <>
        A new tab opens on claude.ai asking you to sign in and approve — you&apos;ll need an active Claude
        subscription. If the pop-up is blocked, a click-through link appears here instead.
      </>,
      <>Some logins hand you a code: paste it into the field under the terminal, not the terminal itself.</>,
      <>
        The token it prints is captured, stored write-only, and the login sandbox is shut down. Runs get it injected
        proxy-side — a run&apos;s sandbox never holds it.
      </>,
    ],
    blurb: (
      <>
        Wardyn opened a sandbox and is running{" "}
        <code className="rounded bg-background/70 px-1 py-0.5 font-mono">claude setup-token</code> for you. It opens the
        Claude login page in a new tab — approve it, then paste the code it gives you into the{" "}
        <span className="font-medium">field below</span> (not the terminal) and hit Send. Wardyn captures the printed
        token automatically and connects your subscription; the token is injected proxy-side into every run and the
        sandbox never holds a live credential.
      </>
    ),
  },
  aws: {
    // --sso-session wardyn selects the [sso-session wardyn] block the server
    // seeded into ~/.aws/config; chained so the helper uploads the moment the
    // login succeeds — the operator never has to run a second command.
    cmd: "aws sso login --sso-session wardyn --no-browser --use-device-code && wardyn-aws-sso",
    title: "Connect an AWS SSO session via container login",
    doneLabel: "your AWS SSO session is connected",
    capture: "helper",
    doneMarker: "wardyn: aws sso credential captured",
    failMarker: "wardyn: aws sso credential rejected:",
    needsStartUrl: true,
    expects: [
      <>
        A sandboxed login run starts and a terminal appears here, running{" "}
        <code className="rounded bg-background/70 px-1 py-0.5 font-mono">aws sso login</code> — with no credential to
        start from.
      </>,
      <>
        A browser tab opens the AWS verification page: enter the short code the terminal shows and approve with your
        IAM Identity Center login.
      </>,
      <>
        The SSO session is uploaded from inside the sandbox and stored write-only; Bedrock runs exchange it for
        short-lived role credentials.
      </>,
    ],
    blurb: (
      <>
        Give Wardyn your organization&apos;s AWS access portal URL and it opens a sandbox, writes a minimal{" "}
        <code className="rounded bg-background/70 px-1 py-0.5 font-mono">~/.aws/config</code> holding just that URL and
        the configured SSO region (no credential — the sandbox has none to start with), and runs{" "}
        <code className="rounded bg-background/70 px-1 py-0.5 font-mono">aws sso login</code> for you. It prints a
        verification URL and a short user code — open the link in any browser, enter the code, and approve. Wardyn then
        captures the SSO session automatically so later Bedrock runs can exchange it for short-lived role credentials —
        with no host <code className="rounded bg-background/70 px-1 py-0.5 font-mono">~/.aws</code> mount and no static
        keys.
      </>
    ),
  },
};

export function loginFlow(provider: string): LoginFlow {
  return LOGIN_FLOWS[provider] ?? LOGIN_FLOWS.anthropic;
}

// Force the login terminal wide so `claude setup-token` never hard-wraps the
// OAuth URL (~250 chars) or the token across lines — a narrow PTY wrap mid-URL
// dropped response_type=code and produced "Invalid OAuth Request" on the opened
// tab. AttachTerminal pins BOTH the PTY and the visual xterm grid to this width
// (the pane scrolls horizontally): the CLI is a full-screen TUI that
// cursor-addresses whatever grid it is told, so a decoupled wide-PTY/narrow-view
// split interleaved its redraw frames into garbage on screen. Wrap-proofing the
// extractors instead is a dead end — rejoining a wrapped URL cannot tell where
// the URL ends and the next word begins, and a fused tail corrupts &state=.
const LOGIN_PTY_COLS = 512;

// With the single wide grid above, `claude setup-token` prints the OAuth URL and
// the sk-ant-oat token each on a SINGLE line, so these two single-line
// extractors are correct and need no reassembly.

// extractSetupToken pulls a COMPLETE `claude setup-token` token out of a chunk of
// terminal output. Shape: `sk-ant-oat<2 digits>-<long url-safe body>`. We only
// return a match followed by another character (newline, ANSI reset, …) — proof
// the token finished printing — so a token still streaming in (truncated at the
// buffer's end) is not captured early. Exported for tests.
export function extractSetupToken(s: string): string | null {
  const re = /sk-ant-oat\d{2}-[A-Za-z0-9_-]{40,}/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(s)) !== null) {
    if (m.index + m[0].length < s.length) return m[0];
  }
  return null;
}

// extractFailSentence pulls the sentence off wardyn-aws-sso's refusal line:
// `<marker> <sentence>`, one line, printed on a REFUSED capture (a
// wrong-account pin, a portal error — never on success, where doneMarker
// prints instead). Same trailing-boundary rule as the other extractors: only
// returns once the line has actually finished printing (a trailing newline),
// so a still-streaming prefix is never read as the whole refusal. U2-05: and
// it is stripped of ANSI escapes and capped HERE, at FAIL_SENTENCE_MAX, so
// both survive a replaced login image (RV-03). Exported for tests.
export function extractFailSentence(s: string, marker: string): string | null {
  const idx = s.indexOf(marker);
  if (idx === -1) return null;
  const rest = s.slice(idx + marker.length);
  const nl = rest.indexOf("\n");
  if (nl === -1) return null;
  return rest.slice(0, nl).replace(/\r$/, "").replace(ANSI_ESCAPES, "").trim().slice(0, FAIL_SENTENCE_MAX);
}

// extractAuthUrl pulls the `claude setup-token` OAuth authorization URL out of a
// chunk of terminal output so we can open it in a new tab. Restricted to the known
// Claude/Anthropic auth hosts (never api.anthropic.com — that's the token exchange,
// not a user-facing page). Same trailing-boundary rule as the token so a
// still-streaming URL isn't opened truncated. Exported for tests.
export function extractAuthUrl(s: string): string | null {
  const re = /https:\/\/(?:claude\.ai|claude\.com|console\.anthropic\.com|platform\.claude\.com)\/[^\s'"<>]+/gi;
  let m: RegExpExecArray | null;
  while ((m = re.exec(s)) !== null) {
    if (m.index + m[0].length < s.length) return m[0].replace(/[.,)]+$/, "");
  }
  return null;
}

// extractDeviceVerificationUrl pulls the AWS SSO device-authorization verification
// URL out of `aws sso login --no-browser --use-device-code` output. Restricted to
// the IAM Identity Center device endpoint + the org access portal; the CLI prints
// both a bare URL and (usually) a `verificationUriComplete` with ?user_code=…,
// and we prefer the complete one since it pre-fills the code. Same
// trailing-boundary rule as the others so a still-streaming URL isn't opened
// truncated. Exported for tests.
function extractDeviceVerificationUrl(s: string): string | null {
  const re = /https:\/\/(?:device\.sso\.[a-z0-9-]+\.amazonaws\.com|[a-z0-9-]+\.awsapps\.com)\/[^\s'"<>]*/gi;
  let best: string | null = null;
  let m: RegExpExecArray | null;
  while ((m = re.exec(s)) !== null) {
    if (m.index + m[0].length >= s.length) continue; // still streaming
    const url = m[0].replace(/[.,)]+$/, "");
    // Prefer the pre-filled variant so the operator doesn't retype the code.
    if (url.includes("user_code=")) return url;
    best = url;
  }
  return best;
}

// serverConfirmsCapture corroborates a doneMarker sighting against the
// server's own /setup/status (S-13): the marker is a PTY string a forged
// sandbox binary can print unconditionally, so it is never sufficient on its
// own.
//
// It has to prove THIS sign-in captured something, not that a credential
// exists (R-1) — otherwise every re-login over an existing row, the case the
// product's own "re-run the login" fix line creates, re-admits a forged
// marker. `source_run_id` is the proof: the server stamps it from the login
// run's own token claims (ssotoken.go), nothing in the sandbox can write it,
// and an honest capture replaces the blob, so after one it always equals this
// run's id. A row carrying SOMEONE ELSE'S run id is therefore a previous
// sign-in, and refuses.
//
// The presence legs below are the FALLBACK, for a daemon old enough to omit
// source_run_id entirely. For aws that is model_access reading live/expiring
// — never the harness row, whose `captured` bit stays true for a dead
// credential (R-2). anthropic has no per-caller model_access shape here, so
// its own harness row is all it has.
//
// TODAY ONLY THE aws FLOW REACHES THIS (R-5): it is the only flow with a
// doneMarker. anthropic ends through saveToken (capture: "scrape"). Its
// branch is kept because it is the right rule for the next helper flow.
// Exported for tests.
export function serverConfirmsCapture(status: SetupStatus, provider: string, runId?: string | null): boolean {
  const rows = (status.harness ?? []).filter((h) => h.provider === provider);
  if (runId && rows.some((h) => h.captured && h.source_run_id === runId)) return true;
  if (rows.some((h) => h.source_run_id)) return false; // someone else's sign-in
  if (provider === "aws") {
    const state = status.model_access?.state;
    return state === "live" || state === "expiring";
  }
  return rows.some((h) => h.captured);
}

// The numbered "what happens next" — the consent gate's content. Each flow
// states its own steps and what is required of the operator.
function ExpectList({ items }: { items: React.ReactNode[] }) {
  return (
    <ol className="space-y-1.5">
      {items.map((item, i) => (
        <li key={i} className="flex gap-2 text-xs leading-relaxed text-muted-foreground">
          <span className="mt-px inline-flex size-4 shrink-0 items-center justify-center rounded-full border border-border font-mono text-meta text-foreground">
            {i + 1}
          </span>
          <span className="min-w-0">{item}</span>
        </li>
      ))}
    </ol>
  );
}

export function HarnessLoginPane({
  provider = "anthropic",
  startURLManaged = false,
  onDone,
  onCancel,
}: {
  provider?: string;
  // The ORG's access portal is already stored and the server will use it: this
  // sign-in runs under a per_user agent row (the member's Getting Started CTA,
  // and the admin's own sign-in on a per_user row of the Agents tab). The
  // start-URL PROMPT is then skipped for a one-line note — harnessLogin's
  // startUrl argument is ignored server-side under such a row, so asking was a
  // field whose value could not take effect, and every member had to hunt down a
  // URL their admin had already entered.
  //
  // Default false: the ordinary Settings flow (no row, or `shared`) has nothing
  // stored to sign in against, so it still asks.
  startURLManaged?: boolean;
  // Called after the token is captured (parent refreshes setup status + closes).
  onDone: () => void;
  // Called when the operator backs out before capturing.
  onCancel: () => void;
}) {
  const flow = loginFlow(provider);
  const askStartUrl = !!flow.needsStartUrl && !startURLManaged;
  const [phase, setPhase] = React.useState<Phase>(askStartUrl ? "prompt" : "intro");
  const [startUrl, setStartUrl] = React.useState("");
  const [runId, setRunId] = React.useState<string | null>(null);
  const [token, setToken] = React.useState("");
  const [error, setError] = React.useState("");
  const [autoCaptured, setAutoCaptured] = React.useState(false);
  // Whether the terminal was ever mounted on this run. The error phase keeps a
  // helper flow's terminal on screen for its SCROLLBACK (a refused capture's
  // only other artifact) — but a run that failed while still coming up has no
  // scrollback, and mounting AttachTerminal on it is the dead panel P5 is about:
  // the ticket mint 409s and one failed mint is terminal in that component.
  const [everAttached, setEverAttached] = React.useState(false);
  const [authUrl, setAuthUrl] = React.useState("");
  const [code, setCode] = React.useState("");

  const termRef = React.useRef<AttachTerminalHandle>(null);

  // Rolling buffer of recent PTY output + latches so we act on each thing once.
  const outBufRef = React.useRef("");
  const savedRef = React.useRef(false);
  const failedRef = React.useRef(false);
  const openedUrlRef = React.useRef(false);
  // Consecutive unreadable polls of the starting run, not a total: one blip must
  // not end a sign-in that is working. A ref, not state — it drives no render
  // and must not churn the poll callback's identity.
  const pollFailuresRef = React.useRef(0);
  // ONE self-run grace timer per launch, armed on the FIRST attach.
  const selfRunArmedRef = React.useRef(false);

  const launch = React.useCallback(async () => {
    setPhase("launching");
    setError("");
    outBufRef.current = "";
    savedRef.current = false;
    failedRef.current = false;
    openedUrlRef.current = false;
    setAutoCaptured(false);
    setEverAttached(false);
    pollFailuresRef.current = 0;
    selfRunArmedRef.current = false;
    setAuthUrl("");
    try {
      const id = await harnessAuthApi.harnessLogin(provider, startUrl.trim());
      // THE ID FIRST, THE TERMINAL LATER. Holding the id from t≈0 is what makes
      // Cancel able to kill a sandbox that is still coming up — before P5 the
      // POST did not answer until dispatch was done, so a timed-out launch left
      // an orphan nobody could name.
      setRunId(id);
      setPhase("starting");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setPhase("error");
    }
  }, [provider, startUrl]);

  // pollRun is the `starting` phase's whole machine: ask the run whether it is
  // up yet, mount the terminal when it is, and end the wait honestly when the
  // run ends instead. Nothing here attaches — AttachTerminal's own mint is
  // owner-or-admin and one failed mint is terminal in that component, so the
  // pane must not mount it until the ticket route would actually answer.
  const pollRun = React.useCallback(async () => {
    if (!runId) return;
    const run = await runsApi.getRun(runId).catch(() => undefined);
    if (!run) {
      // A transient read is not an outcome; the next tick asks again — until
      // enough of them fail in a row that "still starting" is a claim this pane
      // can no longer make.
      pollFailuresRef.current += 1;
      if (pollFailuresRef.current >= RUN_POLL_MAX_CONSECUTIVE_FAILURES) {
        setError(LOGIN_SANDBOX_UNREADABLE);
        setPhase("error");
      }
      return;
    }
    pollFailuresRef.current = 0;
    if (run.state === "RUNNING") {
      setEverAttached(true);
      setPhase("attached");
      return;
    }
    if (isTerminalRunState(run.state)) {
      // The run's OWN sentence when it has one (D9's failure_hint covers the
      // pre-agent-start class this wait actually hits: an image that would not
      // pull, a ceiling that would not resolve); never a reworded guess.
      setError(run.failure_hint || LOGIN_SANDBOX_ENDED);
      setPhase("error");
    }
  }, [runId]);

  // usePoll drives BACKGROUND refreshes only (its own contract), so the first
  // ask is made here — otherwise every sign-in waits a full tick on a sandbox
  // that may already be up.
  React.useEffect(() => {
    if (phase === "starting") void pollRun();
  }, [phase, pollRun]);
  usePoll(pollRun, RUN_POLL_MS, phase !== "starting");

  // The version-skew fallback, armed once on the first attach: if the sandbox
  // has not said it is signing in by the time the grace window closes, this is
  // an image that predates the self-run and nothing else will type the pair.
  // Four signals all mean "it IS signing in, do not touch it": the image's own
  // banner, a device URL already on screen, and either helper marker (a capture
  // that already landed, or one already refused).
  React.useEffect(() => {
    if (phase !== "attached" || flow.capture !== "helper" || selfRunArmedRef.current) return;
    selfRunArmedRef.current = true;
    const timer = setTimeout(() => {
      const buf = outBufRef.current;
      if (openedUrlRef.current || buf.includes(SELFRUN_MARKER)) return;
      if (flow.doneMarker && buf.includes(flow.doneMarker)) return;
      if (flow.failMarker && buf.includes(flow.failMarker)) return;
      termRef.current?.sendText(flow.cmd + "\r");
    }, SELFRUN_GRACE_MS);
    return () => clearTimeout(timer);
  }, [phase, flow]);

  // saveToken stores a token (explicit from auto-capture, or the pasted field).
  const saveToken = React.useCallback(
    async (explicit?: string) => {
      const t = (explicit ?? token).trim();
      if (!t || savedRef.current) return;
      savedRef.current = true;
      setPhase("saving");
      setError("");
      try {
        await harnessAuthApi.harnessCredentialPaste(provider, t);
        if (runId) await runsApi.killRun(runId).catch(() => {});
        setPhase("done");
        onDone();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
        setPhase("attached"); // stay on the terminal so they can retry the paste
        savedRef.current = false; // allow another attempt (auto or manual)
      }
    },
    [provider, token, runId, onDone],
  );

  // confirmCapture corroborates a helper doneMarker sighting against the
  // server (S-13) before the pane claims a capture. `savedRef` is already
  // latched by the caller so a repeat marker sighting can't re-enter this
  // while the fetch is in flight.
  //
  // The run is killed FIRST (R-7): the credential is already stored by the
  // time the helper prints its marker, so the login sandbox has no reason to
  // outlive it by a round trip, and ssotoken.go's already_captured latch
  // covers a repeat. Then "saving", so the spinner covers the wait (R-8).
  //
  // Three outcomes, not two. A DISAGREEMENT is the forgery this exists for. A
  // THROW (a propagated 401) is still fail-closed — never assume the marker
  // was honest because the check failed. An UNREACHABLE payload is neither:
  // it is the synthetic body getSetupStatus resolves for a 5xx or a dropped
  // socket, and an honest capture that DID land must not be accused of not
  // existing — retry once, then say the check failed (R-3).
  const confirmCapture = React.useCallback(async () => {
    if (runId) void runsApi.killRun(runId).catch(() => {});
    setPhase("saving");
    // A THROW stays fail-closed and is never retried: a propagated 401 is an
    // answer, not a blip, and retrying it would only delay the refusal.
    const read = async (): Promise<SetupStatus | null> => {
      try {
        return await setupApi.getSetupStatus();
      } catch {
        return null;
      }
    };
    let status = await read();
    if (status?.unreachable) {
      await new Promise((r) => setTimeout(r, CONFIRM_RETRY_MS));
      status = await read();
    }
    // THE READ TOLERATES ITS OWN WRITE (finding 7). The read above SUCCEEDED and
    // simply shows no row for this run yet — or shows the row of a sign-in the
    // supersede is in the middle of ending. Both converge within a tick or two,
    // so re-read before accusing the sandbox. A forged marker never converges
    // and still lands on the refusal 1.5s later.
    for (let i = 0; i < CAPTURE_CONFIRM_RETRIES; i++) {
      if (!status || status.unreachable || serverConfirmsCapture(status, provider, runId)) break;
      await new Promise((r) => setTimeout(r, CONFIRM_RETRY_MS));
      status = await read();
    }
    if (status && !status.unreachable && serverConfirmsCapture(status, provider, runId)) {
      setAutoCaptured(true);
      setPhase("done");
      onDone();
      return;
    }
    failedRef.current = true;
    setError(status?.unreachable ? CAPTURE_CHECK_UNREACHABLE : CAPTURE_NOT_CORROBORATED);
    setPhase("error");
  }, [provider, runId, onDone]);

  // Watch the login terminal: open the OAuth URL in a new tab, then capture and
  // save the printed token — both automatically.
  const handleOutput = React.useCallback(
    (chunk: string) => {
      outBufRef.current = (outBufRef.current + chunk).slice(-16384);
      if (!openedUrlRef.current) {
        const url = flow.capture === "helper" ? extractDeviceVerificationUrl(outBufRef.current) : extractAuthUrl(outBufRef.current);
        if (url) {
          openedUrlRef.current = true;
          setAuthUrl(url);
          // Best-effort auto-open. A browser may block a popup not tied to a user
          // gesture; the surfaced link below is the reliable one-click fallback.
          try {
            window.open(url, "_blank", "noopener,noreferrer");
          } catch {
            /* blocked — the visible link covers it */
          }
        }
      }
      if (savedRef.current || failedRef.current) return;
      // Helper-capture providers (AWS SSO): the credential is uploaded by the
      // in-sandbox helper through the brokered endpoint — it is NEVER printed, so
      // there is nothing to scrape. Watch only for the helper's success marker.
      if (flow.capture === "helper") {
        // Checked BEFORE doneMarker: a refused capture (wrong-account pin, a
        // portal error) prints the fail marker and NEVER the done marker —
        // without this the pane just sat on "waiting" forever, the only signal
        // a terminal that had quietly stopped scrolling.
        if (flow.failMarker) {
          const sentence = extractFailSentence(outBufRef.current, flow.failMarker);
          if (sentence) {
            failedRef.current = true;
            setError(`${SANDBOX_REFUSAL_LEAD_IN} ${sentence}`);
            setPhase("error");
            if (runId) void runsApi.killRun(runId).catch(() => {});
            return;
          }
        }
        if (flow.doneMarker && outBufRef.current.includes(flow.doneMarker)) {
          savedRef.current = true;
          void confirmCapture();
        }
        return;
      }
      const tok = extractSetupToken(outBufRef.current);
      if (tok) {
        setToken(tok);
        setAutoCaptured(true);
        void saveToken(tok);
      }
    },
    [saveToken, confirmCapture, flow, runId, onDone],
  );

  // Bridge the pasted login code into the terminal's stdin, so the operator uses
  // a normal input field with native paste instead of the terminal's Ctrl+Shift+V.
  const sendCode = React.useCallback(() => {
    const c = code.trim();
    if (!c) return;
    termRef.current?.sendText(c + "\r");
    setCode("");
  }, [code]);

  const cancel = React.useCallback(() => {
    if (runId) runsApi.killRun(runId).catch(() => {});
    onCancel();
  }, [runId, onCancel]);

  return (
    <div className="space-y-3 rounded-lg border border-border bg-surface-2/40 p-3" data-testid="harness-login-pane">
      <div className="flex items-center gap-2">
        <KeyRound className="size-4 shrink-0 text-primary" />
        <span className="text-sm font-medium text-foreground">{flow.title}</span>
      </div>
      {/* The blurb narrates the RUNNING flow ("Wardyn opened a sandbox…") — on
          the intro nothing has launched yet, so the expectations list speaks
          instead and the blurb would be a lie. */}
      {phase !== "intro" && <p className="text-xs leading-relaxed text-muted-foreground">{flow.blurb}</p>}

      {error && (
        <div
          role="alert"
          className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2 text-xs text-warning"
        >
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          <p>{error}</p>
        </div>
      )}

      {phase === "intro" && (
        <div className="space-y-3" data-testid="login-intro">
          <ExpectList items={flow.expects} />
          {/* The portal the sign-in will use is the row's, not one to type. */}
          {flow.needsStartUrl && startURLManaged && (
            <p className="text-xs leading-relaxed text-muted-foreground">{AGENTS.SSO_START_URL_MANAGED}</p>
          )}
          <div className="flex flex-wrap gap-2">
            <Button size="sm" onClick={() => void launch()}>
              <KeyRound className="size-3.5" /> Start login
            </Button>
            <Button size="sm" variant="outline" onClick={onCancel}>
              Cancel
            </Button>
          </div>
        </div>
      )}

      {phase === "prompt" && (
        <div className="space-y-2" data-testid="login-start-url-prompt">
          <ExpectList items={flow.expects} />
          <label className="block text-xs font-medium text-foreground" htmlFor="harness-login-start-url">
            Your AWS access portal URL
          </label>
          <div className="flex flex-wrap items-center gap-2">
            <Input
              id="harness-login-start-url"
              value={startUrl}
              onChange={(e) => setStartUrl(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && isLikelyStartUrl(startUrl)) void launch();
              }}
              placeholder="https://my-org.awsapps.com/start"
              className="h-9 min-w-[18rem] flex-1 font-mono"
              aria-label="AWS access portal start URL"
            />
            <Button size="sm" onClick={() => void launch()} disabled={!isLikelyStartUrl(startUrl)}>
              <KeyRound className="size-3.5" /> Start login
            </Button>
            <Button size="sm" variant="outline" onClick={onCancel}>
              Cancel
            </Button>
          </div>
          <p className="text-xs text-muted-foreground">
            Find it in the AWS access portal (IAM Identity Center) — it looks like{" "}
            <code className="rounded bg-background/70 px-1 py-0.5 font-mono">https://my-org.awsapps.com/start</code>.
            Wardyn does not store it; the SSO region comes from the daemon&apos;s{" "}
            <code className="rounded bg-background/70 px-1 py-0.5 font-mono">-bedrock-aws-sso-region</code> /{" "}
            <code className="rounded bg-background/70 px-1 py-0.5 font-mono">-bedrock-region</code> setting.
          </p>
        </div>
      )}

      {phase === "launching" && (
        <p className="flex items-center gap-2 text-xs text-muted-foreground">
          <Loader2 className="size-3.5 animate-spin" /> Opening the login sandbox…
        </p>
      )}

      {/* The wait, with the one control that matters during it: Cancel kills
          the run by the id the POST already handed back, so a sandbox stuck on
          a cold pull is the operator's to end rather than the idle cap's. */}
      {phase === "starting" && (
        <div className="flex flex-wrap items-center gap-2" data-testid="login-sandbox-starting">
          <p role="status" className="flex flex-1 items-center gap-2 text-xs leading-relaxed text-muted-foreground">
            <Loader2 className="size-3.5 shrink-0 animate-spin" /> {LOGIN_SANDBOX_STARTING}
          </p>
          <Button size="sm" variant="outline" onClick={cancel}>
            <Square className="size-3.5" /> Cancel
          </Button>
        </div>
      )}

      {phase === "error" && (
        <div className="flex flex-wrap gap-2">
          <Button size="sm" onClick={() => void launch()}>
            <KeyRound className="size-3.5" /> Try again
          </Button>
          {/* `cancel`, not a bare onCancel: an error can now arrive while the
              sandbox is still ALIVE — the wait ended because Wardyn stopped
              being able to read the run, not because the run stopped — and
              backing out of that without a kill is the orphan P5 exists to end.
              Harmless on the arms that already killed it (killRun on a dead run
              is a caught no-op) and on a launch that never got an id. */}
          <Button size="sm" variant="ghost" onClick={cancel}>
            Cancel
          </Button>
        </div>
      )}

      {/* A refused helper capture keeps the terminal mounted, read-only in
          effect: the run is already killed (the failMarker branch above), so
          the socket just closes — but the scrollback (device-code chatter,
          the portal's reply, the helper's own preceding lines) stays on
          screen beside the alert instead of vanishing with it, since the
          extracted sentence is the operator's only other artifact. The
          interactive bits below (paste boxes, the helper's own Cancel) are
          suppressed in error phase — Try again/Cancel above already cover it. */}
      {(phase === "attached" || phase === "saving" || (phase === "error" && flow.capture === "helper" && everAttached)) && runId && (
        <div className="space-y-2">
          {authUrl && phase !== "error" && (
            <a
              href={authUrl}
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex items-center gap-1.5 rounded-md border border-primary/40 bg-primary/10 px-2.5 py-1.5 text-xs font-medium text-primary hover:bg-primary/20"
              data-testid="auth-url-link"
            >
              <ExternalLink className="size-3.5" />{" "}
              {flow.capture === "helper" ? "Open the AWS verification page ↗" : "Open the Claude login page ↗"}
            </a>
          )}
          <AttachTerminal
            ref={termRef}
            runId={runId}
            /* helper flows (aws): the sandbox runs the pair itself — see
               SELFRUN_MARKER above. Every other provider still auto-types. */
            autoRun={flow.capture === "helper" ? undefined : flow.cmd}
            onOutput={handleOutput}
            ptyCols={LOGIN_PTY_COLS}
            heightClass="h-96"
          />
          {phase === "error" ? null : phase === "saving" && !autoCaptured && flow.capture === "helper" ? (
            /* R-8: the corroboration round trip, narrated. Deliberately NOT the
               "captured" note below — the server has not agreed yet. HELPER
               FLOWS ONLY: a scrape flow's `saving` with no autoCapture is a
               MANUAL token paste, which keeps its own row (and its own Save
               spinner) and would be told about an AWS sign-in it never made. */
            /* U2-07: a live region, like the error path's role="alert" beside
               it — a note that narrates a silent round trip is silence again
               for a screen-reader user. role="status" (polite), not "alert":
               this is one bounded fetch, not the Agents banner's poll loop. */
            <p
              role="status"
              className="flex items-center gap-2 text-xs text-muted-foreground"
              data-testid="capture-verifying-note"
            >
              <Loader2 className="size-3.5 animate-spin" /> {CAPTURE_VERIFYING}
            </p>
          ) : autoCaptured ? (
            <p className="flex items-center gap-2 text-xs text-success" data-testid="auto-capture-note">
              {phase === "saving" ? <Loader2 className="size-3.5 animate-spin" /> : <ShieldCheck className="size-3.5" />}
              {flow.capture === "helper"
                ? "SSO session captured — connecting…"
                : "Token detected — connecting your subscription…"}
            </p>
          ) : flow.capture === "helper" ? (
            /* Device-code flow: the code is entered on the AWS verification PAGE,
               not in the terminal, and the credential is uploaded by the in-sandbox
               helper — so there is no code field and nothing to paste here. */
            <div className="flex flex-wrap items-center gap-2">
              <p className="flex-1 text-xs leading-relaxed text-muted-foreground">
                Open the verification link above, enter the user code shown in the terminal, and approve. Wardyn
                captures the session automatically when the login completes.
              </p>
              <Button size="sm" variant="outline" onClick={cancel}>
                <Square className="size-3.5" /> Cancel
              </Button>
            </div>
          ) : (
            <div className="space-y-2">
              {/* Primary interaction: paste the login-page code; we type it into
                  the terminal's stdin so the operator never needs Ctrl+Shift+V. */}
              <label className="block text-xs font-medium text-foreground" htmlFor="harness-login-code">
                Paste the code from the login page
              </label>
              <div className="flex flex-wrap items-center gap-2">
                <Input
                  id="harness-login-code"
                  value={code}
                  onChange={(e) => setCode(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") sendCode();
                  }}
                  placeholder="paste the code Claude gave you, then press Enter"
                  className="h-9 min-w-[18rem] flex-1 font-mono"
                  aria-label="login code"
                />
                <Button size="sm" onClick={sendCode} disabled={!code.trim()}>
                  <CornerDownLeft className="size-3.5" /> Send code
                </Button>
                <Button size="sm" variant="outline" onClick={cancel}>
                  <Square className="size-3.5" /> Cancel
                </Button>
              </div>
              {/* Fallback: paste the final token directly if auto-capture missed. */}
              <details className="text-xs text-muted-foreground">
                <summary className="cursor-pointer select-none py-1">Token didn&apos;t auto-capture?</summary>
                <div className="mt-1 flex flex-wrap items-center gap-2">
                  <Input
                    value={token}
                    onChange={(e) => setToken(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key === "Enter") void saveToken();
                    }}
                    placeholder="paste the sk-ant-oat… token"
                    className="h-9 min-w-[18rem] flex-1 font-mono"
                    aria-label="setup-token"
                    type="password"
                  />
                  <Button
                    size="sm"
                    variant="secondary"
                    onClick={() => void saveToken()}
                    disabled={phase === "saving" || !token.trim()}
                  >
                    {phase === "saving" ? (
                      <Loader2 className="size-3.5 animate-spin" />
                    ) : (
                      <ShieldCheck className="size-3.5" />
                    )}
                    Save token
                  </Button>
                </div>
              </details>
            </div>
          )}
        </div>
      )}

      {phase === "done" && (
        <p className="flex items-center gap-2 text-xs text-success">
          <ShieldCheck className="size-3.5" /> Token captured — {flow.doneLabel}.
        </p>
      )}
    </div>
  );
}
