/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin -> Runners drawer for a self-hosted runner (M-RN-1 RA2, mock M-POOLS v6 RN2): its Machine, its
// resources and limits (its owner's to set; an admin reads them), the pools it is in, its facts and
// Revoke. Machine, posture, barriers and allowed folders are what the runner said about itself, each
// labelled so. The key fingerprint is shown whole and has no copy control, here or anywhere in the
// console. An unclaimed runner has reported nothing that matters and relays nothing, so it shows the
// wait, the fingerprint and Revoke; a revoked one shows who revoked it and no Revoke.
import * as React from "react";
import { toast } from "sonner";
import { runners as runnersApi } from "../../../lib/api/runners";
import { absoluteTime, relativeTime } from "../../../lib/format";
import type { RunnerView } from "../../../lib/types/runners";
import { RES, RUNNER_TRUST, RUNNER_V6 } from "../../wardyn/copy/runner-pools";
import { RUNNER, RUNNERS } from "../../wardyn/copy/runners";
import { useSecurityOperator } from "../../wardyn/operator-context";
import { Mono } from "../../wardyn/code-block";
import { Button } from "../../ui/button";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "../../ui/sheet";
import { Hint, KindChip, OfflineNote, ResourceTable, RunsLine } from "./resource-table";
import { amountsText, runnerRes } from "./resources";
import { ArchText, MachineText, PoolLinks } from "./runner-card";
import { RunnerChip, TrustChip } from "./runner-chip";
import { barrierNames, postureLines, shortDate } from "./runner-display";
import { ConfirmDialog } from "./confirm-dialog";
import { RunnerTrust } from "./runner-trust";

function Section({ label, reported, children, ...rest }: { label: string; reported?: string; children: React.ReactNode } & React.HTMLAttributes<HTMLElement>) {
  return (
    <section className="border-b border-border py-3" {...rest}>
      <div className="mb-1 flex items-baseline justify-between gap-3">
        <h3 className="label-eyebrow">{label}</h3>
        {reported && <span className="text-meta text-muted-foreground">{reported}</span>}
      </div>
      <div className="space-y-0.5 text-body text-foreground">{children}</div>
    </section>
  );
}

function Header({ runner }: { runner: RunnerView }) {
  const dated = (text: (when: string) => string, iso: string) => (
    <span title={absoluteTime(iso)}>{text(shortDate(iso))}</span>
  );
  const parts: React.ReactNode[] = [runner.owner || RUNNER_V6.ORGANIZATION];
  if (runner.state === "revoked" && runner.revoked_at) {
    parts.push(dated((when) => RUNNER.REVOKED_BY(runner.revoked_by ?? "", when), runner.revoked_at));
  } else {
    parts.push(dated(RUNNER.REGISTERED, runner.created_at));
    if (runner.claimed_at) parts.push(dated(RUNNER.CLAIMED, runner.claimed_at));
  }
  return (
    <div className="flex flex-wrap items-center gap-x-1.5 text-meta text-muted-foreground">
      {parts.map((p, i) => (
        <React.Fragment key={i}>
          {i > 0 && <span aria-hidden>·</span>}
          {p}
        </React.Fragment>
      ))}
    </div>
  );
}

