/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The allocations block — who gets a drive, the form that binds one, and the
// "Who gets what" preview (docs/design/user-drives-mock/index.html, states 2, 4
// and 5).
//
// Four things here are load-bearing rather than styling:
//
//  1. The add form COLLAPSES to its disabled ADD_TITLE summary row while the
//     drive editor above is open, taking its teal Allocate with it — one
//     `default` button on the screen at any moment, without inventing a modal.
//  2. EFFECT_NOTE and SIGNIN_NOTE render TOGETHER: an ALLOCATION is a row the
//     resolver re-reads every run, so it binds at the next RUN; a person's
//     GROUP MEMBERSHIP is read once at sign-in, so a group's allocation reaches
//     them at their next SIGN-IN. Without the second, allocating to a group and
//     watching nothing happen looks like a broken write.
//  3. A DIRECTORY NAME IS A PERSON'S. home_override is accepted on a user-tier
//     row only (a group cannot share one directory), so the field is disabled
//     on the other two tiers and HOME_OVERRIDE_NA replaces its hint — the same
//     400 the server raises, avoided rather than met.
//  4. The preview resolves CLAIMS, never a person, and it asks the SERVER: the
//     identical resolveUserDriveFor the enforcement path takes. There is no
//     client-side precedence here at all.
//
// REMOVE IS `outline`, never destructive: unallocating is reversible and
// deletes nothing — the directory stays until the admin reclaims it.
//
// Every product string comes from user-drives-copy.ts and the modules §7.1
// defers to. This file adds none.
import * as React from "react";
import { Loader2, Users } from "lucide-react";
import { toast } from "sonner";
import { previewClaims } from "../../../lib/api/governance";
import {
  drives as api,
  type UserDriveGrant,
  type UserDriveListItem,
  type UserDrivePreview,
} from "../../../lib/api/drives";
import { getErrorMessage, relativeTime } from "../../../lib/format";
import { GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { DRIVES, PEOPLE, PERM, PREVIEW } from "../../../lib/user-drives-copy";
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
import { DirectoryCombobox } from "../../wardyn/directory-combobox";
import { Field, Switch } from "../../wardyn/form-primitives";
import { Chip } from "../../wardyn/primitives";
import { EmptyState } from "../../wardyn/states";
import { SUBJECTS, SUBJECT_LABEL, Segmented, subjectText } from "../permissions";
import { Note, enforcementGloss, modeText, modeTone, question, sizeText } from "./display";

// PREVIEW_RESULT's {tier}, frozen in §7.3's table rather than in prose.
const TIER_LABEL: Record<CapabilitySubjectType, string> = {
  user: DRIVES.PREVIEW_TIER_USER,
  group: DRIVES.PREVIEW_TIER_GROUP,
  all: DRIVES.PREVIEW_TIER_ALL,
};

// A writable override is a THIRD state on the wire (an absent *bool), so the
// control has three positions and "inherit" is not `false`.
type WritableChoice = "inherit" | "rw" | "ro";
const WRITABLE_CHOICES: { value: WritableChoice; label: string }[] = [
  { value: "inherit", label: DRIVES.WRITABLE_OVERRIDE_INHERIT },
  { value: "rw", label: DRIVES.MODE_RW },
  { value: "ro", label: DRIVES.MODE_RO },
];

export function AllocationsBlock({
  drives,
  grants,
  disabled,
  collapsed,
  onChanged,
}: {
  drives: UserDriveListItem[];
  grants: UserDriveGrant[];
  disabled: boolean;
  /** The drive editor is open: the add form collapses for exactly that span. */
  collapsed: boolean;
  onChanged: () => void;
}) {
  const [toRemove, setToRemove] = React.useState<UserDriveGrant | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [replaced, setReplaced] = React.useState(false);
  const driveName = (id: string) => drives.find((d) => d.id === id)?.name ?? id;

  const remove = async (g: UserDriveGrant) => {
    setBusy(true);
    try {
      await api.deleteGrant(g.id);
      setToRemove(null);
      onChanged();
    } catch (e) {
      toast.error(PERM.REMOVE, { description: getErrorMessage(e) });
    } finally {
      setBusy(false);
    }
  };

  const confirm = toRemove ? DRIVES.REMOVE_CONFIRM(subjectText(toRemove), driveName(toRemove.drive_id)) : "";
  const [confirmHead, confirmBody] = question(confirm);

  return (
    <>
      <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card">
        <div className="px-6 pt-5">
          <h2 className="text-sm font-medium text-foreground">{DRIVES.ALLOC_TITLE}</h2>
          <p className="mt-1 text-body text-muted-foreground">{DRIVES.ALLOC_LEAD}</p>
          <Note>{DRIVES.PRECEDENCE}</Note>
        </div>
        <div className="mt-4">
          {grants.length === 0 ? (
            <EmptyState icon={Users} title={DRIVES.EMPTY_ALLOC_TITLE} description={DRIVES.EMPTY_ALLOC_BODY} />
          ) : (
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>{PERM.COL_WHO}</TableHead>
                  <TableHead>{DRIVES.COL_DRIVE}</TableHead>
                  <TableHead>{GOV.COL_PRIORITY}</TableHead>
                  <TableHead>{DRIVES.COL_OVERRIDES}</TableHead>
                  <TableHead>{PERM.COL_ADDED}</TableHead>
                  <TableHead className="w-[90px]" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {grants.map((g) => (
                  <TableRow key={g.id}>
                    <TableCell>
                      <span className="flex flex-wrap items-center gap-2">
                        <Chip tone="neutral">{SUBJECT_LABEL[g.subject_type] ?? g.subject_type}</Chip>
                        {g.subject_type !== "all" && <Mono>{g.subject}</Mono>}
                      </span>
                    </TableCell>
                    {/* A drive name is a human-chosen label, never mono. */}
                    <TableCell>{driveName(g.drive_id)}</TableCell>
                    {/* Priority is meaningful only inside the group tier. */}
                    <TableCell>{g.subject_type === "group" ? g.priority : GOV.PRIORITY_NA}</TableCell>
                    <TableCell>
                      <Overrides grant={g} />
                    </TableCell>
                    <TableCell className="whitespace-nowrap text-muted-foreground" title={g.created_at}>
                      {relativeTime(g.created_at)}
                    </TableCell>
                    <TableCell>
                      <Button
                        variant="outline"
                        size="sm"
                        disabled={disabled}
                        onClick={() => setToRemove(g)}
                        aria-label={`${PERM.REMOVE} ${subjectText(g)}`}
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
            data-testid="drives-add-allocation-collapsed"
            className="mt-4 flex items-center justify-between gap-3 border-t border-border bg-surface-2 px-6 py-4 opacity-60"
          >
            <h3 className="text-sm font-medium text-foreground">{DRIVES.ADD_TITLE}</h3>
            <Button variant="outline" disabled>
              {DRIVES.ADD_CTA}
            </Button>
          </div>
        ) : (
          <AddAllocationForm
            drives={drives}
            disabled={disabled}
            replaced={replaced}
            onAdded={(wasReplaced) => {
              setReplaced(wasReplaced);
              onChanged();
            }}
          />
        )}
      </section>

      <DrivePreview />

      <AlertDialog open={!!toRemove} onOpenChange={(o) => !o && setToRemove(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{confirmHead}</AlertDialogTitle>
            <AlertDialogDescription>{confirmBody}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{PEOPLE.CANCEL}</AlertDialogCancel>
            {/* outline, never destructive: nothing on the drive is deleted. */}
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

// Zero or more chips: the size override, the writable override in the mode's
// own chip vocabulary, the directory override, and Paused for enabled=false.
// OVERRIDES_NONE only when there are none AND the row is enabled.
function Overrides({ grant: g }: { grant: UserDriveGrant }) {
  const chips: React.ReactNode[] = [];
  if (g.size_mib_override) {
    chips.push(
      <Chip key="size" tone="neutral">
        {DRIVES.OVERRIDE_SIZE(sizeText(g.size_mib_override))}
      </Chip>,
    );
  }
  if (g.writable_override != null) {
    chips.push(
      <Chip key="mode" tone={modeTone(g.writable_override)}>
        {modeText(g.writable_override)}
      </Chip>,
    );
  }
  if (g.home_override) {
    chips.push(
      // The chip's label around a MONO directory name: the name is a literal
      // path segment the resolver matches byte for byte, so it is set the way
      // every other wire value on this screen is. Composed from the frozen
      // template's own text (OVERRIDE_HOME with an empty name) rather than a
      // second spelling of it.
      <Chip key="home" tone="neutral">
        {DRIVES.OVERRIDE_HOME("")}
        <Mono className="text-inherit">{g.home_override}</Mono>
      </Chip>,
    );
  }
  if (!g.enabled) {
    chips.push(
      <Chip key="paused" tone="neutral">
        {DRIVES.PAUSED_CHIP}
      </Chip>,
    );
  }
  if (chips.length === 0) return <>{DRIVES.OVERRIDES_NONE}</>;
  return <span className="flex flex-wrap items-center gap-1.5">{chips}</span>;
}

// The add form. Its Allocate IS the screen's `default` button at rest: this
// whole block renders only once a drive exists to allocate (drives-screen.tsx),
// so the case the teal had to be handed back for — an empty registry, where the
// drives empty state's New drive carries it — never reaches this form.
function AddAllocationForm({
  drives,
  disabled,
  replaced,
  onAdded,
}: {
  drives: UserDriveListItem[];
  disabled: boolean;
  /** The last upsert repointed an existing row (a 200, not a 201). */
  replaced: boolean;
  onAdded: (replaced: boolean) => void;
}) {
  const [subjectType, setSubjectType] = React.useState<CapabilitySubjectType>("group");
  const [subject, setSubject] = React.useState("");
  const [driveID, setDriveID] = React.useState("");
  const [priority, setPriority] = React.useState("0");
  const [sizeOverride, setSizeOverride] = React.useState("");
  const [writable, setWritable] = React.useState<WritableChoice>("inherit");
  const [homeOverride, setHomeOverride] = React.useState("");
  const [enabled, setEnabled] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const whoHint = SUBJECTS.find((s) => s.value === subjectType)?.hint ?? "";
  const userTier = subjectType === "user";
  const ready = !!driveID && (subjectType === "all" || !!subject.trim());

  const submit = async () => {
    setSaving(true);
    try {
      const res = await api.upsertGrant({
        subject_type: subjectType,
        subject: subjectType === "all" ? "" : subject.trim(),
        drive_id: driveID,
        priority: Number(priority) || 0,
        size_mib_override: Number(sizeOverride) || 0,
        // Omitted, not false: an absent *bool is "same as the drive", which is
        // neither of the two booleans.
        ...(writable === "inherit" ? {} : { writable_override: writable === "rw" }),
        home_override: userTier ? homeOverride.trim() : "",
        enabled,
      });
      setSubject("");
      onAdded(res.replaced);
    } catch (e) {
      toast.error(DRIVES.ADD_TITLE, { description: getErrorMessage(e) });
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="mt-4 border-t border-border px-6 py-5">
      <h3 className="text-sm font-medium text-foreground">{DRIVES.ADD_TITLE}</h3>
      {/* Two halves of one fact — see this file's header. */}
      <p className="mt-1 max-w-[82ch] text-body text-muted-foreground">{DRIVES.EFFECT_NOTE}</p>
      <p className="mt-1 max-w-[82ch] text-body text-muted-foreground">{DRIVES.SIGNIN_NOTE}</p>

      <div className="mt-4 grid gap-4 md:grid-cols-2 lg:grid-cols-3">
        <Field label={PERM.FIELD_WHO} hint={whoHint}>
          <div className="space-y-2">
            <Segmented
              value={subjectType}
              onChange={(v) => setSubjectType(v)}
              disabled={disabled}
              options={SUBJECTS.map((s) => ({ value: s.value, label: s.label }))}
            />
            {/* Free text with suggestions ON TOP, never instead of; with no
                directory configured this is the plain input it has always been. */}
            {subjectType !== "all" && (
              <DirectoryCombobox
                label={PERM.FIELD_WHO}
                value={subject}
                onChange={setSubject}
                kind={subjectType}
                disabled={disabled}
              />
            )}
          </div>
        </Field>

        <Field label={DRIVES.FIELD_DRIVE} htmlFor="drive-allocation-drive">
          <Select value={driveID} onValueChange={setDriveID} disabled={disabled || drives.length === 0}>
            <SelectTrigger id="drive-allocation-drive">
              <SelectValue placeholder={DRIVES.DRIVE_PLACEHOLDER} />
            </SelectTrigger>
            <SelectContent>
              {drives.map((d) => (
                <SelectItem key={d.id} value={d.id}>
                  {d.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>

        <Field label={GOV.FIELD_PRIORITY} htmlFor="drive-allocation-priority" hint={GOV.PRIORITY_HINT}>
          <Input
            id="drive-allocation-priority"
            type="number"
            value={priority}
            onChange={(e) => setPriority(e.target.value)}
            disabled={disabled || subjectType !== "group"}
            className="font-mono"
          />
        </Field>

        <Field
          label={DRIVES.FIELD_SIZE_OVERRIDE}
          htmlFor="drive-allocation-size"
          hint={DRIVES.SIZE_OVERRIDE_HINT}
        >
          <Input
            id="drive-allocation-size"
            type="number"
            min={0}
            value={sizeOverride}
            onChange={(e) => setSizeOverride(e.target.value)}
            disabled={disabled}
            className="font-mono"
          />
        </Field>

        <Field label={DRIVES.FIELD_WRITABLE_OVERRIDE} hint={DRIVES.WRITABLE_OVERRIDE_HINT}>
          <Segmented value={writable} onChange={setWritable} disabled={disabled} options={WRITABLE_CHOICES} />
        </Field>

        <Field
          label={DRIVES.FIELD_HOME_OVERRIDE}
          htmlFor="drive-allocation-home"
          // A group cannot share one directory, so the field is disabled off the
          // user tier and the hint says which rows may name one.
          hint={userTier ? DRIVES.HOME_OVERRIDE_HINT : DRIVES.HOME_OVERRIDE_NA}
        >
          <Input
            id="drive-allocation-home"
            value={userTier ? homeOverride : ""}
            onChange={(e) => setHomeOverride(e.target.value)}
            disabled={disabled || !userTier}
            className="font-mono"
            autoComplete="off"
          />
        </Field>

        <Field label={DRIVES.FIELD_ENABLED} hint={DRIVES.ENABLED_HINT}>
          <Switch checked={enabled} onChange={setEnabled} disabled={disabled} label={DRIVES.FIELD_ENABLED} />
        </Field>
      </div>

      {/* An inline plain note, not a toast: an allocation that silently replaced
          another is worth reading twice. */}
      {replaced && <Note>{DRIVES.ALLOC_REPLACED}</Note>}

      <div className="mt-4">
        <Button onClick={submit} disabled={disabled || saving || !ready}>
          {saving ? <Loader2 className="size-4 animate-spin" /> : null}
          {DRIVES.ADD_CTA}
        </Button>
      </div>
    </div>
  );
}

// "Who gets what" — a claims dry run, saving nothing.
//
// It asks the SERVER (POST /drives/preview), which runs the same
// resolveUserDriveFor every real run makes, and prints the exact OBJECT NAME
// the offboarding command needs. This is the only surface that renders that
// name; no member-facing string ever does.
//
// It never gates itself on there being allocations to match. With none, the
// server answers {} and PREVIEW_NONE says "no drive is allocated to these
// claims" — which is the true answer, and the one an admin checking their work
// came here for. A disabled button would have withheld it.
function DrivePreview() {
  const [claims, setClaims] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [result, setResult] = React.useState<{ answer: UserDrivePreview } | { error: true } | null>(null);

  const run = async () => {
    setBusy(true);
    try {
      setResult({ answer: await api.previewDrive(previewClaims(claims)) });
    } catch {
      setResult({ error: true });
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="mt-6 rounded-xl border border-border bg-card px-6 py-5">
      <h2 className="text-sm font-medium text-foreground">{DRIVES.PREVIEW_TITLE}</h2>
      <p className="mt-1 max-w-[82ch] text-body text-muted-foreground">{DRIVES.PREVIEW_LEAD}</p>
      <div className="mt-4 grid gap-4 md:grid-cols-[1fr_auto]">
        {/* The People step's own field and hint, verbatim: the preview takes the
            claims a token would carry and has no label of its own (§7.3). */}
        <Field label={PREVIEW.FIELD_CLAIMS} htmlFor="drive-preview-claims" hint={PREVIEW.FIELD_CLAIMS_HINT}>
          <Textarea
            id="drive-preview-claims"
            value={claims}
            onChange={(e) => setClaims(e.target.value)}
            className="font-mono"
            rows={3}
          />
        </Field>
        <div className="flex items-start">
          <Button variant="outline" onClick={run} disabled={busy || !claims.trim()}>
            {busy ? <Loader2 className="size-4 animate-spin" /> : null}
            {DRIVES.PREVIEW_CTA}
          </Button>
        </div>
      </div>
      {result &&
        ("error" in result ? (
          <Note tone="red">{GOV.PREVIEW_RESULT_UNKNOWN}</Note>
        ) : result.answer.drive_name && result.answer.matched_tier ? (
          <PreviewResult answer={result.answer} />
        ) : (
          // An empty object is "no allocation matched" — the absent-key doctrine
          // the wire type documents.
          <Note>{DRIVES.PREVIEW_NONE}</Note>
        ))}
      <p className="mt-3 text-xs text-muted-foreground">{GOV.PREVIEW_NOT_SAVED}</p>
    </section>
  );
}

// The answered preview. A PAUSED winner derives no home and no object name —
// nothing above a paused row is computed, because nothing would mount — so
// those two rows are absent rather than empty, and the chip says why.
function PreviewResult({ answer: a }: { answer: UserDrivePreview }) {
  return (
    <Note>
      <span className="flex flex-wrap items-center gap-2">
        <b className="font-semibold">{DRIVES.PREVIEW_RESULT(a.drive_name!, TIER_LABEL[a.matched_tier!])}</b>
        {a.paused && <Chip tone="neutral">{DRIVES.PAUSED_CHIP}</Chip>}
      </span>
      <dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5" data-testid="drives-preview-result">
        {a.home_name && (
          <>
            <dt>{DRIVES.FIELD_HOME}</dt>
            <dd className="min-w-0">
              <Mono className="text-inherit">{a.home_name}</Mono>
            </dd>
          </>
        )}
        {a.object_name && (
          <>
            <dt>{DRIVES.PREVIEW_OBJECT_LABEL}</dt>
            <dd className="min-w-0">
              <Mono className="text-inherit">{a.object_name}</Mono>
              <span className="mt-0.5 block">{DRIVES.PREVIEW_OBJECT_HINT}</span>
            </dd>
          </>
        )}
        <dt>{DRIVES.COL_SIZE}</dt>
        <dd className="min-w-0">{sizeText(a.size_mib)}</dd>
        <dt>{DRIVES.COL_MODE}</dt>
        <dd className="min-w-0">
          <Chip tone={modeTone(a.writable)}>{modeText(a.writable)}</Chip>
        </dd>
        <dt>{DRIVES.PREVIEW_ENFORCEMENT_LABEL}</dt>
        <dd className="min-w-0">{enforcementGloss(a.enforcement)}</dd>
      </dl>
    </Note>
  );
}
