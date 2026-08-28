/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The admin Permissions screen (0.6 pillar 2, WS-A stage A-E/E2) — the seventh
// sidebar entry, beside Policies, admin-only. One page, three blocks, in the
// order the reviewed mock draws them (docs/design/permissioning-mock/index.html):
// header facts → per-kind enforcement → the grant table + add form.
//
// EVERY user-visible string here comes from lib/permissions-copy.ts. That module
// is the frozen canon (docs/design/permissioning-prompt.md §7); this file adds no
// copy of its own, so the shipped wording cannot drift from the reviewed mock.
//
// Two honesty rules are structural, not styling:
//   1. A grant is AMBER, never green. An allow row is a widened blast radius —
//      there is no success tone anywhere on this screen, and no success toast
//      when a grant lands.
//   2. Never render enforcement that isn't happening. A kind that is off says
//      "Not enforced" next to every grant it governs, and its grants say
//      "Advisory until enforced".
import * as React from "react";
import { Info, Loader2, ShieldCheck } from "lucide-react";
import { toast } from "sonner";
import { permissions as api } from "../../lib/api/permissions";
import { runs as runsApi } from "../../lib/api/runs";
import { getErrorMessage, relativeTime } from "../../lib/format";
import { CAPABILITY_KINDS, KIND, PERM, type CapabilityKind } from "../../lib/permissions-copy";
import type {
  AgentRun,
  CapabilityEffect,
  CapabilityGrant,
  CapabilitySubjectType,
  PermissionsSnapshot,
} from "../../lib/types";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../ui/alert-dialog";
import { cn } from "../ui/utils";
import { Field } from "../wardyn/form-primitives";
import { Mono } from "../wardyn/code-block";
import { PageHeader } from "../wardyn/page-header";
import { Chip } from "../wardyn/primitives";
import { EmptyState, ErrorState, TableSkeleton } from "../wardyn/states";
import { useOperator, usePrincipal } from "../wardyn/operator-context";

// The audit actor a bare admin-bearer caller is recorded as (actorFromRequest,
// internal/api/runs_policy.go) — a machine lane, never a member.
const ADMIN_TOKEN_PRINCIPAL = "admin-token";

// The subject-type segments, in the order the add form offers them, paired with
// the hint each one puts under the Who field.
const SUBJECTS: { value: CapabilitySubjectType; label: string; hint: string }[] = [
  { value: "user", label: PERM.SUBJECT_USER, hint: PERM.HINT_USER },
  { value: "group", label: PERM.SUBJECT_GROUP, hint: PERM.HINT_GROUP },
  { value: "all", label: PERM.SUBJECT_ALL, hint: PERM.HINT_ALL },
];

const SUBJECT_LABEL: Record<CapabilitySubjectType, string> = {
  user: PERM.SUBJECT_USER,
  group: PERM.SUBJECT_GROUP,
  all: PERM.SUBJECT_ALL,
};

// A stored grant may name a kind this build doesn't know (the schema puts no
// CHECK on `capability` on purpose — see migration 0042). Render it verbatim
// rather than blank: an unknown row is inert, not invisible.
function kindLabel(capability: string): string {
  return KIND[capability as CapabilityKind]?.label ?? capability;
}

// Who a row names, for the remove confirmation.
function subjectText(g: CapabilityGrant): string {
  return g.subject_type === "all" ? PERM.SUBJECT_ALL : g.subject;
}

// A quiet standing fact in the header — not an alert. The doctrine and the
// exemption are the two most-misread facts about this feature, so they are on
// the screen rather than in a doc.
function Fact({ icon: Icon, children }: { icon: React.ElementType; children: React.ReactNode }) {
  return (
    <p className="flex max-w-[82ch] items-start gap-2 text-body text-muted-foreground">
      <Icon className="mt-0.5 size-3.5 shrink-0" />
      <span>{children}</span>
    </p>
  );
}

// A consequence note under a kind. `tone` carries the meaning: plain for the
// advisory/posture facts, red for the two that bite (a deny with the switch
// off, and enforcing with nothing granted).
function Note({ tone = "plain", children }: { tone?: "plain" | "red"; children: React.ReactNode }) {
  return (
    <p
      className={cn(
        "mt-2 max-w-[78ch] rounded-lg px-3 py-2 text-xs leading-relaxed",
        tone === "red" ? "bg-danger-subtle text-danger" : "bg-muted text-muted-foreground",
      )}
    >
      {children}
    </p>
  );
}

