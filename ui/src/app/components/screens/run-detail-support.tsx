/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Support sessions on the run page (approved board SC): the helper's Request help button and dialog, and the
// one banner every party reads (the owner's request waiting, the active session with End now, the line for how
// help ended). Nothing renders, and nothing is fetched, unless the setup answer carries features.support_sessions.
import * as React from "react";
import { toast } from "sonner";
import type { RunDetail } from "../../lib/types";
import { HttpError } from "../../lib/api/core";
import { support, type SupportDoor, type SupportGrantView, type SupportStatus } from "../../lib/api/support";
import { usePoll } from "../../lib/use-poll";
import { getErrorMessage } from "../../lib/format";
import { isBackgroundRun, isServiceRun } from "../../lib/run-entry";
import { HARNESS_LOGIN_TASK } from "./run-detail/sign-in-run";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
import { Label } from "../ui/label";
import { Textarea } from "../ui/textarea";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../ui/dialog";
import { useOperator, usePrincipal, useSecurityOperator } from "../wardyn/operator-context";
import { useShellSetupStatus } from "../wardyn/model-access-context";
import {
  SUPPORT_BANNER,
  SUPPORT_CONSENT,
  SUPPORT_DOOR,
  SUPPORT_DOORS,
  SUPPORT_REASON_MAX,
  SUPPORT_REQUEST,
  supportEndLine,
  type SupportViewer,
} from "../wardyn/copy/support";
import { SupportConsentDialog, utcClock } from "./run-detail-support-consent";

const POLL_MS = 10_000;

// The run tasks the server sets itself and never lets a helper into: reservedRunTasks
// (internal/api/runs_create_validate.go), which supportRunIneligible (internal/api/support_entry.go) refuses.
const RESERVED_RUN_TASKS: readonly string[] = [HARNESS_LOGIN_TASK, "workspace record", "workspace verify"];

// The principal of the deployment's admin token (adminTokenPrincipal, internal/api/runs_policy.go): no person,
// so the server refuses its request (supportPerson, internal/api/support_entry.go).
const ADMIN_TOKEN_PRINCIPAL = "admin-token";

const closedKey = (grant: string) => `wardyn.support.closed.${grant}`;
const wasClosed = (grant: string): boolean => {
  try {
    return window.localStorage.getItem(closedKey(grant)) === "1";
  } catch {
    return false;
  }
};

export interface RunSupport {
  /** The setup answer carries support_sessions: the console draws anything at all. */
  on: boolean;
  viewer: SupportViewer | null;
  status: SupportStatus | null;
  open: SupportGrantView | null;
  /** The signed-in person helps through an accepted grant that opens the terminal door. */
  helperTerminal: boolean;
  /** The doors of the open grant the signed-in person helps through; null for anyone else. The same "helping" state as helperTerminal. */
  helperDoors: SupportDoor[] | null;
  refresh: () => Promise<void>;
  /** Show a grant the server just confirmed as ended, and drop any status read that started before it. */
  applyEnded: (ended: SupportGrantView) => void;
}

/**
 * The run's grant state for whoever is looking: read from GET /runs/{id}/support (owner, helper and security
 * admin may read it; anyone else is told no and sees nothing), and read again every few seconds while the run is
 * live or a grant is open, so a request, an acceptance and an end show up without a reload.
 */
