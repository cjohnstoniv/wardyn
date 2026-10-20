/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Your account -> My runner (0.9, packet M-RN-2 and mock M-POOLS v6): register a computer, claim it, set
// its limits, put it in pools of your own, see its status and revoke it; and My pools, the person's own
// user pools. User view only. The claim is the security step: a waiting runner shows only an abbreviated
// fingerprint and the claim takes the whole one from the person's own computer, so no fingerprint on this
// card has a copy control and the claim field is never pre-filled. Everything a claimed runner says about
// itself is labelled as reported. The card renders nothing while runners are off.
import * as React from "react";
import { useLocation } from "react-router-dom";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { RUNNERS_DISABLED, runners as runnersApi } from "../../../lib/api/runners";
import { absoluteTime, getErrorMessage, relativeTime } from "../../../lib/format";
import type { RunnerRegistrationToken, RunnerView } from "../../../lib/types/runners";
import { MY_POOLS, RES, RUNNER_V6 } from "../../wardyn/copy/runner-pools";
import { MY_RUNNER, RUNNER, RUNNERS } from "../../wardyn/copy/runners";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { makeMono, Mono } from "../../wardyn/code-block";
import { CopyButton } from "../../wardyn/copy-button";
import { usePrincipal } from "../../wardyn/operator-context";
import { STATES } from "../../wardyn/states";
import { Button } from "../../ui/button";
import { ConfirmDialog } from "../runners/confirm-dialog";
import { LimitsDialog } from "../runners/limits-dialog";
import { registerCommand, StartRunnerSteps } from "../runners/mint-token-dialog";
import { Hint, KindChip, OfflineNote, ResourceTable, RunsLine, Sec } from "../runners/resource-table";
import { amountsText, proposeLimits, runnerRes } from "../runners/resources";
import { ArchText, MachineText } from "../runners/runner-card";
import { RunnerChip } from "../runners/runner-chip";
import { CHIP_WORD, postureLines, runnerChipKind, shortDate, sortRunners, timeLeft } from "../runners/runner-display";
import { ClaimRunnerDialog } from "./claim-runner-dialog";
import { MyPoolsSection, RunnerPoolsSection } from "./my-pools-section";
import { useMyPools } from "./use-my-pools";

const POLL_MS = 5000;
// These two 403s mean the card has no use for this caller: runners are off, or the credential is not a person's own session.
const NO_CARD_REASONS = [RUNNERS_DISABLED, "runner_session_required"];
const COPY_BUTTON = "gap-1.5 rounded-md border border-border bg-card px-2.5 py-1 text-xs text-foreground hover:bg-surface-2";
const withMono = makeMono(["wardyn runner claim"]);

type Status = "loading" | "ready" | "error" | "hidden";

function Fact({ label, reported, children }: { label: string; reported?: string; children: React.ReactNode }) {
  return (
    <div className="space-y-0.5">
      <div className="flex items-baseline justify-between gap-3">
        <h4 className="label-eyebrow">{label}</h4>
        {reported && <span className="text-meta text-muted-foreground">{reported}</span>}
      </div>
      <div className="text-body text-foreground">{children}</div>
    </div>
  );
}

const ROW = "mb-3 space-y-2 rounded-xl border border-border bg-surface-2 px-3.5 py-3";
type Pools = ReturnType<typeof useMyPools>;

function MachineSec({ r }: { r: RunnerView }) {
  return (
    <Sec label={RUNNER_V6.MACHINE} meta={RUNNER.REPORTED} data-machine>
      <MachineText machine={r.machine_name} />
      <ArchText arch={r.arch} />
    </Sec>
  );
}