// A two- or three-way segmented picker. Buttons with aria-pressed, not tabs and
// not a Select: these are form choices, they are all visible at once in the
// mock, and a plain button is the one control that stays clickable in both the
// vitest and Playwright harnesses without a pointer-events dance.
function Segmented<T extends string>({
  value,
  options,
  onChange,
  disabled,
}: {
  value: T;
  options: { value: T; label: string }[];
  onChange: (v: T) => void;
  disabled?: boolean;
}) {
  return (
    <div className="inline-flex w-fit overflow-hidden rounded-lg border border-border-strong">
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          aria-pressed={value === o.value}
          disabled={disabled}
          onClick={() => onChange(o.value)}
          className={cn(
            "border-l border-border px-3 py-1.5 text-xs transition-colors first:border-l-0 disabled:cursor-not-allowed disabled:opacity-50",
            value === o.value
              ? o.value === "deny"
                ? "bg-danger-subtle font-medium text-danger"
                : "bg-muted font-medium text-foreground"
              : "text-muted-foreground hover:text-foreground",
          )}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

export function PermissionsScreen() {
  const operator = useOperator();
  const [snap, setSnap] = React.useState<PermissionsSnapshot>({ grants: [], enforcement: {} });
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [confirm, setConfirm] = React.useState<{ kind: CapabilityKind; next: boolean } | null>(null);
  const [toRemove, setToRemove] = React.useState<CapabilityGrant | null>(null);
  const [busy, setBusy] = React.useState(false);

  const load = React.useCallback(() => {
    setStatus("loading");
    api
      .getPermissions()
      .then((s) => {
        setSnap(s);
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, []);
  React.useEffect(load, [load]);

  const memberCount = useAffectedMemberCount();

  const grantsFor = React.useCallback(
    (kind: CapabilityKind) => snap.grants.filter((g) => g.capability === kind),
    [snap.grants],
  );

  const writeEnforcement = async (kind: CapabilityKind, next: boolean) => {
    setBusy(true);
    try {
      // FULL-MAP replace: send every kind's current state with this one
      // changed, because an omitted key is a real "stop enforcing".
      const body: Record<string, boolean> = {};
      for (const k of CAPABILITY_KINDS) body[k] = k === kind ? next : !!snap.enforcement[k];
      const saved = await api.putEnforcement(body);
      setSnap((s) => ({ ...s, enforcement: saved }));
      setConfirm(null);
    } catch (e) {
      toast.error("Failed to save enforcement", { description: getErrorMessage(e) });
    } finally {
      setBusy(false);
    }
  };

  const removeGrant = async (g: CapabilityGrant) => {
    setBusy(true);
    try {
      await api.deleteGrant(g.id);
      setSnap((s) => ({ ...s, grants: s.grants.filter((x) => x.id !== g.id) }));
      setToRemove(null);
    } catch (e) {
      toast.error("Failed to remove grant", { description: getErrorMessage(e) });
    } finally {
      setBusy(false);
    }
  };

  const nothingEnforced = CAPABILITY_KINDS.every((k) => !snap.enforcement[k]);

  return (
    <div className="mx-auto max-w-[1120px] px-6 py-6">
      <PageHeader title={PERM.TITLE} description={PERM.LEAD} />

      <div className="space-y-2">
        <Fact icon={ShieldCheck}>{PERM.DOCTRINE}</Fact>
        <Fact icon={Info}>{PERM.EXEMPT}</Fact>
      </div>

      {/* The upgrade-from-0.5 posture: every kind off and no grants at all. */}
      {status === "ready" && nothingEnforced && snap.grants.length === 0 && (
        <Note>{PERM.DEFAULT_POSTURE}</Note>
      )}

      <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card">
        <div className="px-6 pt-5">
          <h2 className="text-sm font-medium text-foreground">{PERM.ENFORCEMENT_TITLE}</h2>
          <p className="mt-0.5 text-body text-muted-foreground">{PERM.ENFORCEMENT_LEAD}</p>
        </div>
        <div className="mt-4">
          {CAPABILITY_KINDS.map((kind) => (
            <KindRow
              key={kind}
              kind={kind}
              enforced={!!snap.enforcement[kind]}
              grants={grantsFor(kind)}
              disabled={!operator || status !== "ready"}
              onToggle={(next) => setConfirm({ kind, next })}
            />
          ))}
        </div>
      </section>

      <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card">
        <div className="px-6 pt-5">
          <h2 className="text-sm font-medium text-foreground">{PERM.GRANTS_TITLE}</h2>
          <p className="mt-1 text-body text-muted-foreground">{PERM.GRANT_IS_NOT_SUCCESS}</p>
          <Note>{PERM.PRECEDENCE}</Note>
        </div>
        <div className="mt-4">
          {status === "loading" ? (
            <TableSkeleton rows={4} cols={5} />
          ) : status === "error" ? (
            <ErrorState onRetry={load} />
          ) : snap.grants.length === 0 ? (
            <EmptyState icon={ShieldCheck} title={PERM.EMPTY_TITLE} description={PERM.EMPTY_BODY} />
          ) : (
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>{PERM.COL_WHO}</TableHead>
                  <TableHead>{PERM.COL_CAPABILITY}</TableHead>
                  <TableHead>{PERM.COL_VALUE}</TableHead>
                  <TableHead>{PERM.COL_EFFECT}</TableHead>
                  <TableHead>{PERM.COL_ADDED}</TableHead>
                  <TableHead className="w-[80px]" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {snap.grants.map((g) => (
                  <TableRow key={g.id} className={cn(g.effect === "deny" && "bg-danger-subtle")}>
                    <TableCell>
                      <span className="flex flex-wrap items-center gap-2">
                        <Chip tone="neutral">{SUBJECT_LABEL[g.subject_type] ?? g.subject_type}</Chip>
                        {g.subject_type !== "all" && <Mono>{g.subject}</Mono>}
                      </span>
                    </TableCell>
                    <TableCell>{kindLabel(g.capability)}</TableCell>
                    <TableCell>
                      <Mono>{g.value}</Mono>
                    </TableCell>
                    <TableCell>
                      {/* Amber for allow — a widened blast radius, never a
                          success tone. Red for deny, the strongest row here. */}
                      <Chip tone={g.effect === "deny" ? "danger" : "warning"}>
                        {g.effect === "deny" ? PERM.EFFECT_DENY : PERM.EFFECT_ALLOW}
                      </Chip>
                    </TableCell>
                    <TableCell className="whitespace-nowrap text-muted-foreground" title={g.created_at}>
                      {relativeTime(g.created_at)}
                    </TableCell>
                    <TableCell>
                      <Button
                        variant="ghost"
                        size="sm"
                        disabled={!operator}
                        onClick={() => setToRemove(g)}
                        aria-label={`${PERM.REMOVE} ${kindLabel(g.capability)} ${g.value}`}
                      >
                        {PERM.REMOVE}
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </div>
      </section>

      <AddGrantForm disabled={!operator} onAdded={load} />

      <section className="mt-6 rounded-xl border border-border bg-card px-6 py-5">
        <h2 className="text-sm font-medium text-foreground">{PERM.SNAPSHOT_TITLE}</h2>
        <p className="mt-1 max-w-[80ch] text-body text-muted-foreground">{PERM.SNAPSHOT_BODY}</p>
      </section>

      <ConfirmEnforcement
        state={confirm}
        memberCount={memberCount}
        hasAllow={confirm ? grantsFor(confirm.kind).some((g) => g.effect === "allow") : false}
        busy={busy}
        onCancel={() => setConfirm(null)}
        onConfirm={() => confirm && writeEnforcement(confirm.kind, confirm.next)}
      />

      <AlertDialog open={!!toRemove} onOpenChange={(o) => !o && setToRemove(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{PERM.REMOVE}</AlertDialogTitle>
            <AlertDialogDescription>
              {PERM.REMOVE_CONFIRM(toRemove ? subjectText(toRemove) : "")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              className="bg-danger text-danger-foreground hover:bg-danger/90"
              onClick={(e) => {
                e.preventDefault();
                if (toRemove) removeGrant(toRemove);
              }}
            >
              {busy ? <Loader2 className="size-4 animate-spin" /> : null}
              {PERM.REMOVE}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

// One enforcement row: the state chip, the consequence sentence for the state
// the kind is CURRENTLY in, and the switch. The sentence is the point of the
// block; the switch is the smaller half.
function KindRow({
  kind,
  enforced,
  grants,
  disabled,
  onToggle,
}: {
  kind: CapabilityKind;
  enforced: boolean;
  grants: CapabilityGrant[];
  disabled: boolean;
  onToggle: (next: boolean) => void;
}) {
  const copy = KIND[kind];
  const hasDeny = grants.some((g) => g.effect === "deny");
  const hasAllow = grants.some((g) => g.effect === "allow");
  return (
    <div className="grid grid-cols-[1fr_auto] items-start gap-4 border-t border-border px-6 py-4 first:border-t-0">
      <div>
        <div className="flex flex-wrap items-center gap-2">
          <h3 className="text-body font-medium text-foreground">{copy.label}</h3>
          <Chip tone={enforced ? "warning" : "neutral"} dot>
            {enforced ? PERM.CHIP_ON : PERM.CHIP_OFF}
          </Chip>
        </div>
        <p className="mt-0.5 text-xs text-muted-foreground">{copy.blurb}</p>
        <p className={cn("mt-2 max-w-[76ch] text-body", enforced ? "text-foreground" : "text-muted-foreground")}>
          {enforced ? copy.enforced : copy.unenforced}
        </p>
        {/* Grants exist but the switch is off: they are recorded, not live. */}
        {!enforced && grants.length > 0 && <Note>{PERM.ADVISORY}</Note>}
        {/* The one thing that bites with the switch off. */}
        {!enforced && hasDeny && <Note tone="red">{PERM.DENY_BEFORE_ENFORCE}</Note>}
        {/* The dangerous cell: enforced with nothing granted. */}
        {enforced && !hasAllow && <Note tone="red">{PERM.ENFORCE_ON_ZERO}</Note>}
      </div>
      <Switch
        checked={enforced}
        disabled={disabled}
        onCheckedChange={onToggle}
        aria-label={`${PERM.ENFORCEMENT_TITLE} ${copy.label}`}
      />
    </div>
  );
}

// ponytail: a local switch, not a restored ui/switch.tsx. cb351ba9 dropped that
// primitive AND its @radix-ui/react-switch dependency as never-imported — true
// when it landed, and this lane is now its ONE consumer. A native button with
// role="switch" is the same accessible contract the tests assert, in ten lines
// and no dependency, following the same aria-checked pattern Seg/RadioCard use.
function Switch({
  checked,
  disabled,
  onCheckedChange,
  "aria-label": ariaLabel,
}: {
  checked: boolean;
  disabled?: boolean;
  onCheckedChange: (next: boolean) => void;
  "aria-label": string;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={ariaLabel}
      disabled={disabled}
      onClick={() => onCheckedChange(!checked)}
      className={cn(
        "inline-flex h-[1.15rem] w-8 shrink-0 items-center rounded-full border border-transparent p-[1px] transition-colors outline-none focus-visible:ring-[3px] focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50",
        checked ? "bg-primary" : "bg-muted",
      )}
    >
      <span
        aria-hidden="true"
        className={cn(
          "pointer-events-none block size-4 rounded-full bg-card shadow-sm transition-transform",
          checked ? "translate-x-[calc(100%-2px)]" : "translate-x-0",
        )}
      />
    </button>
  );
}

// The switch-on / switch-off confirmation. The zero-grant form is the lockout
// guard — the top risk in this whole workstream — so it is impossible to walk
// past: the note is red and the confirm button turns destructive.
function ConfirmEnforcement({
  state,
  memberCount,
  hasAllow,
  busy,
  onCancel,
  onConfirm,
}: {
  state: { kind: CapabilityKind; next: boolean } | null;
  memberCount: number;
  hasAllow: boolean;
  busy: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const turningOn = !!state?.next;
  const lockout = turningOn && !hasAllow;
  return (
    <AlertDialog open={!!state} onOpenChange={(o) => !o && onCancel()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>
            {(turningOn ? PERM.ENFORCE_ON_TITLE : PERM.ENFORCE_OFF_TITLE)(state ? KIND[state.kind].label : "")}
          </AlertDialogTitle>
          <AlertDialogDescription>
            {turningOn ? PERM.ENFORCE_ON_BODY(memberCount) : PERM.ENFORCE_OFF_BODY}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {lockout && <Note tone="red">{PERM.ENFORCE_ON_ZERO}</Note>}
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            className={cn(
              lockout
                ? "bg-danger text-danger-foreground hover:bg-danger/90"
                : turningOn
                  ? "bg-warning text-background hover:bg-warning/90"
                  : undefined,
            )}
            onClick={(e) => {
              e.preventDefault();
              onConfirm();
            }}
          >
            {busy ? <Loader2 className="size-4 animate-spin" /> : null}
            {turningOn ? PERM.ENFORCE_CONFIRM : PERM.ENFORCE_STOP}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

// The add form. Wildcards are typed into Value, not a separate control — the
// value field's label and hint are what change per kind.
function AddGrantForm({ disabled, onAdded }: { disabled: boolean; onAdded: () => void }) {
  const [subjectType, setSubjectType] = React.useState<CapabilitySubjectType>("user");
  const [subject, setSubject] = React.useState("");
  const [kind, setKind] = React.useState<CapabilityKind>("egress_host");
  const [value, setValue] = React.useState("");
  const [effect, setEffect] = React.useState<CapabilityEffect>("allow");
  const [saving, setSaving] = React.useState(false);
  const [duplicate, setDuplicate] = React.useState(false);

  const copy = KIND[kind];
  const whoHint = SUBJECTS.find((s) => s.value === subjectType)?.hint ?? "";
  const ready = !!value.trim() && (subjectType === "all" || !!subject.trim());

  const submit = async () => {
    setSaving(true);
    setDuplicate(false);
    try {
      const res = await api.upsertGrant({
        subject_type: subjectType,
        subject: subjectType === "all" ? "" : subject.trim(),
        capability: kind,
        value: value.trim(),
        effect,
      });
      // No success toast, ever: a grant is not an achievement. The row landing
      // in the table above is the whole confirmation.
      setDuplicate(res.updated);
      setValue("");
      onAdded();
    } catch (e) {
      toast.error("Failed to add grant", { description: getErrorMessage(e) });
    } finally {
      setSaving(false);
    }
  };

  return (
    <section className="mt-6 rounded-xl border border-border bg-card px-6 py-5">
      <h2 className="text-sm font-medium text-foreground">{PERM.ADD_TITLE}</h2>
      <div className="mt-4 grid gap-4 md:grid-cols-2 lg:grid-cols-4">
        <Field label={PERM.FIELD_WHO} hint={whoHint}>
          <div className="space-y-2">
            <Segmented
              value={subjectType}
              onChange={(v) => setSubjectType(v)}
              disabled={disabled}
              options={SUBJECTS.map((s) => ({ value: s.value, label: s.label }))}
            />
            {subjectType !== "all" && (
              <Input
                aria-label={PERM.FIELD_WHO}
                value={subject}
                onChange={(e) => setSubject(e.target.value)}
                disabled={disabled}
                className="font-mono"
                autoComplete="off"
              />
            )}
          </div>
        </Field>

        <Field label={PERM.FIELD_CAPABILITY} htmlFor="grant-capability">
          <Select value={kind} onValueChange={(v) => setKind(v as CapabilityKind)} disabled={disabled}>
            <SelectTrigger id="grant-capability">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {CAPABILITY_KINDS.map((k) => (
                <SelectItem key={k} value={k}>
                  {KIND[k].label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>

        <Field label={copy.valueLabel} htmlFor="grant-value" hint={copy.valueHint} required>
          <Input
            id="grant-value"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            disabled={disabled}
            className="font-mono"
            autoComplete="off"
            spellCheck={false}
            required
          />
        </Field>

        <Field label={PERM.FIELD_EFFECT}>
          <Segmented
            value={effect}
            onChange={(v) => setEffect(v)}
            disabled={disabled}
            options={[
              { value: "allow", label: PERM.EFFECT_ALLOW },
              { value: "deny", label: PERM.EFFECT_DENY },
            ]}
          />
        </Field>
      </div>
      <div className="mt-4 flex flex-wrap items-center gap-3">
        <Button onClick={submit} disabled={disabled || saving || !ready}>
          {saving ? <Loader2 className="size-4 animate-spin" /> : null}
          {PERM.ADD_CTA}
        </Button>
        {duplicate && <Chip tone="info">{PERM.DUPLICATE}</Chip>}
      </div>
    </section>
  );
}

// How many members the enforcement confirm says are about to be bounded.
//
// ponytail: there is no member registry to count — Wardyn keeps no user table,
// and OIDC identities exist only inside a session cookie. The console's one
// durable record of who has used this deployment is the runs list, so this
// counts DISTINCT run creators, minus the signed-in admin (exempt) and minus
// the admin-token machine lane. Ceiling: it over-counts if another ADMIN has
// launched a run (the safe direction — a louder warning), and it cannot see a
// member who has never launched one. Upgrade path: a server-side principal
// roster (or a count on GET /permissions), at which point this hook reads it
// instead of deriving it.
function useAffectedMemberCount(): number {
  const principal = usePrincipal();
  const [count, setCount] = React.useState(0);
  React.useEffect(() => {
    let live = true;
    runsApi
      .listRuns()
      .then((rs: AgentRun[]) => {
        if (!live) return;
        const people = new Set(
          rs
            .map((r) => r.created_by)
            .filter((p) => !!p && p !== principal && p !== ADMIN_TOKEN_PRINCIPAL),
        );
        setCount(people.size);
      })
      .catch(() => {
        /* the dialog still states the consequence; only the count is unknown */
      });
    return () => {
      live = false;
    };
  }, [principal]);
  return count;
}
