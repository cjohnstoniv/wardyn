/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The assignments block — who runs under which profile, the form that binds
// one, and the resolved-profile preview (governance-mock states 1, 5, 10, 11).
//
// Three things here are load-bearing rather than styling:
//
//  1. The add form COLLAPSES to its disabled ADD_TITLE summary row while the
//     profile editor above is open (`collapsed`), taking its teal Assign with
//     it — one `default` button on the screen at any moment (CONSOLE-RULES §6)
//     without inventing a modal. A disabled state on a form already on the
//     page, which is §7's "a busy control is disabled, not removed".
//  2. EFFECT_NOTE and SIGNIN_NOTE render TOGETHER: they are two halves of one
//     fact with different subjects (an assignment binds at the next RUN; a
//     person's group membership is re-read at their next SIGN-IN), and without
//     the second, assigning to a group and watching nothing happen for an
//     already-signed-in member looks like a broken write.
//  3. The preview resolves CLAIMS, never a person — Wardyn keeps no
//     server-side session row, so no admin request can read a live member's
//     groups (§O Q2: forced, not chosen).
//
// Every product string comes from the copy modules. This file adds none.
import * as React from "react";
import { Loader2, Users } from "lucide-react";
import { toast } from "sonner";
import {
  governance as api,
  type GovernanceAssignment,
  type GovernancePreview,
  type GovernanceProfile,
  type GovernanceSnapshot,
} from "../../../lib/api/governance";
import { getErrorMessage, relativeTime } from "../../../lib/format";
import { GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { PEOPLE, PREVIEW } from "../../../lib/people-access-copy";
import { PERM } from "../../../lib/permissions-copy";
import type { CapabilitySubjectType } from "../../../lib/types";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../../ui/alert-dialog";
import { Button, buttonVariants } from "../../ui/button";
import { Input } from "../../ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../ui/table";
import { Textarea } from "../../ui/textarea";
import { Mono } from "../../wardyn/code-block";
import { Field } from "../../wardyn/form-primitives";
import { Chip } from "../../wardyn/primitives";
import { EmptyState } from "../../wardyn/states";
import { Segmented } from "../permissions";
import { Note, question } from "./display";

// The subject vocabulary is permissions-copy.ts's, never a second copy of it
// (§5 #6): a governance assignment and a capability grant must not disagree
// about what "Everyone signed in" means.
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

// What PREVIEW_RESULT's {matched} says — the matching row named in the table's
// own vocabulary (§7.3).
const MATCHED_LABEL: Record<CapabilitySubjectType, string> = {
  user: GOV.MATCHED_USER,
  group: GOV.MATCHED_GROUP,
  all: GOV.MATCHED_ALL,
};

// Who a row names, for the unassign confirmation.
const subjectText = (a: GovernanceAssignment): string =>
  a.subject_type === "all" ? PERM.SUBJECT_ALL : a.subject;

// The claims box is kind-LESS — one textarea, the People step's own field — so
// a typed line may be a sign-in subject, an email or a group and nothing here
// can tell. Every line therefore goes to BOTH wire lists and the SERVER offers
// each to both tiers; its answer names the tier that matched, which is what
// PREVIEW_RESULT renders. Exactly the shape access-panel.tsx already posts to
// /access/preview ({roles: lines, groups: lines}).
//
// ponytail: the ONE thing decided here is ORDER, never precedence. The user
// tier's tie-break is POSITION in this list (the resolver's array_position),
// and the enforcement path builds it sign-in-subject first, email second
// (capabilitySubjects) — so an "@" line sorts last and the preview agrees with
// what actually binds the member. Array.prototype.sort is stable, so every
// other line keeps the order it was typed in. The ranking itself stays in SQL.
const claimLines = (claims: string): string[] =>
  claims
    .split("\n")
    .map((c) => c.trim())
    .filter(Boolean);

const subjectOrder = (lines: string[]): string[] =>
  [...lines].sort((a, b) => Number(a.includes("@")) - Number(b.includes("@")));

export function AssignmentsBlock({
  snapshot,
  disabled,
  collapsed,
  onChanged,
}: {
  snapshot: GovernanceSnapshot;
  disabled: boolean;
  /** The profile editor is open: the add form collapses for exactly that span. */
  collapsed: boolean;
  onChanged: () => void;
}) {
  const [toRemove, setToRemove] = React.useState<GovernanceAssignment | null>(null);
  const [busy, setBusy] = React.useState(false);
  const profileName = (id: string) => snapshot.profiles.find((p) => p.id === id)?.name ?? id;

  const remove = async (a: GovernanceAssignment) => {
    setBusy(true);
    try {
      await api.deleteAssignment(a.id);
      setToRemove(null);
      onChanged();
    } catch (e) {
      toast.error(PERM.REMOVE, { description: getErrorMessage(e) });
    } finally {
      setBusy(false);
    }
  };

  const confirm = toRemove ? GOV.UNASSIGN_CONFIRM(subjectText(toRemove), profileName(toRemove.profile_id)) : "";
  const [confirmHead, confirmBody] = question(confirm);

  return (
    <>
      <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card">
        <div className="px-6 pt-5">
          <h2 className="text-sm font-medium text-foreground">{GOV.ASSIGN_TITLE}</h2>
          <p className="mt-1 text-body text-muted-foreground">{GOV.ASSIGN_LEAD}</p>
          <Note>{GOV.PRECEDENCE}</Note>
        </div>
        <div className="mt-4">
          {snapshot.assignments.length === 0 ? (
            <EmptyState icon={Users} title={GOV.EMPTY_ASSIGN_TITLE} description={GOV.EMPTY_ASSIGN_BODY} />
          ) : (
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>{PERM.COL_WHO}</TableHead>
                  <TableHead>{GOV.COL_PROFILE}</TableHead>
                  <TableHead>{GOV.COL_PRIORITY}</TableHead>
                  <TableHead>{PERM.COL_ADDED}</TableHead>
                  <TableHead className="w-[90px]" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {snapshot.assignments.map((a) => (
                  <TableRow key={a.id}>
                    <TableCell>
                      <span className="flex flex-wrap items-center gap-2">
                        {/* Neutral, always: an assignment has no effect column,
                            so the /permissions amber-allow / red-deny law does
                            not reach it. */}
                        <Chip tone="neutral">{SUBJECT_LABEL[a.subject_type] ?? a.subject_type}</Chip>
                        {a.subject_type !== "all" && <Mono>{a.subject}</Mono>}
                      </span>
                    </TableCell>
                    <TableCell>{profileName(a.profile_id)}</TableCell>
                    {/* Priority is meaningful only inside the group tier. */}
                    <TableCell>{a.subject_type === "group" ? a.priority : GOV.PRIORITY_NA}</TableCell>
                    <TableCell className="whitespace-nowrap text-muted-foreground" title={a.created_at}>
                      {relativeTime(a.created_at)}
                    </TableCell>
                    <TableCell>
                      {/* outline, never destructive: unassigning is reversible. */}
                      <Button
                        variant="outline"
                        size="sm"
                        disabled={disabled}
                        onClick={() => setToRemove(a)}
                        aria-label={`${PERM.REMOVE} ${subjectText(a)}`}
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

        {collapsed ? (
          <div
            data-testid="governance-add-assignment-collapsed"
            className="mt-4 flex items-center justify-between gap-3 border-t border-border bg-surface-2 px-6 py-4 opacity-60"
          >
            <h3 className="text-sm font-medium text-foreground">{GOV.ADD_TITLE}</h3>
            <Button variant="outline" disabled>
              {GOV.ADD_CTA}
            </Button>
          </div>
        ) : (
          <AddAssignmentForm
            profiles={snapshot.profiles}
            disabled={disabled}
            teal={snapshot.profiles.length > 0}
            onAdded={onChanged}
          />
        )}
      </section>

      <ResolvedPreview snapshot={snapshot} />

      <AlertDialog open={!!toRemove} onOpenChange={(o) => !o && setToRemove(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{confirmHead}</AlertDialogTitle>
            <AlertDialogDescription>{confirmBody}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{PEOPLE.CANCEL}</AlertDialogCancel>
            <AlertDialogAction
              className={buttonVariants({ variant: "outline" })}
              onClick={(e) => {
                e.preventDefault();
                if (toRemove) remove(toRemove);
              }}
            >
              {busy ? <Loader2 className="size-4 animate-spin" /> : null}
              {PERM.REMOVE}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

// The add form. Its Assign is the screen's `default` button at rest — unless
// there is no profile to assign yet, in which case the empty state's New
// profile carries the teal instead (`teal`).
function AddAssignmentForm({
  profiles,
  disabled,
  teal,
  onAdded,
}: {
  profiles: GovernanceProfile[];
  disabled: boolean;
  teal: boolean;
  onAdded: () => void;
}) {
  const [subjectType, setSubjectType] = React.useState<CapabilitySubjectType>("group");
  const [subject, setSubject] = React.useState("");
  const [profileID, setProfileID] = React.useState("");
  const [priority, setPriority] = React.useState("0");
  const [saving, setSaving] = React.useState(false);

  const whoHint = SUBJECTS.find((s) => s.value === subjectType)?.hint ?? "";
  const ready = !!profileID && (subjectType === "all" || !!subject.trim());

  const submit = async () => {
    setSaving(true);
    try {
      await api.upsertAssignment({
        subject_type: subjectType,
        subject: subjectType === "all" ? "" : subject.trim(),
        profile_id: profileID,
        priority: Number(priority) || 0,
      });
      setSubject("");
      onAdded();
    } catch (e) {
      toast.error(GOV.ADD_TITLE, { description: getErrorMessage(e) });
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="mt-4 border-t border-border px-6 py-5">
      <h3 className="text-sm font-medium text-foreground">{GOV.ADD_TITLE}</h3>
      {/* Two halves of one fact — see this file's header. */}
      <p className="mt-1 max-w-[82ch] text-body text-muted-foreground">{GOV.EFFECT_NOTE}</p>
      <p className="mt-1 max-w-[82ch] text-body text-muted-foreground">{GOV.SIGNIN_NOTE}</p>

      <div className="mt-4 grid gap-4 md:grid-cols-2 lg:grid-cols-3">
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

        <Field label={GOV.FIELD_PROFILE} htmlFor="governance-assignment-profile">
          <Select value={profileID} onValueChange={setProfileID} disabled={disabled || profiles.length === 0}>
            <SelectTrigger id="governance-assignment-profile">
              <SelectValue placeholder={GOV.PROFILE_PLACEHOLDER} />
            </SelectTrigger>
            <SelectContent>
              {profiles.map((p) => (
                <SelectItem key={p.id} value={p.id}>
                  {p.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>

        {/* A number the API takes and the audit row records — never a drag
            surface, which would silently rewrite priorities on rows nobody
            touched and cannot express a deliberate tie (§O Q4). */}
        <Field label={GOV.FIELD_PRIORITY} htmlFor="governance-assignment-priority" hint={GOV.PRIORITY_HINT}>
          <Input
            id="governance-assignment-priority"
            type="number"
            value={priority}
            onChange={(e) => setPriority(e.target.value)}
            disabled={disabled}
            className="font-mono"
          />
        </Field>
      </div>

      <div className="mt-4">
        <Button variant={teal ? "default" : "outline"} onClick={submit} disabled={disabled || saving || !ready}>
          {saving ? <Loader2 className="size-4 animate-spin" /> : null}
          {GOV.ADD_CTA}
        </Button>
      </div>
    </div>
  );
}

// The resolved preview: a claims dry run, saving nothing.
//
// It asks the SERVER (POST /governance/preview), which runs
// Store.ResolveGovernanceProfile — the same call every real run makes. So the
// answer is against the server's current rows rather than whatever this screen
// loaded minutes ago, AND it is produced by the one implementation of the
// precedence rule. A failed request is the one honest cause of
// PREVIEW_RESULT_UNKNOWN.
function ResolvedPreview({ snapshot }: { snapshot: GovernanceSnapshot }) {
  const [claims, setClaims] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [result, setResult] = React.useState<{ answer: GovernancePreview } | { error: true } | null>(null);

  const run = async () => {
    setBusy(true);
    try {
      const lines = claimLines(claims);
      setResult({ answer: await api.previewGovernance({ user_subjects: subjectOrder(lines), groups: lines }) });
    } catch {
      setResult({ error: true });
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="mt-6 rounded-xl border border-border bg-card px-6 py-5">
      <h2 className="text-sm font-medium text-foreground">{GOV.PREVIEW_TITLE}</h2>
      <p className="mt-1 max-w-[82ch] text-body text-muted-foreground">{GOV.PREVIEW_LEAD}</p>
      <div className="mt-4 grid gap-4 md:grid-cols-[1fr_auto]">
        {/* The People step's own field and hint, verbatim: two labels for one
            accepted shape is how they drift apart (§7.3). */}
        <Field label={PREVIEW.FIELD_CLAIMS} htmlFor="governance-preview-claims" hint={PREVIEW.FIELD_CLAIMS_HINT}>
          <Textarea
            id="governance-preview-claims"
            value={claims}
            onChange={(e) => setClaims(e.target.value)}
            className="font-mono"
            rows={3}
          />
        </Field>
        <div className="flex items-start">
          <Button variant="outline" onClick={run} disabled={busy || !claims.trim() || snapshot.profiles.length === 0}>
            {busy ? <Loader2 className="size-4 animate-spin" /> : null}
            {GOV.PREVIEW_RUN_CTA}
          </Button>
        </div>
      </div>
      {/* An empty answer is "no assignment matched" — the deployment ceiling,
          the same absent-key doctrine the wire type documents. The name and
          the tier ship together, so one test covers both. */}
      {result &&
        ("error" in result ? (
          <Note tone="red">{GOV.PREVIEW_RESULT_UNKNOWN}</Note>
        ) : result.answer.profile_name && result.answer.matched_tier ? (
          <Note>{GOV.PREVIEW_RESULT(result.answer.profile_name, MATCHED_LABEL[result.answer.matched_tier])}</Note>
        ) : (
          <Note>{GOV.PREVIEW_RESULT_DEFAULT}</Note>
        ))}
      <p className="mt-3 text-xs text-muted-foreground">{GOV.PREVIEW_NOT_SAVED}</p>
    </section>
  );
}
