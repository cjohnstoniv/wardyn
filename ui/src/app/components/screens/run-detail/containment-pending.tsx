/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A local run its Kubernetes runner holds as containment pending: the egress bound
// while it holds, "termination unknown" once the bound has passed, the name-reuse and association labels,
// and for security operators the node record form with the machines it must name.
// Everything shown is the server's: the bound, whether it passed, and whether a record needs two people.

import * as React from "react";
import { toast } from "sonner";
import { runners as runnersApi } from "../../../lib/api/runners";
import { PendingChangeError } from "../../../lib/api/core";
import { absoluteTime, getErrorMessage } from "../../../lib/format";
import type { ApprovalRequest, RunDetail } from "../../../lib/types";
import type {
  NodeRecordAttestation,
  NodeRecordSource,
  NodeTuple,
  RunContainment,
  RunNodeRecord,
} from "../../../lib/types/run-containment";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { CONTAINMENT as C } from "../../wardyn/copy/run-containment";
import { useSecurityOperator } from "../../wardyn/operator-context";
import { RunLifetimeBanner } from "./run-lifetime-banner";

const SELECT_CLASS =
  "h-9 w-full max-w-[260px] rounded-md border border-border-strong bg-input-background px-3 text-sm outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring";

/** Reads the run's containment; null until read, or when the run is not on a registered runner. */
export function useRunContainment(run: RunDetail): [RunContainment | null, () => void] {
  const [state, setState] = React.useState<RunContainment | null>(null);
  const [tick, setTick] = React.useState(0);
  const local = run.placement === "local";
  React.useEffect(() => {
    if (!local) return;
    let live = true;
    runnersApi
      .runContainment(run.id)
      .then((c) => live && setState(c))
      .catch(() => live && setState(null));
    return () => {
      live = false;
    };
  }, [local, run.id, run.updated_at, tick]);
  return [local ? state : null, () => setTick((t) => t + 1)];
}

/** The lifetime banner, told whether the run is held as containment pending, and the containment panel under it. */
export function RunLifetimeWithContainment({ run, pending, onChanged }: { run: RunDetail; pending: ApprovalRequest[]; onChanged: () => void }) {
  const [containment, reload] = useRunContainment(run);
  return (
    <>
      <RunLifetimeBanner run={run} pending={pending} onChanged={onChanged} containmentPending={!!containment?.pending} />
      <ContainmentPending runId={run.id} containment={containment} onChanged={reload} />
    </>
  );
}

export function ContainmentPending({ runId, containment, onChanged }: { runId: string; containment: RunContainment | null; onChanged: () => void }) {
  const securityOperator = useSecurityOperator();
  const pending = containment?.pending;
  const records = containment?.records ?? [];
  if (!pending && records.length === 0) return null;
  return (
    <div data-testid="run-containment" className="mx-4 mt-2 shrink-0 rounded-md border border-danger/40 bg-danger-subtle px-3 py-2.5 text-xs">
      {pending && (
        <>
          <h3 data-testid="run-containment-line" className="text-sm font-medium text-foreground">
            {pending.bound_passed ? C.PASSED_TITLE : C.pendingTitle(absoluteTime(pending.bound_until))}
          </h3>
          <p className="mt-1 text-muted-foreground">{pending.bound_passed ? C.PASSED_BODY : C.PENDING_BODY}</p>
          {pending.name_reused && (
            <p data-testid="run-containment-name-reused" className="mt-1.5">
              <span className="font-medium text-foreground">{C.NAME_REUSED}.</span>{" "}
              <span className="text-muted-foreground">{C.NAME_REUSED_BODY}</span>
            </p>
          )}
          {pending.no_causal_association && (
            <p data-testid="run-containment-no-causal" className="mt-1.5">
              <span className="font-medium text-foreground">{C.NO_CAUSAL}.</span>{" "}
              <span className="text-muted-foreground">{C.NO_CAUSAL_BODY}</span>
            </p>
          )}
          {securityOperator && pending.tuple && (
            <NodeRecordForm
              runId={runId}
              tuple={pending.tuple}
              since={pending.since}
              nameReused={pending.name_reused}
              noCausal={pending.no_causal_association}
              twoPeople={pending.two_people}
              onSent={onChanged}
            />
          )}
        </>
      )}
      {records.length > 0 && <NodeRecords records={records} />}
    </div>
  );
}

function NodeRecords({ records }: { records: RunNodeRecord[] }) {
  return (
    <div className="mt-2.5">
      <h4 className="font-medium text-foreground">{C.RECORDS_TITLE}</h4>
      <ul className="mt-1 space-y-0.5 text-muted-foreground">
        {records.map((r) => (
          <li key={r.id} data-testid="run-node-record">
            {r.attestation === "node_powered_off" ? C.ATTEST_POWERED_OFF : C.ATTEST_NO_PRIOR} · {absoluteTime(r.created_at)} ·{" "}
            {r.state === "accepted" ? C.STATE_ACCEPTED : r.state === "refused" ? C.stateRefused(r.reason ?? "") : C.STATE_SENT}
          </li>
        ))}
      </ul>
    </div>
  );
}