export function useRunSupport(run: RunDetail | null): RunSupport {
  const { status: setup } = useShellSetupStatus();
  const on = setup?.features?.support_sessions === true;
  const principal = usePrincipal();
  const operator = useOperator();
  const securityOperator = useSecurityOperator();
  const isOwner = !!run?.created_by && principal === run.created_by;
  const mayRead = on && !!run && (isOwner || operator || securityOperator);
  const [status, setStatus] = React.useState<SupportStatus | null>(null);
  const runId = run?.id;
  // Each status read takes the next number; an applied change (a read's answer or a confirmed end) records its
  // number, and an answer to a read numbered below it started earlier and is dropped.
  const readSeq = React.useRef(0);
  const appliedSeq = React.useRef(0);
  const refresh = React.useCallback(async () => {
    if (!mayRead || !runId) return;
    const n = ++readSeq.current;
    try {
      const next = await support.status(runId);
      if (n < appliedSeq.current) return;
      appliedSeq.current = n;
      setStatus(next);
    } catch (e) {
      if (n < appliedSeq.current) return;
      // Not a party to this run's help (a 404, or a not-found reason): nothing to show. Any other failure keeps
      // the last answer, so one failed read neither shuts a helper's terminal nor drops the owner's banner.
      if (e instanceof HttpError && (e.status === 404 || e.reason.endsWith("not_found"))) {
        appliedSeq.current = n;
        setStatus(null);
      }
    }
  }, [mayRead, runId]);
  const applyEnded = React.useCallback((ended: SupportGrantView) => {
    appliedSeq.current = ++readSeq.current;
    // The end closes the grant it names: a different grant that is open on the run now stays shown.
    setStatus((prev) => ({ ...prev, open: prev?.open?.grant === ended.grant ? null : (prev?.open ?? null), ended }));
  }, []);
  React.useEffect(() => {
    void refresh();
  }, [refresh]);
  const open = status?.open ?? null;
  usePoll(refresh, POLL_MS, !mayRead || !(run?.state === "RUNNING" || open));

  const grant = open ?? status?.ended ?? null;
  let viewer: SupportViewer | null = null;
  if (mayRead) {
    if (isOwner) viewer = "owner";
    else if (grant && principal !== "" && principal === grant.helper) viewer = "helper";
    else if (securityOperator) viewer = "security";
  }
  const helping = viewer === "helper" && open?.state === "active";
  const helperDoors = helping ? (open?.doors ?? []) : null;
  const helperTerminal = !!helperDoors?.includes("terminal");
  return { on: mayRead, viewer, status, open, helperTerminal, helperDoors, refresh, applyEnded };
}

/** The door chips of a banner, in the board's order. */
function DoorChips({ doors }: { doors: SupportDoor[] }) {
  return (
    <div className="flex flex-wrap gap-1.5">
      {SUPPORT_DOORS.filter((d) => doors.includes(d)).map((d) => (
        <span key={d} className="rounded-md border border-border bg-muted/40 px-2 py-0.5 text-xs text-foreground">
          {SUPPORT_DOOR[d].name}
        </span>
      ))}
    </div>
  );
}

function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="label-eyebrow">{label}</dt>
      <dd className="mt-1 text-sm text-foreground">{children}</dd>
    </div>
  );
}

const BANNER = "mb-0 flex flex-wrap items-start justify-between gap-3 rounded-lg border border-info/30 bg-info-subtle px-4 py-3";

