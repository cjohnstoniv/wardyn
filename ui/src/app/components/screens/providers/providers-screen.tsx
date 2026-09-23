/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The /providers screen — the admin's org policy over git hosts and
// storage ceilings. Super-only, no nav item: reached from the funnel's
// `providers` step card and the Settings card (setup/providers-card.tsx),
// exactly the /drives precedent (drives-screen.tsx's own header note) —
// nothing links a security admin or a member here, but the URL is a URL, and
// both GET/PUT /workspace-providers are operatorOnly, so their read is a 403.
// That is a tier refusal, not a transport failure, and this screen answers it
// as one.
//
// Every user-visible string here comes from workspace-providers-copy.ts (§7,
// frozen) or the reused canon it names (S.GIT_FOOTER, PERM.*, DRIVES.*). This
// file adds no copy of its own.
//
// One `default` (teal) button at a time (CONSOLE-RULES §6, prompt §4): Save
// providers, under the active tab. The Agents tab (C-UI, W4) is present but
// empty here — a Segmented option with an empty body, not hidden: the frozen
// mock always draws three tabs, and C-UI lands its real body in the same
// wave the member-facing Model access states ship, so the control's shape
// shouldn't move twice. Empty, not a placeholder sentence: a placeholder
// needs its own frozen row (§7.7 has none), and an empty body is the
// smaller change of the two.
import * as React from "react";
import { AlertTriangle } from "lucide-react";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { providers as api, type WorkspaceProviders } from "../../../lib/api/providers";
import { setup as setupApi } from "../../../lib/api/setup";
import type { SetupStatus } from "../../../lib/types";
import { getErrorMessage } from "../../../lib/format";
import { readableDiff } from "../../../lib/readable-diff";
import { useUnsavedGuard } from "../../../lib/use-unsaved-guard";
import { useRegisterUnsaved } from "../../../lib/unsaved-registry";
import { useWriteDropped } from "../../../lib/use-write-dropped";
import { REAUTH_DIALOG } from "../../../lib/reauth-copy";
import { AGENTS, PROVIDERS, PROVIDERS_DRAFT } from "../../../lib/workspace-providers-copy";
import { ACCESS_STATE } from "../../../lib/people-access-copy";
import { Button } from "../../ui/button";
import { PageHeader } from "../../wardyn/page-header";
import { OperatorOnlyHint } from "../../wardyn/primitives";
import { SavedElsewhereBanner } from "../../wardyn/saved-elsewhere-banner";
import { EmptyState, TableSkeleton, loadFailStatus } from "../../wardyn/states";
import { useOperator } from "../../wardyn/operator-context";
import { Segmented } from "../permissions";
import { AgentsTab } from "./agents-tab";
import { gitRowInvalid } from "./display";
import { GitTab } from "./git-tab";
import { StorageTab } from "./storage-tab";

type Tab = "git" | "storage" | "agents";
type ScreenStatus = "loading" | "forbidden" | "error" | "ready";

const EMPTY: WorkspaceProviders = {};