// The machines the record must name, as the runner recorded them. An absent value was never recorded and
// is left out, never filled in.
function Machines({ tuple, since }: { tuple: NodeTuple; since: string }) {
  return (
    <div className="mt-2">
      <h5 className="font-medium text-foreground">{C.MACHINES_LABEL}</h5>
      <ul data-testid="run-node-record-machines" className="mt-1 space-y-1 font-mono text-[11px] text-muted-foreground">
        {tuple.pods.map((p) => (
          <li key={p.uid}>
            <div>{C.pod(p.component, p.uid)}</div>
            <div>{p.node_name ? C.node(p.node_name) : C.NODE_NONE}</div>
            {p.causal_uid && <div>{C.holder(C.HOLDER_CAUSAL, p.causal_uid)}</div>}
            {p.post_bind && (
              <div>
                {C.holder(C.HOLDER_POST_BIND, p.post_bind.uid)} ({C.machine(p.post_bind.machine_id, p.post_bind.system_uuid)})
              </div>
            )}
            {p.before_holder_uid && <div>{C.holder(C.HOLDER_BEFORE, p.before_holder_uid)}</div>}
            {p.closing_holder_uid && <div>{C.holder(C.HOLDER_CLOSING, p.closing_holder_uid)}</div>}
          </li>
        ))}
      </ul>
      {tuple.reuses && tuple.reuses.length > 0 && (
        <>
          <h5 className="mt-1.5 font-medium text-foreground">{C.REUSES_LABEL}</h5>
          <ul data-testid="run-node-record-reuses" className="mt-1 font-mono text-[11px] text-muted-foreground">
            {tuple.reuses.map((r) => (
              <li key={`${r.uid}/${r.machine_id}/${r.system_uuid}`}>{C.reuse(r.name, r.uid, r.machine_id, r.system_uuid)}</li>
            ))}
          </ul>
        </>
      )}
      <p className="mt-1.5 text-muted-foreground">{C.markedAt(absoluteTime(since))}</p>
    </div>
  );
}

function NodeRecordForm({
  runId,
  tuple,
  since,
  nameReused,
  noCausal,
  twoPeople,
  onSent,
}: {
  runId: string;
  tuple: NodeTuple;
  since: string;
  nameReused: boolean;
  noCausal: boolean;
  twoPeople: boolean;
  onSent: () => void;
}) {
  // "No prior holder" is offered only where the server would take it: the no-association path, with no
  // recorded name reuse. The runner still decides it against its own acceptance list.
  const noPriorOffered = noCausal && !nameReused;
  const [attestation, setAttestation] = React.useState<NodeRecordAttestation>("node_powered_off");
  const [source, setSource] = React.useState<NodeRecordSource>("provider_console");
  const [sourceId, setSourceId] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [held, setHeld] = React.useState(false);
  const send = async () => {
    setBusy(true);
    try {
      await runnersApi.recordNode(runId, { attestation, source, source_id: sourceId.trim(), tuple });
      toast.success(C.SENT);
      onSent();
    } catch (err) {
      if (err instanceof PendingChangeError) setHeld(true);
      else toast.error(C.SEND_FAILED, { description: getErrorMessage(err) });
    } finally {
      setBusy(false);
    }
  };
  return (
    <form
      data-testid="run-node-record-form"
      className="mt-3 border-t border-danger/30 pt-2.5"
      onSubmit={(e) => {
        e.preventDefault();
        void send();
      }}
    >
      <h4 className="text-sm font-medium text-foreground">{C.FORM_TITLE}</h4>
      <p className="mt-1 text-muted-foreground">{C.FORM_LEAD}</p>
      <ul className="mt-1 list-disc pl-4 text-foreground">
        <li>{C.WARN_DELETED_NODE}</li>
        <li>{C.WARN_PARTITIONED}</li>
      </ul>
      <Machines tuple={tuple} since={since} />
      <fieldset className="mt-2.5">
        <legend className="font-medium text-foreground">{C.ATTEST_LABEL}</legend>
        <label className="mt-1 flex items-start gap-2">
          <input type="radio" name="node-record-attestation" checked={attestation === "node_powered_off"} onChange={() => setAttestation("node_powered_off")} />
          <span>
            <span className="text-foreground">{C.ATTEST_POWERED_OFF}</span>
            <span className="block text-muted-foreground">{C.ATTEST_POWERED_OFF_HINT}</span>
          </span>
        </label>
        {noPriorOffered && (
          <label className="mt-1 flex items-start gap-2">
            <input type="radio" name="node-record-attestation" checked={attestation === "no_prior_holder"} onChange={() => setAttestation("no_prior_holder")} />
            <span>
              <span className="text-foreground">{C.ATTEST_NO_PRIOR}</span>
              <span className="block text-muted-foreground">{C.ATTEST_NO_PRIOR_HINT}</span>
            </span>
          </label>
        )}
      </fieldset>
      <div className="mt-2.5 grid gap-2 sm:grid-cols-2">
        <label className="block">
          <span className="font-medium text-foreground">{C.SOURCE_LABEL}</span>
          <select className={`mt-1 ${SELECT_CLASS}`} value={source} onChange={(e) => setSource(e.target.value as NodeRecordSource)}>
            <option value="provider_console">{C.SOURCE_PROVIDER}</option>
            <option value="management_controller">{C.SOURCE_CONTROLLER}</option>
            <option value="other">{C.SOURCE_OTHER}</option>
          </select>
        </label>
        <label className="block">
          <span className="font-medium text-foreground">{C.SOURCE_ID_LABEL}</span>
          <Input className="mt-1 h-9" value={sourceId} maxLength={256} onChange={(e) => setSourceId(e.target.value)} />
          <span className="mt-0.5 block text-muted-foreground">{C.SOURCE_ID_HINT}</span>
        </label>
      </div>
      {twoPeople && <p className="mt-2 text-foreground">{C.TWO_PEOPLE}</p>}
      {held ? (
        <p data-testid="run-node-record-held" className="mt-2 font-medium text-foreground">
          {C.HELD}
        </p>
      ) : (
        <Button type="submit" size="sm" className="mt-2.5" disabled={busy || sourceId.trim() === ""}>
          {busy ? C.SUBMITTING : C.SUBMIT}
        </Button>
      )}
    </form>
  );
}
