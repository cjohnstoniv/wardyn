/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Your account (M-5, #636 — the settings split, §4.3): a person's OWN
// connections and keys, reached at /account: Your model connections (#541,
// User view only), Azure DevOps (a personal connection, #386) and Your SSH
// keys.
//
// Nothing here belongs to the deployment — Host, the admin's Model providers
// list, Providers and User drives all stayed in Admin Settings
// (admin-settings-screen.tsx), and with Host gone this page has no consumer
// left for GET /site-config (operatorOnly, R1), so this screen never fetches it.
//
// /ssh-keys is gone (deleted with no alias); SshKeysPane is mounted here
// instead, exactly as it always rendered on this route.
//
// #1200 compact cards (owner-approved mock): every card here collapses to a
// one-line summary and expands on click, none
// open by default — admin-settings-screen.tsx's own header comment has the
// full reasoning.
import * as React from "react";
import { setup as setupApi } from "../../../lib/api/setup";
import type { SetupStatus } from "../../../lib/types";
import { PageHeader } from "../../wardyn/page-header";
import { YOUR_ACCOUNT } from "../../wardyn/copy/console-view";
import { useShellSetupStatus } from "../../wardyn/model-access-context";
import { ModelConnectionsCard } from "./model-connections-card";
import { useConsoleMode } from "../../wardyn/console-view";
import { AdoConnectionCard } from "./ado-connection";
import { MyComponentsCard } from "./my-components-card";
import { SshKeysPane } from "../ssh-keys";
import { ErrorState, TableSkeleton } from "../../wardyn/states";

export function YourAccountScreen() {
  const adminView = useConsoleMode() === "admin";
  const { status: modelStatus, refresh: refreshModels } = useShellSetupStatus();
  const [state, setState] = React.useState<"loading" | "error" | "ready">("loading");
  const [status, setStatus] = React.useState<SetupStatus | null>(null);

  const load = React.useCallback(() => {
    setupApi
      .getSetupStatus()
      .then((s) => {
        setStatus(s);
        setState("ready");
      })
      .catch(() => setState("error"));
  }, []);
  React.useEffect(load, [load]);

  return (
    <div className="mx-auto w-full max-w-[900px] px-6 py-8">
      <PageHeader title="Your account" description={YOUR_ACCOUNT.LEDE} />
      {state === "loading" && <TableSkeleton />}
      {state === "error" && <ErrorState onRetry={load} />}
      {state === "ready" && status && (
        <div className="space-y-2">
          {/* #541 (§5.4, packet MP-D): every person's own model-provider
              credentials, User view only — an admin reaches it by switching
              to Member view. Renders nothing with no provider block. */}
          {!adminView && <ModelConnectionsCard status={modelStatus ?? status} onChanged={refreshModels} />}
          {/* #386, Q9: a personal connection. Renders nothing with no Azure
              DevOps row configured. */}
          <AdoConnectionCard status={status} onChanged={load} />
          {/* #1914: components a person saved to reuse; absent when they may
              not define any and have none. */}
          <MyComponentsCard secretsPath={adminView ? "/admin/secrets" : "/secrets"} />
          <SshKeysPane heading="h3" />
        </div>
      )}
    </div>
  );
}