export function RunnerDrawer({
  runner,
  onClose,
  onChanged,
  onOpenPool,
}: {
  runner: RunnerView | null;
  onClose: () => void;
  /** The runner was revoked, or turned out to be already: re-read the list. */
  onChanged: () => void;
  onOpenPool: (id: string) => void;
}) {
  const [confirming, setConfirming] = React.useState(false);
  const securityAdmin = useSecurityOperator();
  const titleRef = React.useRef<HTMLSpanElement | null>(null);
  if (!runner) return null;
  const claimed = runner.state === "claimed";
  const reportedAt = runner.posture_reported_at ? RUNNER.REPORTED_AT(relativeTime(runner.posture_reported_at)) : "";
  const reported = (extra?: string) => [RUNNER.REPORTED, extra].filter(Boolean).join(" · ");
  const res = runnerRes(runner.limits, runner.usage);

  return (
    <>
      <Sheet open onOpenChange={(o) => !o && onClose()}>
        <SheetContent
          className="scroll-thin w-full gap-0 overflow-y-auto sm:max-w-[640px]"
          // Focus lands on the title, which names what opened (the sheet's close control is not a place to start).
          onOpenAutoFocus={(e) => {
            e.preventDefault();
            titleRef.current?.focus();
          }}
        >
          <SheetHeader className="border-b border-border pb-4">
            <SheetTitle className="pr-8">
              <span ref={titleRef} tabIndex={-1} className="outline-none">
                {runner.name}
              </span>
            </SheetTitle>
            <SheetDescription asChild>
              <div>
                <Header runner={runner} />
                {runner.state === "unclaimed" && runner.owner && (
                  <p className="mt-1 text-meta text-muted-foreground">{RUNNERS.WAITING_CLAIM(runner.owner)}</p>
                )}
              </div>
            </SheetDescription>
            <div className="flex flex-wrap items-center gap-2">
              <KindChip kind="self_hosted" />
              <RunnerChip runner={runner} />
              <TrustChip runner={runner} />
            </div>
          </SheetHeader>
          <div className="px-4">
            <Section label={RUNNER_TRUST.TITLE} reported={RUNNER_TRUST.META} data-trust>
              <RunnerTrust runner={runner} canEdit={securityAdmin} onChanged={onChanged} />
            </Section>
            {claimed && (
              <>
                <Section label={RUNNER_V6.MACHINE} reported={RUNNER.REPORTED} data-machine>
                  <MachineText machine={runner.machine_name} />
                  <ArchText arch={runner.arch} />
                  <Hint>{RUNNER_V6.MACHINE_HINT}</Hint>
                </Section>
                <Section label={RES.RESOURCES} reported={RUNNER_V6.LIMITS_BY_OWNER} data-resources>
                  {res && runner.limits ? (
                    <>
                      <RunsLine max={runner.limits.runs_at_once} active={runner.usage.in_use.runs} />
                      <ResourceTable view={res} />
                    </>
                  ) : (
                    <p>{RES.LIMITS_NOT_SET}</p>
                  )}
                  {!runner.online && <OfflineNote />}
                  <Hint>{RUNNER_V6.OWNER_ONLY}</Hint>
                  {runner.usage.reported && (
                    <Hint>
                      {RUNNER_V6.REPORTS_ITS(amountsText(runner.usage.reported))} <span>{RUNNER.REPORTED}</span>
                    </Hint>
                  )}
                </Section>
                <Section label={RUNNER_V6.POOLS} data-pools>
                  <PoolLinks pools={runner.pools} onOpenPool={onOpenPool} />
                  <Hint>{RUNNER_V6.POOLS_HINT}</Hint>
                </Section>
                <Section label={RUNNER.POSTURE} reported={reported(reportedAt)}>
                  {runner.posture ? postureLines(runner.posture).map((l) => <div key={l}>{l}</div>) : RUNNER.NOT_REPORTED}
                </Section>
                <Section label={RUNNER.BARRIERS} reported={reported()}>
                  {barrierNames(runner.classes)}
                </Section>
                <Section label={RUNNER.ROOTS} reported={reported()}>
                  {runner.allowed_roots?.length ? (
                    runner.allowed_roots.map((r) => (
                      <div key={r}>
                        <Mono>{r}</Mono>
                      </div>
                    ))
                  ) : runner.capacity ? (
                    // The server omits an empty list, so "none" is known only once the runner has reported.
                    RUNNER.ROOTS_NONE
                  ) : (
                    RUNNER.NOT_REPORTED
                  )}
                </Section>
              </>
            )}
            <Section label={RUNNER.FINGERPRINT}>
              <Mono className="break-all">{runner.key_fingerprint}</Mono>
            </Section>
            {claimed && runner.version && (
              <Section label={RUNNER.VERSION}>
                <Mono>{runner.version}</Mono>
              </Section>
            )}
            {runner.state !== "revoked" && (
              <div className="py-4">
                <Button variant="outline" onClick={() => setConfirming(true)}>
                  {RUNNERS.REVOKE}
                </Button>
              </div>
            )}
          </div>
        </SheetContent>
      </Sheet>
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title={RUNNERS.REVOKE_TITLE(runner.name)}
        body={[RUNNERS.REVOKE_BODY, ...(runner.usage.in_use.runs > 0 ? [RUNNER.REVOKE_RUNS(runner.usage.in_use.runs)] : [])]}
        confirmLabel={RUNNER.REVOKE_CONFIRM}
        onConfirm={() => runnersApi.revoke(runner.id)}
        onDone={() => {
          toast.success(RUNNER.REVOKED_TOAST(runner.name));
          onChanged();
          // Revoke no longer exists on this drawer, so focus returns to its title.
          requestAnimationFrame(() => titleRef.current?.focus());
        }}
        onRaced={onChanged}
      />
    </>
  );
}