/** The run page's support strip: the Request help button, or the banner for the grant in front of the viewer. */
export function RunSupportStrip({ run, sup }: { run: RunDetail; sup: RunSupport }) {
  const { viewer, open, status, refresh, applyEnded } = sup;
  const operator = useOperator();
  const principal = usePrincipal();
  const [requesting, setRequesting] = React.useState(false);
  const [reviewing, setReviewing] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [closed, setClosed] = React.useState<string | null>(null);
  if (!sup.on) return null;

  const end = async (grant: SupportGrantView) => {
    setBusy(true);
    try {
      applyEnded(await support.end(run.id, grant.grant));
    } catch (e) {
      toast.error(getErrorMessage(e));
    } finally {
      setBusy(false);
      void refresh();
    }
  };

  // Help may be asked for on a live run that belongs to a person, by a super admin signed in as a person who is
  // not that person: the server's supportRunIneligible and supportCaller, less local mode, which it answers.
  const mayAsk =
    operator &&
    principal !== "" &&
    principal !== ADMIN_TOKEN_PRINCIPAL &&
    viewer !== "owner" &&
    run.state === "RUNNING" &&
    !open &&
    !run.lost_at &&
    !run.operator_owned &&
    !isServiceRun(run) &&
    !isBackgroundRun(run) &&
    !RESERVED_RUN_TASKS.includes(run.task);

  let body: React.ReactNode = null;
  if (open && viewer) {
    body = open.state === "requested" ? (
      <RequestedBanner run={run} grant={open} viewer={viewer} busy={busy} onReview={() => setReviewing(true)} onCancel={() => void end(open)} />
    ) : (
      <ActiveBanner grant={open} viewer={viewer} busy={busy} onEnd={() => void end(open)} />
    );
  } else if (status?.ended && viewer && !wasClosed(status.ended.grant) && closed !== status.ended.grant) {
    const ended = status.ended;
    const line = supportEndLine(ended, viewer);
    if (line) {
      body = (
        <div className={BANNER} role="status" data-testid="support-ended">
          <p className="min-w-0 flex-1 text-sm text-info">{line}</p>
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              try {
                window.localStorage.setItem(closedKey(ended.grant), "1");
              } catch {
                /* kept for this view only */
              }
              setClosed(ended.grant);
            }}
          >
            {SUPPORT_BANNER.CLOSE}
          </Button>
        </div>
      );
    }
  }
  if (!body && !mayAsk) return null;

  return (
    <div className="shrink-0 space-y-2 px-4 pt-3" data-testid="support-strip">
      {body}
      {mayAsk && (
        <div className="flex justify-end">
          <Button variant="outline" size="sm" onClick={() => setRequesting(true)} data-testid="support-request-button">
            {SUPPORT_REQUEST.TITLE}
          </Button>
        </div>
      )}
      {requesting && (
        <RequestHelpDialog
          runId={run.id}
          offered={status?.doors_offered ?? SUPPORT_DOORS.slice()}
          onClose={() => setRequesting(false)}
          onSent={() => {
            setRequesting(false);
            void refresh();
          }}
        />
      )}
      {reviewing && open && (
        <SupportConsentDialog
          run={run}
          grant={open}
          recordedDoors={open.recorded_doors ?? status?.recorded_doors ?? []}
          onClose={() => setReviewing(false)}
          onAnswered={() => {
            setReviewing(false);
            void refresh();
          }}
        />
      )}
    </div>
  );
}

function RequestedBanner({ run, grant, viewer, busy, onReview, onCancel }: { run: RunDetail; grant: SupportGrantView; viewer: SupportViewer; busy: boolean; onReview: () => void; onCancel: () => void }) {
  return (
    <div className={BANNER} role="status" data-testid="support-requested">
      <div className="min-w-0 flex-1">
        {viewer === "helper" ? (
          <p className="text-base font-medium text-foreground">{SUPPORT_BANNER.WAITING(run.created_by)}</p>
        ) : (
          <p className="text-base font-medium text-foreground">{SUPPORT_BANNER.ASKED(grant.helper)}</p>
        )}
        <p className="mt-1 text-sm text-muted-foreground">{SUPPORT_CONSENT.REQUESTED(utcClock(grant.requested_at))}</p>
      </div>
      {viewer === "owner" && (
        <Button size="sm" onClick={onReview} data-testid="support-review">
          {SUPPORT_BANNER.REVIEW}
        </Button>
      )}
      {viewer === "helper" && (
        <Button variant="outline" size="sm" disabled={busy} onClick={onCancel} data-testid="support-cancel-request">
          {SUPPORT_BANNER.CANCEL_REQUEST}
        </Button>
      )}
      {viewer === "security" && (
        <Button variant="outline" size="sm" disabled={busy} onClick={onCancel} className="text-danger" data-testid="support-end-now">
          {SUPPORT_BANNER.END_NOW}
        </Button>
      )}
    </div>
  );
}

function ActiveBanner({ grant, viewer, busy, onEnd }: { grant: SupportGrantView; viewer: SupportViewer; busy: boolean; onEnd: () => void }) {
  return (
    <div className={BANNER} role="status" data-testid="support-active">
      <div className="min-w-0 flex-1 space-y-3">
        <p className="text-base font-medium text-foreground">
          {viewer === "helper" ? SUPPORT_BANNER.ACTIVE_HELPER : SUPPORT_BANNER.ACTIVE_OTHERS(grant.helper)}
        </p>
        <dl className="grid grid-cols-2 gap-x-6 gap-y-3 sm:grid-cols-4">
          <Fact label={SUPPORT_BANNER.HELPER}>{grant.helper}</Fact>
          <Fact label={SUPPORT_BANNER.DOORS}>
            <DoorChips doors={grant.doors ?? []} />
          </Fact>
          <Fact label={SUPPORT_BANNER.UNTIL}>{utcClock(grant.expires_at)}</Fact>
          <Fact label={SUPPORT_BANNER.LAST_SEEN}>{utcClock(grant.last_seen_at) || "—"}</Fact>
        </dl>
      </div>
      <Button variant="outline" size="sm" disabled={busy} onClick={onEnd} className="text-danger" data-testid="support-end-now">
        {SUPPORT_BANNER.END_NOW}
      </Button>
    </div>
  );
}