export function ProvidersScreen() {
  const operator = useOperator();
  const [status, setStatus] = React.useState<ScreenStatus>("loading");
  const [setupStatus, setSetupStatus] = React.useState<SetupStatus | null>(null);
  const [draft, setDraft] = React.useState<WorkspaceProviders>(EMPTY);
  // #217 — the snapshot `draft` started from, kept only so "Copy my changes"
  // and the unsaved-navigation guard both have something to diff against.
  // Reset alongside `draft` on every load() and every successful save, so
  // neither reads a just-saved document as still dirty.
  const [original, setOriginal] = React.useState<WorkspaceProviders>(EMPTY);
  const [etag, setEtag] = React.useState<string | null>(null);
  const [tab, setTab] = React.useState<Tab>("git");
  const [saving, setSaving] = React.useState(false);
  // #483: a save of this screen's was refused when the session ended.
  const [writeDropped, clearWriteDropped] = useWriteDropped("providers");
  const [saveError, setSaveError] = React.useState<string | null>(null);
  const [savedElsewhere, setSavedElsewhere] = React.useState(false);
  // Stays on the page as an amber note until the NEXT save (Q8, drawn as (b))
  // — a count of newly-refused sources is worth reading twice, not only in
  // the transient toast.
  const [narrowed, setNarrowed] = React.useState<number | null>(null);
  // Whether the loaded snapshot — never the draft — has zero git rows. The Save
  // withhold below is about the state the org is in: true legacy-open mode,
  // whose own banner owns the one affirmative. Gating it on the draft would mean
  // removing the last row hides the only button that could save that removal,
  // silently discarding the change on navigation.
  const [loadedEmpty, setLoadedEmpty] = React.useState(true);

  const load = React.useCallback(() => {
    setSavedElsewhere(false);
    Promise.all([api.getWorkspaceProviders(), setupApi.getSetupStatus()])
      .then(([snap, s]) => {
        setDraft(snap.providers);
        setOriginal(snap.providers);
        setLoadedEmpty((snap.providers.git ?? []).length === 0);
        setEtag(snap.etag);
        setSetupStatus(s);
        setStatus("ready");
      })
      // GET /workspace-providers is operatorOnly, like GET /drives — a 403 is
      // the tier, not the network (drives-screen.tsx's own comment).
      .catch((e) => setStatus(loadFailStatus(e)));
  }, []);
  React.useEffect(load, [load]);

  // The Agents tab's own Save re-fires only this — never `load` (A-01): a
  // successful agent save needs `setupStatus` refreshed (modelAccess/
  // harnesses can be stale the instant a per_user lane is declared), but
  // `load` also resets `draft` (the Git/Storage tabs' own unsaved edits),
  // clears `savedElsewhere`, and a transient GET failure here would flip the
  // whole screen to FETCH_FAILED right after a successful, unrelated save.
  // Errors are swallowed on purpose: a stale chip is better than a dead
  // screen for a refresh nothing on screen is waiting on. R-02: a
  // rejection re-fires once — the ordinary failure here is one transient
  // request, not an outage — then gives up silently.
  const refreshSetupStatus = React.useCallback(() => {
    setupApi
      .getSetupStatus()
      .then(setSetupStatus)
      .catch(() => setupApi.getSetupStatus().then(setSetupStatus).catch(() => {}));
  }, []);

  const save = async () => {
    setSaving(true);
    setSaveError(null);
    clearWriteDropped();
    try {
      const result = await api.putWorkspaceProviders(draft, etag);
      setDraft(result.providers);
      // The PUT response is the new BASELINE too — a save with nothing left
      // unsaved must not still read as dirty to the guard above or to a
      // second "Copy my changes" the person never asked for.
      setOriginal(result.providers);
      // The PUT response is the new loaded snapshot: saving a removal down to
      // zero rows puts the org in true legacy-open mode, and the banner's Add
      // owns the affirmative again.
      setLoadedEmpty((result.providers.git ?? []).length === 0);
      setEtag(result.etag);
      setNarrowed(result.sourcesNoLongerAdmitted > 0 ? result.sourcesNoLongerAdmitted : null);
      if (result.sourcesNoLongerAdmitted > 0) {
        toast.warning(PROVIDERS.SAVED_TOAST, { description: PROVIDERS.SAVED_NARROWED(result.sourcesNoLongerAdmitted) });
      } else {
        toast.success(PROVIDERS.SAVED_TOAST);
      }
    } catch (e) {
      // #483: a 401 is the sign-in dialog's to answer; once the person is
      // back, writeDropped says this save never went through.
      if (e instanceof HttpError && e.status === 401) return;
      if (e instanceof HttpError && e.status === 412) {
        setSavedElsewhere(true);
      } else if (e instanceof HttpError && e.status === 400) {
        // The server's own 400 (§7.1's PROVIDERS_400.* block), rendered
        // verbatim under SAVE_REFUSED_TITLE — never a console-authored
        // reword.
        setSaveError(e.message);
      } else {
        toast.error(PROVIDERS.SAVE_ERROR, { description: getErrorMessage(e) });
      }
    } finally {
      setSaving(false);
    }
  };

  // Any row the server is guaranteed to refuse withholds Save — on every tab,
  // because the PUT carries the whole document: a git row with no addresses is a
  // 400 whichever tab is open when Save is pressed, and there is nothing honest
  // to send. The row itself carries the reason (BASE_URLS_REQUIRED under its
  // textarea), so this is a withheld button with a visible cause, not a dead end.
  const invalidGitRow = (draft.git ?? []).some(gitRowInvalid);

  // #217 — the changed fields, as readable text (never the whole draft as
  // JSON): both "Copy my changes" on a 412 and the unsaved-navigation guard
  // below read off the same diff, so the guard can never fire on a draft the
  // banner would call clean.
  const changedLines = React.useMemo(() => readableDiff(original, draft), [original, draft]);
  useUnsavedGuard(changedLines.length > 0);
  // #483: the same lines, for "Copy my changes" on the signed-out bar/dialog.
  useRegisterUnsaved("providers", changedLines.length > 0, () => changedLines.join("\n"));

  const secretsPresent = setupStatus?.secrets.present ?? [];
  const githubApp = setupStatus?.secrets.github_app ?? false;
  const enforcement = setupStatus?.runner.ephemeral_disk_enforcement;

  return (
    <div className="mx-auto max-w-[900px] px-6 py-6">
      <PageHeader title={PROVIDERS.TITLE} description={PROVIDERS.LEAD} />

      {status === "forbidden" ? (
        <div className="mt-6">
          <OperatorOnlyHint />
        </div>
      ) : status === "error" ? (
        <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card">
          <EmptyState
            icon={AlertTriangle}
            title={PROVIDERS.FETCH_FAILED_TITLE}
            description={PROVIDERS.FETCH_FAILED_BODY}
            action={
              <Button variant="outline" size="sm" onClick={load}>
                {ACCESS_STATE.FETCH_FAILED_RETRY}
              </Button>
            }
          />
        </section>
      ) : status === "loading" ? (
        <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card">
          <TableSkeleton rows={3} cols={4} />
        </section>
      ) : (
        <section className="mt-6 space-y-4 rounded-xl border border-border bg-card p-5">
          <Segmented
            value={tab}
            onChange={setTab}
            options={[
              { value: "git", label: PROVIDERS.GIT_TITLE },
              { value: "storage", label: PROVIDERS.STORAGE_TAB },
              // C-UI (W4): rows from SetupStatus.harnesses, the mechanism
              // radio, the credential-source toggle. Present, not hidden —
              // see the file header.
              { value: "agents", label: AGENTS.AGENTS_TITLE },
            ]}
          />

          {/* F4-F3 (Appendix A V8): keep the draft mounted — the banner sits
              above the tabs rather than replacing them, so an edit typed
              moments before the 412 is still on screen and readable. #217:
              Copy my changes before Discard mine and reload (= load()) — a
              "Save over theirs" arm is still refused, a security document is
              never last-writer-wins from this banner. */}
          {savedElsewhere && <SavedElsewhereBanner changedLines={changedLines} onDiscard={load} />}
          {narrowed !== null && (
            <div className="rounded-lg border border-warning/30 bg-warning-subtle p-3 text-body text-warning">
              {PROVIDERS.SAVED_NARROWED(narrowed)}
            </div>
          )}
          {saveError && (
            <div className="rounded-lg border border-danger/30 bg-danger-subtle p-3 text-body text-danger">
              <b className="font-semibold">{PROVIDERS.SAVE_REFUSED_TITLE}</b>
              <p className="mt-0.5">{saveError}</p>
            </div>
          )}

          {tab === "git" && (
            <GitTab
              git={draft.git ?? []}
              onChange={(git) => setDraft((d) => ({ ...d, git }))}
              present={secretsPresent}
              githubApp={githubApp}
              operator={operator}
              loadedEmpty={loadedEmpty}
              patBrokerEnabled={draft.git_pat_broker_enabled ?? true}
              onStatusRefresh={refreshSetupStatus}
            />
          )}
          {tab === "storage" && (
            <StorageTab
              storage={draft.storage ?? {}}
              onChange={(storage) => setDraft((d) => ({ ...d, storage }))}
              enforcement={enforcement}
              operator={operator}
            />
          )}
          {/* Agents is its own resource (SiteConfig.agent_providers, its
              own GET/PUT) — a separate document from Git/Storage's
              WorkspaceProviders, so it fetches and saves itself (the
              UserDrivesCard precedent) rather than riding this screen's
              draft/save. Its own Save button is the tab's one teal — the
              shared one below is withheld while it's active. */}
          {tab === "agents" && (
            <AgentsTab
              /* Undefined, never `?? []`: the tab builds its whole PUT
                 body from this, so an older daemon's absent roster must not
                 read as an empty one and save `{agents: []}`, disabling every
                 agent. Absent is unknown (setup.ts's own rule). */
              harnesses={setupStatus?.harnesses}
              modelAccess={setupStatus?.model_access}
              operator={operator}
              /* The roster comes from this screen's /setup/status read, so
                 the tab's roster-unknown Retry has to re-fire that — its own
                 load() re-reads /agent-providers, which is not the read that
                 failed, and clicking it changed nothing. */
              onRetryRoster={load}
              onStatusRefresh={refreshSetupStatus}
            />
          )}
          {/* One teal button at a time (CONSOLE-RULES §6, prompt §4): in
              the legacy-open empty state the Git tab's own banner action is
              the state's one affirmative, so the screen's Save providers is
              withheld rather than doubling it. Withheld only while there is
              nothing to save — the loaded snapshot was empty and the draft
              still is: a draft emptied by Remove is a pending change (Save
              stays; the banner's Add steps down to outline — git-tab.tsx's
              `loadedEmpty`), and a first row added on a fresh install is one
              too (Save appears the moment the draft has a row). */}
          {tab !== "agents" && !(tab === "git" && loadedEmpty && (draft.git ?? []).length === 0) && (
            <div className="flex items-center justify-end gap-3 border-t border-border pt-4">
              {/* #217 — a disabled control states its reason BESIDE it, not
                  only in a title tooltip a keyboard or a phone never shows. */}
              {!operator && <OperatorOnlyHint />}
              {operator && changedLines.length > 0 && (
                <span className="mr-auto text-meta text-muted-foreground">{PROVIDERS_DRAFT.UNSAVED_MARKER}</span>
              )}
              {writeDropped && (
                <span role="status" className="text-meta text-warning">
                  {REAUTH_DIALOG.WRITE_DROPPED}
                </span>
              )}
              <Button disabled={!operator || saving || invalidGitRow} onClick={save}>
                {PROVIDERS.SAVE_CTA}
              </Button>
            </div>
          )}
        </section>
      )}
    </div>
  );
}
