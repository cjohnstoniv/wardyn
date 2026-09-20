/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Git tab of /providers — one row per closed kind, always (§2.1): absent
// (its host follows the legacy scm_hosts list), present and on (its base URLs
// admit), present and off (its host is refused — never falls through to
// legacy). The credential lanes render inside the row, byte-for-byte
// GitHostCard's shape — Lane/SecretLane/HostSummary, exported from
// connection-cards.tsx rather than re-typed here (§9.1's file plan).
//
// This file owns the unsaved `git: GitProvider[]` array the parent
// (providers-screen.tsx) holds; every edit calls `onChange` with the next
// array, and the parent's single Save button PUTs the whole document.
import * as React from "react";
import type { GitLane, GitProvider, GitProviderKind } from "../../../lib/api/providers";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import { PERM } from "../../../lib/permissions-copy";
import { PEOPLE } from "../../../lib/people-access-copy";
import { S as GIT_S, HostSummary, Lane as CredentialLane, LaneBody, SecretLane } from "../settings/connection-cards";
import { slugHost } from "../../../lib/scm-provider";
import { Button, buttonVariants } from "../../ui/button";
import { Textarea } from "../../ui/textarea";
import { Checkbox } from "../../ui/checkbox";
import { Field, Switch } from "../../wardyn/form-primitives";
import { Chip } from "../../wardyn/primitives";
import { useRovingRadio } from "../../wardyn/use-roving-radio";
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
import { appLaneAvailable, hostOf, invalidBaseURLLines, KIND_LABEL, LANE_META, laneUnavailableReason, sshLaneAvailable, sshScopedHostLevel } from "./display";

const ALL_LANES: GitLane[] = ["app", "pat", "ssh"];
const ALL_KINDS: GitProviderKind[] = ["github", "azure_devops"];

// The lanes this row's kind + base URLs can actually carry — never `app` on
// azure_devops, never `ssh`/`app` on a self-hosted host that can't offer them
// (laneUnavailableReason, the same predicate the checkbox itself disables
// on). permittedLanes/withLaneToggled both filter through this, so an empty
// `lanes` (wire convention: "every lane the kind supports") can never expand
// into a lane the kind cannot carry: toggling `ssh` off an Azure DevOps row
// must never write `["app","pat"]`, which the server 400s and the disabled
// `app` checkbox then leaves no way to un-write.
function availableLanes(kind: GitProviderKind, baseUrls: string[]): GitLane[] {
  return ALL_LANES.filter((l) => !laneUnavailableReason(l, kind, baseUrls));
}

// The one derivation of base_urls from textarea bytes — used both to commit a
// change and to decide whether an outside change should re-seed the textarea.
function normalizeBaseURLText(text: string): string[] {
  return text.split("\n").map((l) => l.trim()).filter(Boolean);
}

function permittedLanes(row: GitProvider): Set<GitLane> {
  const available = availableLanes(row.kind, row.base_urls);
  const stored = row.lanes && row.lanes.length > 0 ? row.lanes : available;
  return new Set(stored.filter((l) => available.includes(l)));
}

// Wire convention: empty means every lane the kind supports — never a
// three-item array (GitProvider.Lanes' doc comment) — "every lane" meaning
// every available one, not literally all three.
function withLaneToggled(row: GitProvider, lane: GitLane): GitLane[] {
  const available = availableLanes(row.kind, row.base_urls);
  const next = permittedLanes(row);
  if (next.has(lane)) next.delete(lane);
  else next.add(lane);
  const filtered = available.filter((l) => next.has(l));
  return filtered.length === available.length ? [] : filtered;
}