/** A runner with no limits yet: what it needs, what the computer reports, what setup would suggest, and Set limits. */
function LimitsSetup({ r, onEdit }: { r: RunnerView; onEdit: () => void }) {
  const reported = r.usage.reported;
  const proposal = reported && proposeLimits(reported);
  return (
    <Sec label={MY_POOLS.LIMITS_TO_SET_UP} meta={MY_POOLS.REQUIRED} data-limits-setup>
      <p className="mb-1.5">{MY_POOLS.LIMITS_NEEDED}</p>
      {reported && (
        <p className="mb-1.5">
          {RUNNER_V6.REPORTS(amountsText(reported))} <span className="text-muted-foreground">{RUNNER.REPORTED}</span>
        </p>
      )}
      {proposal && <p className="mb-1.5 text-muted-foreground">{MY_POOLS.SUGGESTED(amountsText(proposal.total), proposal.runs_at_once, amountsText(proposal.per_run))}</p>}
      <Button variant="outline" size="sm" aria-label={MY_POOLS.SET_LIMITS_FOR(r.name)} onClick={onEdit}>
        {MY_POOLS.SET_LIMITS}
      </Button>
    </Sec>
  );
}

function ClaimedRow({ r, pools, onRevoke, onEditLimits }: { r: RunnerView; pools: Pools; onRevoke: () => void; onEditLimits: () => void }) {
  const offline = runnerChipKind(r) === "offline";
  const res = runnerRes(r.limits, r.usage);
  return (
    <li className={ROW}>
      <div className="flex flex-wrap items-center gap-2">
        <h4 className="text-sm font-medium text-foreground">{r.name}</h4>
        <RunnerChip runner={r} />
        <KindChip kind="self_hosted" />
        {offline && <span className="text-meta text-muted-foreground">{r.last_seen_at ? MY_RUNNER.LAST_SEEN(relativeTime(r.last_seen_at)) : RUNNER.NEVER_CONNECTED}</span>}
      </div>
      {offline && !r.last_seen_at && <OfflineNote neverConnected />}
      {r.version && <p className="text-meta text-muted-foreground">{`${RUNNER.VERSION} ${r.version}`}</p>}
      <div>
        <MachineSec r={r} />
        {res && r.limits ? (
          <Sec label={RES.RESOURCES} meta={MY_POOLS.LIMITS_YOU_SET} data-resources>
            <RunsLine max={r.limits.runs_at_once} active={r.usage.in_use.runs} />
            <ResourceTable view={res} />
            {offline && r.last_seen_at && <OfflineNote />}
            {r.usage.reported && (
              <Hint data-reported>
                {RUNNER_V6.REPORTS_YOUR(amountsText(r.usage.reported))} <span>{RUNNER.REPORTED}</span>
              </Hint>
            )}
            <div className="mt-3">
              <Button variant="outline" size="sm" aria-label={RUNNER_V6.EDIT_LIMITS_FOR(r.name)} onClick={onEditLimits}>
                {RUNNER_V6.EDIT_LIMITS}
              </Button>
            </div>
          </Sec>
        ) : (
          <LimitsSetup r={r} onEdit={onEditLimits} />
        )}
        <RunnerPoolsSection runner={r} pools={pools} />
      </div>
      <Fact label={RUNNER.ROOTS} reported={RUNNER.REPORTED}>
        {r.allowed_roots?.length ? (
          <>
            {r.allowed_roots.map((p) => (
              <div key={p}>
                <Mono>{p}</Mono>
              </div>
            ))}
            <p className="mt-1 text-meta text-muted-foreground">{MY_RUNNER.ROOTS_HINT}</p>
          </>
        ) : (
          // The server omits an empty list, so "none" is known only once the runner has reported.
          <p>{r.capacity ? RUNNER.ROOTS_NONE : RUNNER.NOT_REPORTED}</p>
        )}
      </Fact>
      <Fact label={RUNNER.POSTURE} reported={[RUNNER.REPORTED, r.posture_reported_at && RUNNER.REPORTED_AT(relativeTime(r.posture_reported_at))].filter(Boolean).join(" · ")}>
        {r.posture ? postureLines(r.posture).join(" · ") : RUNNER.NOT_REPORTED}
      </Fact>
      <Fact label={RUNNER.FINGERPRINT}>
        <Mono className="break-all">{r.key_fingerprint}</Mono>
      </Fact>
      <Button variant="outline" size="sm" onClick={onRevoke}>
        {RUNNERS.REVOKE}
      </Button>
    </li>
  );
}

