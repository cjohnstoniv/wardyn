/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Settings card: the runner placement rule `require_managed_device`, one switch. Turning on asks first (a new run
// on a Personal computer is then refused); turning off is one click, since it loosens. Both are
// PUT /runners/settings naming only this field, so `enabled` is never rewritten, and the switch always shows
// the stored answer the PUT returns, never the click. Super-admin only, by being mounted from AdminSettingsScreen.
import * as React from "react";
import { Loader2, RotateCw } from "lucide-react";
import { HttpError } from "../../../lib/api/core";
import { runners as runnersApi } from "../../../lib/api/runners";
import { getErrorMessage } from "../../../lib/format";
import type { RunnerSettings } from "../../../lib/types";
import { Button } from "../../ui/button";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { RUNNERS } from "../../wardyn/copy/runners";
import { Switch } from "../../wardyn/form-primitives";
import { ConfirmDialog } from "../runners/confirm-dialog";

// A 403 means the viewer's role changed after the card loaded; the card says so in its own words.
const refusedAs = (e: unknown): unknown => (e instanceof HttpError && e.status === 403 ? new Error(RUNNERS.MANAGED_NOT_SUPER) : e);
const notSuper = (e: unknown): never => {
  throw refusedAs(e);
};

export function RunnersSettingsCard() {
  const [state, setState] = React.useState<"loading" | "ready" | "failed">("loading");
  const [stored, setStored] = React.useState<RunnerSettings>({ enabled: false, require_managed_device: false });
  const [confirming, setConfirming] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [offError, setOffError] = React.useState("");

  const load = React.useCallback(async () => {
    setState("loading");
    try {
      setStored(await runnersApi.settings());
      setState("ready");
    } catch {
      setState("failed");
    }
  }, []);
  React.useEffect(() => {
    void load();
  }, [load]);

  const turnOff = async () => {
    setBusy(true);
    setOffError("");
    try {
      setStored(await runnersApi.setRequireManagedDevice(false));
    } catch (e) {
      setOffError(getErrorMessage(refusedAs(e)));
    } finally {
      setBusy(false);
    }
  };

  const on = !!stored.require_managed_device;
  return (
    <CollapsibleCard title={RUNNERS.MANAGED_CARD_TITLE} testId="runners-settings-card">
      {state === "loading" && <Loader2 className="mt-4 size-4 animate-spin text-muted-foreground" />}
      {state === "failed" && (
        <div className="mt-4 flex flex-wrap items-center gap-2">
          <p className="text-sm text-muted-foreground">{RUNNERS.MANAGED_LOAD_FAILED}</p>
          <Button variant="outline" size="sm" onClick={() => void load()}>
            <RotateCw className="size-3.5" />
            {RUNNERS.MANAGED_RETRY}
          </Button>
        </div>
      )}
      {state === "ready" && (
        <div className="mt-3 flex items-start gap-3">
          <Switch
            checked={on}
            disabled={busy}
            label={RUNNERS.MANAGED_LABEL}
            describedBy="runners-managed-hint"
            className="mt-0.5"
            onChange={(next) => (next ? setConfirming(true) : void turnOff())}
          />
          <div className="min-w-0 space-y-1">
            <p className="text-body font-medium text-foreground">{RUNNERS.MANAGED_LABEL}</p>
            <p id="runners-managed-hint" className="text-meta text-muted-foreground">
              {RUNNERS.MANAGED_HINT}
            </p>
            <p className="text-meta text-muted-foreground">{RUNNERS.MANAGED_HONEST}</p>
            {stored.enabled === false && <p className="text-meta text-muted-foreground">{RUNNERS.MANAGED_RUNNERS_OFF}</p>}
            {offError && (
              <p role="alert" className="mt-2 text-body text-danger">
                <span className="font-medium">{RUNNERS.MANAGED_REFUSED_TITLE}</span>
                <br />
                {offError}
              </p>
            )}
          </div>
        </div>
      )}
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title={RUNNERS.MANAGED_CONFIRM_TITLE}
        body={[RUNNERS.MANAGED_CONFIRM_BODY]}
        confirmLabel={RUNNERS.MANAGED_CONFIRM}
        destructive={false}
        errorTitle={RUNNERS.MANAGED_REFUSED_TITLE}
        onConfirm={() =>
          runnersApi
            .setRequireManagedDevice(true)
            .then(setStored)
            .catch(notSuper)
        }
        onDone={() => {}}
      />
    </CollapsibleCard>
  );
}