const runes = (s: string): number => [...s].length;

/** The helper's dialog: the doors they want and why. No minutes: the owner picks those. */
function RequestHelpDialog({ runId, offered, onClose, onSent }: { runId: string; offered: SupportDoor[]; onClose: () => void; onSent: () => void }) {
  const [doors, setDoors] = React.useState<SupportDoor[]>([]);
  const [reason, setReason] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const n = runes(reason.trim());
  const tooLong = n > SUPPORT_REASON_MAX;
  const pathMode = SUPPORT_DOORS.some((d) => !offered.includes(d));
  const canSend = doors.length > 0 && n >= 1 && !tooLong && !busy;
  const send = async () => {
    setBusy(true);
    setError("");
    try {
      await support.request(runId, SUPPORT_DOORS.filter((d) => doors.includes(d)), reason.trim());
      onSent();
    } catch (e) {
      setError(e instanceof HttpError ? e.message : getErrorMessage(e));
      setBusy(false);
    }
  };
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-lg" data-testid="support-request-dialog">
        <DialogHeader>
          <DialogTitle>{SUPPORT_REQUEST.TITLE}</DialogTitle>
          <DialogDescription>{SUPPORT_REQUEST.LEDE}</DialogDescription>
        </DialogHeader>
        <fieldset className="space-y-3">
          <legend className="mb-2 text-sm font-medium text-foreground">{SUPPORT_REQUEST.DOORS}</legend>
          {SUPPORT_DOORS.filter((d) => offered.includes(d)).map((d) => (
            <div key={d} className="flex items-start gap-3">
              <Checkbox
                id={`req-door-${d}`}
                checked={doors.includes(d)}
                onCheckedChange={(on) => setDoors((cur) => (on === true ? [...cur, d] : cur.filter((x) => x !== d)))}
                className="mt-0.5"
              />
              <Label htmlFor={`req-door-${d}`} className="flex flex-col items-start gap-0.5">
                <span className="text-sm font-medium text-foreground">{SUPPORT_DOOR[d].name}</span>
                <span className="text-xs font-normal text-muted-foreground">{SUPPORT_DOOR[d].help}</span>
              </Label>
            </div>
          ))}
          {pathMode && <p className="text-xs text-muted-foreground">{SUPPORT_REQUEST.PATH_MODE}</p>}
        </fieldset>
        <div className="space-y-1.5">
          <Label htmlFor="support-reason" className="text-sm font-medium text-foreground">
            {SUPPORT_REQUEST.REASON}
          </Label>
          <Textarea id="support-reason" rows={4} value={reason} onChange={(e) => setReason(e.target.value)} aria-invalid={tooLong} aria-describedby="support-reason-hint" />
          <div className="flex items-start justify-between gap-3">
            <p id="support-reason-hint" className={tooLong ? "text-xs text-danger" : "text-xs text-muted-foreground"}>
              {tooLong ? SUPPORT_REQUEST.TOO_LONG : SUPPORT_REQUEST.REASON_HINT}
            </p>
            <span className="shrink-0 font-mono text-xs text-muted-foreground" data-testid="support-reason-count">
              {SUPPORT_REQUEST.COUNT(n)}
            </span>
          </div>
        </div>
        {error && (
          <p role="alert" className="text-sm text-danger">
            {error}
          </p>
        )}
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            {SUPPORT_REQUEST.CANCEL}
          </Button>
          <Button disabled={!canSend} onClick={() => void send()}>
            {SUPPORT_REQUEST.SEND}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