function WaitingRow({ r, me, pools, onClaim, onNotMine, onEditLimits }: { r: RunnerView; me: string; pools: Pools; onClaim: () => void; onNotMine: () => void; onEditLimits: () => void }) {
  const byAdmin = !!r.minted_by_email || (!!r.minted_by && me !== "" && r.minted_by !== me);
  return (
    <li className={ROW}>
      <div className="flex flex-wrap items-center gap-2">
        <h4 className="text-sm font-medium text-foreground">{r.name}</h4>
        <RunnerChip runner={r} />
        <KindChip kind="self_hosted" />
      </div>
      <p className="text-meta text-muted-foreground">
        {MY_RUNNER.WAITING_LINE(relativeTime(r.created_at), r.claim_expires_at ? timeLeft(r.claim_expires_at) : "")}
      </p>
      <p className="text-meta text-muted-foreground">{byAdmin ? MY_RUNNER.SOURCE_ADMIN(r.minted_by_email || r.minted_by || "") : MY_RUNNER.SOURCE_SELF}</p>
      <div>
        <MachineSec r={r} />
        {r.limits ? (
          <Sec label={MY_POOLS.LIMITS} meta={MY_POOLS.SET} data-limits-setup>
            <p className="mb-1.5">{MY_POOLS.LIMITS_SUMMARY(amountsText(r.limits.total), amountsText(r.limits.per_run), r.limits.runs_at_once)}</p>
            <Button variant="outline" size="sm" aria-label={RUNNER_V6.EDIT_LIMITS_FOR(r.name)} onClick={onEditLimits}>
              {RUNNER_V6.EDIT_LIMITS}
            </Button>
          </Sec>
        ) : (
          <LimitsSetup r={r} onEdit={onEditLimits} />
        )}
      </div>
      <Fact label={RUNNER.FINGERPRINT}>
        <Mono className="break-all">{r.key_fingerprint}</Mono>
      </Fact>
      <div className="flex gap-2">
        <Button variant="outline" size="sm" data-claim onClick={onClaim}>
          {MY_RUNNER.CLAIM}
        </Button>
        <Button variant="outline" size="sm" onClick={onNotMine}>
          {MY_RUNNER.NOT_MINE}
        </Button>
      </div>
      <RunnerPoolsSection runner={r} pools={pools} />
    </li>
  );
}

function RevokedNotice({ r }: { r: RunnerView }) {
  return (
    <li className="mb-3 space-y-1 rounded-xl border border-border bg-surface-2 px-3.5 py-3">
      <div className="flex flex-wrap items-center gap-2">
        <h4 className="text-sm font-medium text-foreground">{r.name}</h4>
        <RunnerChip runner={r} />
      </div>
      <p className="text-body text-muted-foreground" title={r.revoked_at ? absoluteTime(r.revoked_at) : undefined}>
        {MY_RUNNER.REVOKED_BY(r.name, r.revoked_by_email || r.revoked_by || "", r.revoked_at ? shortDate(r.revoked_at) : "")}
      </p>
    </li>
  );
}

