/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin view → Credentials (design F-1, packet F §4/§5): who holds a
// credential for each model provider, and erasing a person's whole namespace
// for offboarding. Both admin tiers read and act (K5-A, GET
// /model-providers/credentials and DELETE /people/{principal}/credentials —
// internal/api/credential_inventory.go, /credential_erase.go); a member is
// refused at the route (the generic admin-view refusal covers it, same as
// every other /admin/* screen).
import * as React from "react";
import { AlertTriangle, KeyRound, Loader2 } from "lucide-react";
import { credentials as credentialsApi, type CredentialInventory, type CredentialRow } from "../../lib/api/credentials";
import { getErrorMessage, relativeTime, absoluteTime } from "../../lib/format";
import { useOperator } from "../wardyn/operator-context";
import { useShellSetupStatus } from "../wardyn/model-access-context";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../ui/dialog";
import { Field } from "../wardyn/form-primitives";
import { Chip } from "../wardyn/primitives";
import { EmptyState, ErrorState, TableSkeleton, loadFailStatus, type ScreenStatus } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { SECURITY_ONLY_REASON } from "../wardyn/copy";
import { MODEL_PROVIDERS } from "../../lib/model-providers-copy";
import { INVENTORY, ERASE } from "../wardyn/copy/credentials";

// The server's canon 503 (credInventoryNoMeta, internal/api/credential_inventory.go)
// — its own empty card, no Retry, distinguished from every other read failure.
const NO_META_ERROR =
  "This deployment's secret store keeps no credential metadata, so there is nothing to list.";

function storeLabel(store: string): string {
  return INVENTORY.STORE[store] ?? store;
}

// eraseRetentionLine is the erase confirm's retention sentence (design F-6),
// keyed off the SAME deployment-wide credential_storage /setup/status reports
// (F-3) — the erase route covers the whole namespace, not one row's store.
function eraseRetentionLine(storage: string | undefined): string {
  switch (storage) {
    case "vault":
      return ERASE.RETENTION_VAULT;
    case "key_vault":
      return ERASE.RETENTION_KEY_VAULT;
    default:
      return ERASE.RETENTION_LOCAL;
  }
}

type EraseTarget = { principal: string; label: string } | "by-email";