// The credential-host lanes key off the row's own first base URL's host, the
// common case (one provider row = one forge instance).
//
// ponytail: a row spanning two distinct hosts under one
// kind shares one credential set keyed by the first — split it into two rows
// (two kinds is the model's own escape hatch is not available here, since
// both rows would be the same KIND; name the second host a second row is not
// offered in v1) if that ever matters; the mock draws only the single-host
// case.
//
// "" when there is no parseable first address, never a default: an Azure
// DevOps row with an empty or mid-typed address must not key its credential
// lane to github.com, landing a PAT saved inside a radiogroup labelled "Azure
// DevOps credentials" in git-pat-github-com — the secret the GitHub clone
// helper reads. The empty host is what the caller renders the lanes disabled
// on (LANES_NEED_ADDRESS), so there is no name to save under until a real
// address exists.
function rowHost(row: GitProvider): string {
  return hostOf(row.base_urls[0] ?? "");
}

function Row({
  kind,
  row,
  present,
  githubApp,
  operator,
  onUpdate,
  onRemove,
  onAdd,
  onStatusRefresh,
}: {
  kind: GitProviderKind;
  row: GitProvider | undefined;
  present: string[];
  githubApp: boolean;
  operator: boolean;
  onUpdate: (next: GitProvider) => void;
  onRemove: () => void;
  onAdd: () => void;
  /** Appendix A V8: re-fires only the parent's /setup/status read — never
   *  `load()`, which would discard an unsaved base-URL draft edit on this or
   *  a sibling row (agents-tab.tsx's onStatusRefresh precedent). */
  onStatusRefresh: () => void;
}) {
  const [confirmRemove, setConfirmRemove] = React.useState(false);
  // The textarea's raw text, held here rather than derived from
  // row.base_urls.join("\n") every render: splitting on every keystroke fed the
  // filtered array straight back into `value`, so a newline could never survive
  // one render and a second address typed after Enter concatenated onto the
  // first. Each change still commits the split/trimmed array to the row (so
  // Save needs no pending-text dance and the parent's draft is always current),
  // but the displayed text — and the invalidLines/aria-invalid the field flags
  // while typing — come from these bytes.
  const joinedURLs = (row?.base_urls ?? []).join("\n");
  const [baseURLText, setBaseURLText] = React.useState(joinedURLs);
  // Re-seed only when the row's URLs changed from outside this textarea (a
  // Retry reload, a 412 reload, a row just added): our own commits always
  // leave normalizeBaseURLText(baseURLText) === joinedURLs, so they never
  // clobber the newline the admin just typed. Setting state during render is
  // React's own documented shape for prop-derived state; it converges in one
  // extra pass.
  if (normalizeBaseURLText(baseURLText).join("\n") !== joinedURLs) setBaseURLText(joinedURLs);
  // The retired GitHostCard's own derivation (present.includes(sshName) ?
  // "ssh" : appStored ? "app" : "pat") — a row's credential lanes open on
  // whatever is already connected, not always the first lane, and the mock
  // opens the populated GitHub row on App. Computed once, off the row's
  // initial host/props — a later edit to base_urls must not yank the
  // operator's own tab selection out from under them.
  const [credLane, setCredLane] = React.useState<GitLane>(() => {
    if (!row) return "pat";
    const initialHost = rowHost(row);
    const initialSlug = slugHost(initialHost);
    if (initialHost && present.includes(`ssh-key-${initialSlug}`)) return "ssh";
    if (row.kind === "github" && githubApp) return "app";
    return "pat";
  });

  // F4-F13 (Appendix A V8): every hook call stays unconditional (above the
  // `!row` early return below) — the lanes actually rendered, in DOM order
  // (app/ssh are conditional on kind/base_urls, so the roving group's item
  // count and index must track exactly what's on screen, not the full
  // GitLane union). `row?.base_urls ?? []` because `row` can still be
  // undefined here (the absent-row early return hasn't run yet).
  const credLanes: GitLane[] = [
    "pat",
    ...(row && kind === "github" && appLaneAvailable(kind, row.base_urls) ? (["app"] as const) : []),
    ...(row && sshLaneAvailable(row.base_urls) ? (["ssh"] as const) : []),
  ];
  const credGroup = useRovingRadio(credLanes.length, Math.max(0, credLanes.indexOf(credLane)), (i) => setCredLane(credLanes[i]));

  if (!row) {
    return (
      <div className="rounded-lg border border-border opacity-70">
        <div className="flex items-center gap-3 p-3">
          <span className="size-3.5 rounded-full border border-border-strong" aria-hidden="true" />
          <div className="min-w-0 flex-1">
            <span className="block text-sm font-medium text-foreground">{KIND_LABEL[kind]}</span>
            <span className="block text-meta text-muted-foreground">{PROVIDERS.ROW_ABSENT_HINT}</span>
          </div>
          <Button variant="outline" size="sm" disabled={!operator} onClick={onAdd}>
            {PROVIDERS.ADD_ROW_CTA}
          </Button>
        </div>
      </div>
    );
  }

  // "" when the row names no parseable address — see rowHost. Everything keyed
  // off it is gated on it being non-empty.
  const host = rowHost(row);
  const slug = slugHost(host);
  const patName = `git-pat-${slug}`;
  const sshName = `ssh-key-${slug}`;
  const permitted = permittedLanes(row);
  const hosts = row.base_urls.map((u) => u.replace(/^https?:\/\//, "")).join(" · ");
  // The text, not the committed array: a line typed but not yet valid is
  // exactly what the admin needs flagged while typing, and a trailing blank
  // line the array drops must not un-flag the line above it.
  const invalidLines = invalidBaseURLLines(baseURLText, kind);
  // A present row with zero addresses is invalid, not a saveable no-op: the
  // server refuses it outright (`git[i].base_urls: name at least one address`),
  // so this must be flagged — an unflagged empty textarea would leave Save
  // enabled and the row's credential lanes keyed to a defaulted host.
  const noAddresses = normalizeBaseURLText(baseURLText).length === 0;

  return (
    <div className="rounded-lg border border-border" data-testid={`provider-row-${kind}`}>
      <div className="flex items-center gap-3 border-b border-border p-3">
        <Switch
          checked={!row.disabled}
          disabled={!operator}
          label={`${PROVIDERS.FIELD_ENABLED} — ${KIND_LABEL[kind]}`}
          onChange={(checked) => onUpdate({ ...row, disabled: !checked })}
        />
        <div className="min-w-0 flex-1">
          <span className="block text-sm font-medium text-foreground">{KIND_LABEL[kind]}</span>
          <span className="block truncate font-mono text-meta text-muted-foreground">{hosts}</span>
        </div>
        {/* Mock states 1 & 3: the row's own on/off fact as a neutral chip —
            off is a fact, never red (§4's colour rule) — not left to the
            switch position + collapsed-body prose alone. ROW_DISABLED_CHIP is
            its own canon key (the AGENT_ROW_DISABLED_CHIP precedent), never
            ROW_DISABLED_HINT sliced at its colon: a canon edit that drops the
            colon must not dump a whole sentence into the chip. */}
        <Chip tone="neutral">{row.disabled ? PROVIDERS.ROW_DISABLED_CHIP : PROVIDERS.FIELD_ENABLED}</Chip>
        <Button variant="outline" size="sm" disabled={!operator} onClick={() => setConfirmRemove(true)}>
          {PERM.REMOVE}
        </Button>
      </div>

      {row.disabled ? (
        <div className="p-3">
          <p className="text-body text-muted-foreground">{PROVIDERS.ROW_DISABLED_HINT}</p>
          {/* The server refuses a zero-address row whether it is on or off, so
              Save is withheld either way — the cause has to stay readable on a
              row whose textarea is collapsed. */}
          {row.base_urls.length === 0 && <p className="mt-1 text-xs leading-snug text-danger">{PROVIDERS.BASE_URLS_REQUIRED}</p>}
        </div>
      ) : (
        <div className="space-y-4 p-3">
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label={PROVIDERS.FIELD_BASE_URLS} hint={PROVIDERS.BASE_URLS_HINT} htmlFor={`provider-${kind}-base-urls`}>
              <Textarea
                id={`provider-${kind}-base-urls`}
                className="font-mono"
                rows={3}
                aria-invalid={invalidLines.length > 0 || noAddresses}
                disabled={!operator}
                value={baseURLText}
                onChange={(e) => {
                  setBaseURLText(e.target.value);
                  onUpdate({ ...row, base_urls: normalizeBaseURLText(e.target.value) });
                }}
              />
              {noAddresses ? (
                // Its own sentence, not BASE_URL_INVALID: that one diagnoses a
                // typed line ("must be an https:// URL with ...") and reads as
                // nonsense over an empty field.
                <p className="text-xs leading-snug text-danger">{PROVIDERS.BASE_URLS_REQUIRED}</p>
              ) : (
                invalidLines.length > 0 && <p className="text-xs leading-snug text-danger">{PROVIDERS.BASE_URL_INVALID}</p>
              )}
            </Field>
            <Field label={PROVIDERS.FIELD_LANES} hint={PROVIDERS.LANES_HINT}>
              {/* A group, not a labelled control: the Field's label cannot point at
                  three checkboxes, so the group carries the name and each box its own. */}
              <div className="space-y-2" role="group" aria-label={PROVIDERS.FIELD_LANES}>
                {ALL_LANES.map((lane) => {
                  const reason = laneUnavailableReason(lane, kind, row.base_urls);
                  const meta = LANE_META[lane as keyof typeof LANE_META];
                  return (
                    <label key={lane} className="flex items-start gap-2">
                      <Checkbox
                        aria-label={meta.label}
                        checked={permitted.has(lane) && !reason}
                        disabled={!operator || !!reason}
                        onCheckedChange={() => onUpdate({ ...row, lanes: withLaneToggled(row, lane) })}
                      />
                      <span>
                        <span className="block text-body font-medium text-foreground" title={meta.tooltip}>
                          {meta.label}
                        </span>
                        {reason && <span className="block text-meta text-muted-foreground">{reason}</span>}
                      </span>
                    </label>
                  );
                })}
              </div>
              {/* The ceiling, said where the policy is written: an SSH clone URL
                  carries no org path, so a row scoped to one org admits SSH for
                  the whole host. The remedy is in the sentence — drop the ssh
                  lane — which is the control right above it. */}
              {sshScopedHostLevel(row.base_urls, permitted.has("ssh") && !laneUnavailableReason("ssh", kind, row.base_urls)) && (
                <p className="text-xs leading-snug text-muted-foreground">{PROVIDERS.SSH_HOST_LEVEL_HINT}</p>
              )}
            </Field>
          </div>

          {/* The credential lanes, inside the row — GitHostCard's Lane/
              SecretLane/HostSummary, unchanged, keyed to this row's host. No
              free-text Host field: the host is the row's own (§2.1, Q4).
              With no host (no parseable first address) every lane is disabled
              and names no secret: the secret name is derived from the host, so a
              defaulted host would write one row's token into another host's
              secret. The moment a valid address exists the lanes key off its
              host.

              F4-F13 (Appendix A V8): the group needs roving tabindex and
              arrow keys (wardyn/use-roving-radio.ts) over the lanes actually
              rendered for this row's kind/base_urls. A `role="radiogroup"`
              nesting a Save button is an ARIA violation, so the selected
              lane's body renders as a sibling below the group, not inside
              it. */}
          <div role="radiogroup" aria-label={`${KIND_LABEL[kind]} credentials`} className="space-y-2" {...credGroup.containerProps}>
            {!host && <p className="text-xs leading-snug text-muted-foreground">{PROVIDERS.LANES_NEED_ADDRESS}</p>}
            <CredentialLane
              id={`lane-${kind}-pat`}
              title="Personal access token"
              hint="The simplest lane — stored once; a per-run helper hands it to git inside the sandbox at clone time."
              connected={!!host && present.includes(patName)}
              connectedDetail={`${host} · stored as ${patName}`}
              selected={!!host && credLane === "pat"}
              disabled={!host}
              onSelect={() => setCredLane("pat")}
              {...credGroup.itemProps(credLanes.indexOf("pat"))}
            />

            {kind === "github" && appLaneAvailable(kind, row.base_urls) && (
              <CredentialLane
                id={`lane-${kind}-app`}
                title="GitHub App"
                hint="Repo-scoped tokens brokered at the proxy — the token never enters the sandbox."
                connected={!!host && githubApp}
                connectedDetail="Installation credentials stored"
                selected={!!host && credLane === "app"}
                disabled={!host}
                onSelect={() => setCredLane("app")}
                {...credGroup.itemProps(credLanes.indexOf("app"))}
              />
            )}

            {sshLaneAvailable(row.base_urls) && (
              <CredentialLane
                id={`lane-${kind}-ssh`}
                title="SSH key"
                hint="A per-run copy is written inside the sandbox for the clone, then shredded."
                connected={!!host && present.includes(sshName)}
                connectedDetail={`${host} · stored as ${sshName}`}
                selected={!!host && credLane === "ssh"}
                disabled={!host}
                onSelect={() => setCredLane("ssh")}
                {...credGroup.itemProps(credLanes.indexOf("ssh"))}
              />
            )}
          </div>

          {host && credLane === "pat" && (
            <LaneBody>
              <SecretLane
                label="Access token"
                // This row's placeholder, not GitHub's: an Azure DevOps PAT
                // carries no `ghp_` prefix, and suggesting one here risks an
                // ADO token typed into git-pat-github-com. The discriminator
                // is the kind, not the host: a GitHub Enterprise Server row is
                // kind github on a corporate host and its PATs are ghp_ too.
                placeholder={kind === "github" ? "ghp_…" : "Paste the token"}
                secretName={patName}
                stored={present.includes(patName)}
                disabled={!operator}
                onChanged={onStatusRefresh}
                summary={<HostSummary host={host} />}
                hint={GIT_S.STORE_NOTE}
                saveVariant="secondary"
              />
            </LaneBody>
          )}
          {host && credLane === "app" && kind === "github" && appLaneAvailable(kind, row.base_urls) && (
            <LaneBody>
              <div className="space-y-4">
                <SecretLane
                  label="App ID"
                  placeholder="123456"
                  secretName="github-app-id"
                  stored={present.includes("github-app-id")}
                  disabled={!operator}
                  onChanged={onStatusRefresh}
                  saveVariant="secondary"
                />
                <SecretLane
                  label="Private key (PEM)"
                  placeholder="-----BEGIN RSA PRIVATE KEY-----" // gitleaks:allow — a placeholder string, never a real key
                  secretName="github-app-key"
                  stored={present.includes("github-app-key")}
                  disabled={!operator}
                  onChanged={onStatusRefresh}
                  saveVariant="secondary"
                />
              </div>
            </LaneBody>
          )}
          {host && credLane === "ssh" && sshLaneAvailable(row.base_urls) && (
            <LaneBody>
              <SecretLane
                label="Private key"
                placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
                secretName={sshName}
                stored={present.includes(sshName)}
                disabled={!operator}
                onChanged={onStatusRefresh}
                summary={<HostSummary host={host} />}
                saveVariant="secondary"
              />
            </LaneBody>
          )}
        </div>
      )}

      <AlertDialog open={confirmRemove} onOpenChange={setConfirmRemove}>
        <AlertDialogContent>
          <AlertDialogHeader>
            {/* One canon key per slot Radix renders (§7.2) — never one frozen
                sentence sliced on "? ", which reflowed a canon edit into the
                wrong half. */}
            <AlertDialogTitle>{PROVIDERS.REMOVE_CONFIRM_TITLE(KIND_LABEL[kind])}</AlertDialogTitle>
            <AlertDialogDescription>{PROVIDERS.REMOVE_CONFIRM_BODY}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{PEOPLE.CANCEL}</AlertDialogCancel>
            {/* Mock state 3: Remove is outline — it deletes no secret, only a
                row, so it does not carry the destructive/teal weight the
                shipped default gives every AlertDialogAction. */}
            <AlertDialogAction
              className={buttonVariants({ variant: "outline" })}
              onClick={() => {
                setConfirmRemove(false);
                onRemove();
              }}
            >
              {PERM.REMOVE}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

export function GitTab({
  git,
  onChange,
  present,
  githubApp,
  operator,
  // Whether the loaded snapshot had zero rows — true legacy-open mode, where
  // this tab's own Add provider is the state's one affirmative and the screen
  // withholds Save. False with an empty `git` means the admin just removed the
  // last row: the banner still describes what an empty set means, but Save is
  // what commits that removal, so Add steps down to outline (CONSOLE-RULES §2
  // — one teal per surface).
  loadedEmpty = true,
  onStatusRefresh,
}: {
  git: GitProvider[];
  onChange: (next: GitProvider[]) => void;
  present: string[];
  githubApp: boolean;
  operator: boolean;
  loadedEmpty?: boolean;
  /** Appendix A V8: every row's SecretLane onChanged threads here — a
   *  setup-status-only refresh (never the screen's whole `load()`, which
   *  would discard an unsaved base-URL draft edit on this or a sibling row). */
  onStatusRefresh: () => void;
}) {
  const rowFor = (kind: GitProviderKind) => git.find((r) => r.kind === kind);

  const updateRow = (kind: GitProviderKind, next: GitProvider) =>
    onChange(git.some((r) => r.kind === kind) ? git.map((r) => (r.kind === kind ? next : r)) : [...git, next]);

  const removeRow = (kind: GitProviderKind) => onChange(git.filter((r) => r.kind !== kind));

  const addRow = (kind: GitProviderKind) =>
    // A fresh Azure DevOps row starts at `https://dev.azure.com/` — invalid on
    // purpose (the org segment is required there, §5.1), so the row shows
    // BASE_URL_INVALID until the admin appends their org; a bare
    // `https://github.com` is a valid host-wide row and needs no edit.
    onChange([...git, { id: kind, kind, base_urls: [kind === "github" ? "https://github.com" : "https://dev.azure.com/"] }]);

  return (
    <div className="space-y-4">
      <p className="text-body text-muted-foreground">{PROVIDERS.GIT_LEAD}</p>

      {git.length === 0 ? (
        // True legacy open mode (mock state 2): the banner alone — no
        // per-kind row list underneath it. Once one row exists, the still-
        // absent kind gets its own mini "Add provider" affordance instead
        // (below), which is a different illustration in the mock, not this
        // same state.
        <div className="rounded-lg border border-dashed border-border p-6 text-center">
          <h4 className="text-sm font-medium text-foreground">{PROVIDERS.LEGACY_OPEN_TITLE}</h4>
          <p className="mt-1.5 text-body text-muted-foreground">{PROVIDERS.LEGACY_OPEN_BODY}</p>
          <p className="mt-1 text-meta text-muted-foreground">{PROVIDERS.LEGACY_OPEN_OTHER_HOSTS}</p>
          <Button className="mt-4" variant={loadedEmpty ? "default" : "outline"} disabled={!operator} onClick={() => addRow("github")}>
            {PROVIDERS.ADD_ROW_CTA}
          </Button>
        </div>
      ) : (
        <div className="space-y-3">
          {ALL_KINDS.map((kind) => (
            <Row
              key={kind}
              kind={kind}
              row={rowFor(kind)}
              present={present}
              githubApp={githubApp}
              operator={operator}
              onUpdate={(next) => updateRow(kind, next)}
              onRemove={() => removeRow(kind)}
              onAdd={() => addRow(kind)}
              onStatusRefresh={onStatusRefresh}
            />
          ))}
        </div>
      )}

      <p className="text-meta leading-snug text-muted-foreground">{GIT_S.GIT_FOOTER}</p>
    </div>
  );
}