function TokenBlock({ token, expired, onDone }: { token: RunnerRegistrationToken; expired: boolean; onDone: () => void }) {
  const eyebrow = React.useRef<HTMLHeadingElement | null>(null);
  React.useEffect(() => eyebrow.current?.focus(), []);
  if (expired) return <p className="text-body text-muted-foreground">{MY_RUNNER.TOKEN_EXPIRED}</p>;
  const command = registerCommand(token, "~/wardyn-runner-token");
  const expires = new Date(token.expires_at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  return (
    <section className="space-y-3 rounded-lg border border-border p-3">
      <div className="flex items-baseline justify-between gap-3">
        <h4 ref={eyebrow} tabIndex={-1} className="label-eyebrow outline-none">
          {RUNNER.TOKEN_LABEL}
        </h4>
        <span className="text-meta text-muted-foreground">{MY_RUNNER.TOKEN_SHOWN_ONCE}</span>
      </div>
      <div className="flex items-center justify-between gap-2 rounded-lg border border-border bg-surface-2/60 p-3">
        <code className="min-w-0 break-all font-mono text-xs">{token.token}</code>
        <CopyButton text={token.token ?? ""} label={RUNNER.COPY_TOKEN} className={COPY_BUTTON}>
          {RUNNER.COPY_TOKEN}
        </CopyButton>
      </div>
      <p className="text-meta text-muted-foreground">{MY_RUNNER.TOKEN_EXPIRES(expires)}</p>
      <p className="text-body text-muted-foreground">{MY_RUNNER.STEP_SAVE_RUN}</p>
      <div className="flex items-center justify-between gap-2 rounded-lg border border-border bg-surface-2/60 p-3">
        <code className="min-w-0 break-all font-mono text-xs">{command}</code>
        <CopyButton text={command} label={RUNNER.COPY_COMMAND} className={COPY_BUTTON}>
          {RUNNER.COPY_COMMAND}
        </CopyButton>
      </div>
      <StartRunnerSteps token={token} />
      <p className="text-body text-foreground">{MY_POOLS.REGISTER_ONE_PER_COMPUTER}</p>
      <p className="text-xs text-muted-foreground">{MY_POOLS.REGISTER_ASKS_LIMITS}</p>
      <p className="text-body text-muted-foreground">{withMono(MY_RUNNER.STEP_CLAIM)}</p>
      <div className="flex justify-end">
        <Button variant="ghost" size="sm" onClick={onDone}>
          {RUNNER.DONE}
        </Button>
      </div>
    </section>
  );
}

export function MyRunnerCard({ onHidden }: { onHidden?: (reason: string) => void } = {}) {
  const me = usePrincipal();
  const location = useLocation();
  const sectionRef = React.useRef<HTMLElement | null>(null);
  const [open, setOpen] = React.useState(false);
  const [status, setStatus] = React.useState<Status>("loading");
  const [rows, setRows] = React.useState<RunnerView[]>([]);
  const [token, setToken] = React.useState<RunnerRegistrationToken | null>(null);
  const [now, setNow] = React.useState(() => Date.now());
  const [registering, setRegistering] = React.useState(false);
  const [registerError, setRegisterError] = React.useState("");
  const [note, setNote] = React.useState<{ id: string; text: string } | null>(null);
  const [claiming, setClaiming] = React.useState<RunnerView | null>(null);
  const [revoking, setRevoking] = React.useState<{ runner: RunnerView; notMine: boolean } | null>(null);
  const [confirming, setConfirming] = React.useState(false);
  const [limitsFor, setLimitsFor] = React.useState<RunnerView | null>(null);
  const idsAtMint = React.useRef(new Set<string>());
  const seq = React.useRef(0);
  // Read through a ref: `read` is built once, and the parent may pass a new function each render.
  const hiddenRef = React.useRef(onHidden);
  hiddenRef.current = onHidden;

  const read = React.useCallback((silent: boolean) => {
    const mine = ++seq.current;
    runnersApi
      .listMine()
      .then((list) => {
        if (mine !== seq.current) return;
        setRows(list);
        setStatus("ready");
      })
      .catch((e) => {
        if (mine !== seq.current) return;
        if (e instanceof HttpError && e.status === 403 && NO_CARD_REASONS.includes(e.reason)) {
          setStatus("hidden");
          hiddenRef.current?.(e.reason);
        } else if (!silent) setStatus("error");
      });
  }, []);
  // A read that was in flight when the card went away must not set state.
  const abandon = React.useCallback(() => {
    seq.current++;
  }, []);
  React.useEffect(() => {
    read(false);
    return abandon;
  }, [read, abandon]);

  const expired = token !== null && now >= Date.parse(token.expires_at);
  const live = rows.filter((r) => r.state !== "revoked");
  const waiting = live.filter((r) => r.state === "unclaimed");
  // The card polls only while a token shows or a runner waits, and never announces a poll.
  const polling = status === "ready" && ((token !== null && !expired) || waiting.length > 0);
  React.useEffect(() => {
    if (!polling) return;
    const tick = () => {
      setNow(Date.now());
      if (document.visibilityState === "visible") read(true);
    };
    const id = setInterval(tick, POLL_MS);
    document.addEventListener("visibilitychange", tick);
    return () => {
      clearInterval(id);
      document.removeEventListener("visibilitychange", tick);
    };
  }, [polling, read]);
  // The token block turns into its expiry sentence at the minute, even between polls.
  React.useEffect(() => {
    if (!token) return;
    const id = setTimeout(() => setNow(Date.now()), Math.max(0, Date.parse(token.expires_at) - Date.now()) + 50);
    return () => clearTimeout(id);
  }, [token]);

  // A runner that was not there when the token was made has registered with it: the token is spent.
  React.useEffect(() => {
    if (!token) return;
    const fresh = waiting.find((r) => !idsAtMint.current.has(r.id));
    if (!fresh) return;
    setToken(null);
    setNote({ id: fresh.id, text: MY_RUNNER.REGISTERED_NOW(fresh.name) });
  }, [rows, token]); // eslint-disable-line react-hooks/exhaustive-deps -- `waiting` derives from rows
  React.useEffect(() => {
    if (note && !waiting.some((r) => r.id === note.id)) setNote(null);
  }, [rows, note]); // eslint-disable-line react-hooks/exhaustive-deps -- `waiting` derives from rows

  const available = status === "ready" || status === "error";
  const pools = useMyPools(status === "ready");
  // The #my-runner deep link opens and focuses the card once per navigation, never on a data refresh.
  React.useEffect(() => {
    if (location.hash === "#my-runner" && available) {
      setOpen(true);
      sectionRef.current?.focus({ focusVisible: true } as FocusOptions);
    }
  }, [location.key, available]); // eslint-disable-line react-hooks/exhaustive-deps -- location.key, not the hash

  if (status === "hidden") return null;

  const register = async () => {
    setRegistering(true);
    setRegisterError("");
    try {
      const t = await runnersApi.mintMine();
      idsAtMint.current = new Set(rows.map((r) => r.id));
      setNow(Date.now());
      setToken(t);
    } catch (e) {
      setRegisterError(getErrorMessage(e));
    } finally {
      setRegistering(false);
    }
  };
  const done = () => {
    setToken(null);
    requestAnimationFrame(() => sectionRef.current?.querySelector<HTMLElement>("[data-claim], [data-register]")?.focus());
  };

  let summary: React.ReactNode;
  if (status === "ready") {
    if (waiting.length > 0) summary = MY_RUNNER.SUMMARY_WAITING(waiting.length);
    else if (live.length === 0) summary = MY_RUNNER.SUMMARY_NONE;
    else if (live.length === 1) summary = MY_RUNNER.SUMMARY_ONE(live[0].name, CHIP_WORD[runnerChipKind(live[0])]);
    else summary = MY_RUNNER.SUMMARY_MANY(live.length, live.filter((r) => runnerChipKind(r) === "online").length);
  }

  const sorted = sortRunners(rows);
  const showToken = token !== null;
  const registerBlock = (!showToken || expired) && (
    <div className="space-y-1.5">
      <Button variant="outline" data-register disabled={registering} onClick={() => void register()}>
        {registering && <Loader2 className="size-3.5 animate-spin" />}
        {MY_RUNNER.REGISTER}
      </Button>
      {registerError && (
        <p role="alert" className="text-body text-danger">
          {registerError}
        </p>
      )}
    </div>
  );
  return (
    <>
      <CollapsibleCard
        title={MY_RUNNER.TITLE}
        summary={summary}
        id="my-runner"
        ref={sectionRef}
        tabIndex={-1}
        open={open}
        onOpenChange={setOpen}
        className="outline-none focus-visible:border-ring focus-visible:ring-ring focus-visible:ring-[3px]"
      >
        <div className="space-y-3">
          <p className="text-body text-muted-foreground">{MY_RUNNER.LEDE}</p>
          {status === "error" ? (
            <div role="status" className="flex flex-wrap items-center gap-2">
              <p className="text-body text-muted-foreground">{MY_RUNNER.FETCH_FAILED}</p>
              <Button size="sm" variant="outline" onClick={() => read(false)}>
                {STATES.RETRY}
              </Button>
            </div>
          ) : (
            <>
              {live.length === 0 && (
                <>
                  <p className="text-body text-foreground">{MY_RUNNER.NO_RUNNER}</p>
                  <p className="text-body text-muted-foreground">{MY_POOLS.NO_RUNNER_NEXT}</p>
                </>
              )}
              {showToken && <TokenBlock token={token} expired={expired} onDone={done} />}
              {note && (
                <p role="status" className="text-body text-foreground">
                  {note.text}
                </p>
              )}
              {sorted.length > 0 && (
                <ul>
                  {sorted.map((r) =>
                    r.state === "revoked" ? (
                      <RevokedNotice key={r.id} r={r} />
                    ) : r.state === "unclaimed" ? (
                      <WaitingRow
                        key={r.id}
                        r={r}
                        me={me}
                        pools={pools}
                        onEditLimits={() => setLimitsFor(r)}
                        onClaim={() => setClaiming(r)}
                        onNotMine={() => {
                          setRevoking({ runner: r, notMine: true });
                          setConfirming(true);
                        }}
                      />
                    ) : (
                      <ClaimedRow
                        key={r.id}
                        r={r}
                        pools={pools}
                        onEditLimits={() => setLimitsFor(r)}
                        onRevoke={() => {
                          setRevoking({ runner: r, notMine: false });
                          setConfirming(true);
                        }}
                      />
                    ),
                  )}
                </ul>
              )}
              {live.length === 0 && registerBlock}
              <MyPoolsSection pools={pools} runners={live} />
              {live.length > 0 && registerBlock}
            </>
          )}
        </div>
      </CollapsibleCard>
      <ClaimRunnerDialog
        runner={claiming}
        onOpenChange={(o) => !o && setClaiming(null)}
        onClaimed={() => {
          read(true);
          pools.reload();
          requestAnimationFrame(() => sectionRef.current?.focus());
        }}
      />
      <LimitsDialog
        target={limitsFor ? { name: limitsFor.name, limits: limitsFor.limits, reported: limitsFor.usage.reported, running: limitsFor.usage.in_use.runs, self: true } : null}
        onOpenChange={(o) => !o && setLimitsFor(null)}
        onSave={(limits) => runnersApi.updateMine(limitsFor?.id ?? "", limits)}
        onSaved={() => {
          read(true);
          pools.reload();
        }}
      />
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title={RUNNERS.REVOKE_TITLE(revoking?.runner.name ?? "")}
        body={
          revoking?.notMine
            ? [MY_RUNNER.NOT_MINE_BODY]
            : [MY_RUNNER.REVOKE_OWN_BODY, ...((revoking?.runner.usage.in_use.runs ?? 0) > 0 ? [RUNNER.REVOKE_RUNS(revoking?.runner.usage.in_use.runs ?? 0)] : [])]
        }
        confirmLabel={RUNNER.REVOKE_CONFIRM}
        onConfirm={() => runnersApi.revokeMine(revoking?.runner.id ?? "", revoking?.notMine)}
        onDone={() => {
          toast.success(RUNNER.REVOKED_TOAST(revoking?.runner.name ?? ""));
          read(true);
          requestAnimationFrame(() => sectionRef.current?.focus());
        }}
        onRaced={() => read(true)}
      />
    </>
  );
}