export function CredentialsScreen() {
  const operator = useOperator();
  const { status } = useShellSetupStatus();
  const [inv, setInv] = React.useState<CredentialInventory | null>(null);
  const [screenStatus, setStatus] = React.useState<ScreenStatus | "no_meta">("loading");
  const [errorMessage, setErrorMessage] = React.useState<string | undefined>(undefined);
  const [filter, setFilter] = React.useState<string | null>(null);
  const [eraseTarget, setEraseTarget] = React.useState<EraseTarget | null>(null);

  const load = React.useCallback(() => {
    setStatus("loading");
    credentialsApi
      .listInventory()
      .then((d) => {
        setInv(d);
        setStatus("ready");
      })
      .catch((e) => {
        const msg = getErrorMessage(e);
        setErrorMessage(msg);
        setStatus(msg === NO_META_ERROR ? "no_meta" : loadFailStatus(e));
      });
  }, []);
  React.useEffect(load, [load]);

  const rows = inv?.credentials ?? [];
  const providerNames = React.useMemo(() => {
    const out: Record<string, string> = {};
    for (const r of inv?.credentials ?? []) if (r.provider_name) out[r.provider] = r.provider_name;
    return out;
  }, [inv]);
  const byProvider = inv?.counts.by_provider ?? {};
  const noProvidersConfigured = Object.keys(byProvider).length === 0;
  const filtered = filter ? rows.filter((r) => r.provider === filter) : rows;
  const stores = new Set(rows.map((r) => r.store));
  const mixedStores = stores.size > 1;

  const eraseLabelFor = (r: CredentialRow) => r.email || r.person;

  return (
    <div className="mx-auto max-w-[1200px] px-6 py-6">
      <PageHeader
        title={INVENTORY.TITLE}
        description={INVENTORY.LEDE}
        actions={
          <Button variant="outline" onClick={() => setEraseTarget("by-email")}>
            {INVENTORY.ERASE_BY_EMAIL}
          </Button>
        }
      />

      {screenStatus === "forbidden" ? (
        <p className="mt-6 text-sm text-muted-foreground">{SECURITY_ONLY_REASON}</p>
      ) : (
        <div className="overflow-hidden rounded-xl border border-border bg-card">
          {screenStatus === "loading" ? (
            <TableSkeleton rows={5} cols={5} />
          ) : screenStatus === "no_meta" ? (
            <EmptyState icon={AlertTriangle} title="Something went wrong" description={errorMessage} />
          ) : screenStatus === "error" ? (
            <ErrorState onRetry={load} />
          ) : noProvidersConfigured ? (
            <EmptyState
              icon={KeyRound}
              title={MODEL_PROVIDERS.EMPTY_TITLE}
              description={INVENTORY.NO_PROVIDERS_BODY}
              action={
                operator ? (
                  <Button variant="outline" size="sm" asChild>
                    <a href="/admin/settings">{INVENTORY.OPEN_SETTINGS}</a>
                  </Button>
                ) : undefined
              }
            />
          ) : rows.length === 0 ? (
            <EmptyState icon={KeyRound} title={INVENTORY.EMPTY_TITLE} description={INVENTORY.EMPTY_BODY} />
          ) : (
            <div className="p-4">
              <p className="text-xs text-muted-foreground">{INVENTORY.SUMMARY(inv!.counts.people, inv!.counts.credentials)}</p>
              <div className="mt-3 flex flex-wrap gap-1.5">
                <button
                  type="button"
                  onClick={() => setFilter(null)}
                  className={`rounded-full border px-2.5 py-0.5 text-xs ${
                    filter === null ? "border-primary text-foreground" : "border-border text-muted-foreground"
                  }`}
                >
                  {INVENTORY.FILTER_ALL}
                </button>
                {Object.entries(byProvider).map(([id, n]) => (
                  <button
                    key={id}
                    type="button"
                    onClick={() => setFilter(id)}
                    className={`rounded-full border px-2.5 py-0.5 text-xs ${
                      filter === id ? "border-primary text-foreground" : "border-border text-muted-foreground"
                    }`}
                  >
                    {providerNames[id] ?? id} · {n}
                  </button>
                ))}
              </div>

              <div className="mt-3 overflow-x-auto">
                <Table>
                  <TableHeader>
                    <TableRow className="hover:bg-transparent">
                      <TableHead>{INVENTORY.COL_PERSON}</TableHead>
                      <TableHead>{INVENTORY.COL_PROVIDER}</TableHead>
                      <TableHead>{INVENTORY.COL_STATE}</TableHead>
                      {mixedStores && <TableHead>{INVENTORY.COL_STORE}</TableHead>}
                      <TableHead>{INVENTORY.COL_ADDED}</TableHead>
                      <TableHead>{INVENTORY.COL_LAST_USED}</TableHead>
                      <TableHead />
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {filtered.map((r) => (
                      <TableRow key={`${r.person}-${r.provider}`}>
                        <TableCell className="whitespace-nowrap font-medium">
                          {r.email || r.person}
                          {r.email && <span className="ml-1.5 text-xs text-muted-foreground">{r.person}</span>}
                        </TableCell>
                        <TableCell>{providerNames[r.provider] ?? r.provider}</TableCell>
                        <TableCell>
                          <Chip
                            tone={r.state === "expired" ? "warning" : "neutral"}
                            title={r.state === "expired" ? INVENTORY.EXPIRED_HINT : undefined}
                          >
                            {r.state === "expired" ? INVENTORY.STATE_EXPIRED : INVENTORY.STATE_STORED}
                          </Chip>
                        </TableCell>
                        {mixedStores && <TableCell className="text-muted-foreground">{storeLabel(r.store)}</TableCell>}
                        <TableCell className="text-muted-foreground" title={absoluteTime(r.added_at)}>
                          {relativeTime(r.added_at)}
                        </TableCell>
                        <TableCell
                          className="text-muted-foreground"
                          title={r.last_used_at ? absoluteTime(r.last_used_at) : undefined}
                        >
                          {r.last_used_at ? relativeTime(r.last_used_at) : INVENTORY.NEVER_USED}
                        </TableCell>
                        <TableCell>
                          <Button
                            size="sm"
                            variant="outline"
                            onClick={() => setEraseTarget({ principal: r.person, label: eraseLabelFor(r) })}
                          >
                            {INVENTORY.ERASE_ROW}
                          </Button>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>

              <p className="mt-3 text-xs text-muted-foreground">
                {mixedStores
                  ? null
                  : stores.has("azurekv")
                    ? INVENTORY.FOOTER("key_vault")
                    : stores.has("vaultkv")
                      ? INVENTORY.FOOTER("vault")
                      : INVENTORY.FOOTER_LOCAL}
              </p>
            </div>
          )}
        </div>
      )}

      <EraseDialog
        target={eraseTarget}
        credentialStorage={status?.credential_storage}
        onOpenChange={(open) => !open && setEraseTarget(null)}
        onErased={load}
      />
    </div>
  );
}

function EraseDialog({
  target,
  credentialStorage,
  onOpenChange,
  onErased,
}: {
  target: EraseTarget | null;
  credentialStorage?: string;
  onOpenChange: (open: boolean) => void;
  onErased: () => void;
}) {
  const [byEmailValue, setByEmailValue] = React.useState("");
  const [confirmValue, setConfirmValue] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const [result, setResult] = React.useState<{ count: number; store?: string; purged?: boolean; recoverable_days?: number } | null>(
    null,
  );
  const [erasedLabel, setErasedLabel] = React.useState("");

  React.useEffect(() => {
    if (target) {
      setByEmailValue("");
      setConfirmValue("");
      setError("");
      setResult(null);
      setErasedLabel("");
    }
  }, [target]);

  if (!target) return null;
  const listed = target !== "by-email";
  const label = listed ? target.label : byEmailValue.trim();
  const canConfirm = listed ? confirmValue.trim().toLowerCase() === target.label.toLowerCase() : byEmailValue.trim().length > 0;

  const doErase = async () => {
    setBusy(true);
    setError("");
    try {
      const principal = listed ? target.principal : byEmailValue.trim();
      const res = await credentialsApi.erase(principal);
      setResult(res);
      setErasedLabel(label);
    } catch (e) {
      setError(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const close = (erased: boolean) => {
    onOpenChange(false);
    if (erased) onErased();
  };

  return (
    <Dialog open onOpenChange={(o) => !o && close(!!result)}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{listed ? ERASE.TITLE(target.label) : ERASE.BY_EMAIL_TITLE}</DialogTitle>
        </DialogHeader>

        {result ? (
          <>
            <p className="text-body text-foreground">
              {result.count === 0 ? ERASE.DONE_NONE(erasedLabel) : ERASE.DONE(result.count, erasedLabel)}
            </p>
            {result.count > 0 && <p className="text-body text-muted-foreground">{ERASE.DONE_AUDIT}</p>}
            {result.count > 0 && result.store && !result.purged && !!result.recoverable_days && (
              <p className="text-body text-muted-foreground">{ERASE.KEY_VAULT_RECOVERABLE(result.recoverable_days)}</p>
            )}
            <DialogFooter>
              <Button onClick={() => close(true)}>{ERASE.CLOSE}</Button>
            </DialogFooter>
          </>
        ) : (
          <>
            {!listed && (
              <Field label={ERASE.FIELD} htmlFor="erase-principal" hint={ERASE.HINT}>
                <Input
                  id="erase-principal"
                  autoFocus
                  autoComplete="off"
                  value={byEmailValue}
                  onChange={(e) => setByEmailValue(e.target.value)}
                />
              </Field>
            )}
            <p className="text-body text-muted-foreground">{ERASE.BODY}</p>
            <p className="text-body text-muted-foreground">{eraseRetentionLine(credentialStorage)}</p>
            {listed && (
              <Field label={ERASE.CONFIRM_LABEL(target.label)} htmlFor="erase-confirm">
                <Input
                  id="erase-confirm"
                  autoFocus
                  autoComplete="off"
                  value={confirmValue}
                  onChange={(e) => setConfirmValue(e.target.value)}
                />
              </Field>
            )}
            {error && (
              <p role="alert" className="text-body text-danger">
                {error}
              </p>
            )}
            <DialogFooter>
              <Button variant="ghost" disabled={busy} onClick={() => close(false)}>
                {ERASE.CANCEL}
              </Button>
              <Button variant="destructive" disabled={busy || !canConfirm} onClick={doErase}>
                {busy && <Loader2 className="size-3.5 animate-spin" />}
                {ERASE.CONFIRM}
              </Button>
            </DialogFooter>
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}
