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
import { Link } from "react-router-dom";
import { AlertTriangle, KeyRound, Loader2 } from "lucide-react";
import { credentials as credentialsApi, type CredentialInventory, type CredentialRow } from "../../lib/api/credentials";
import { HttpError } from "../../lib/api/core";
import { getErrorMessage, relativeTime, absoluteTime } from "../../lib/format";
import { useOperator, useSecurityOperator } from "../wardyn/operator-context";
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
import { INVENTORY, ERASE, ERASE_DATA } from "../wardyn/copy/credentials";
import { EraseDataDialog } from "./erase-data-dialog";
import { AdminMintedTokens } from "./credentials-minted-tokens";

// The server's canon 503 (credInventoryNoMeta, internal/api/credential_inventory.go)
// — its own empty card, no Retry, distinguished from every other read failure.
const NO_META_ERROR =
  "This deployment's secret store keeps no credential metadata, so there is nothing to list.";

function storeLabel(store: string): string {
  return INVENTORY.STORE[store] ?? store;
}

// A person's contiguous run of rows, in the order the inventory listed them —
// erase works per PERSON, not per credential (packet F
// §4 a colfoot: "The table is grouped by person because erase works per
// person"). The Person and Erase cells render once per group, with a rowSpan.
interface PersonGroup {
  person: string;
  email?: string;
  rows: CredentialRow[];
}
function groupByPerson(rows: CredentialRow[]): PersonGroup[] {
  const order: string[] = [];
  const groups = new Map<string, PersonGroup>();
  for (const r of rows) {
    let g = groups.get(r.person);
    if (!g) {
      g = { person: r.person, email: r.email, rows: [] };
      groups.set(r.person, g);
      order.push(r.person);
    }
    g.rows.push(r);
  }
  return order.map((p) => groups.get(p)!);
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
  // The by-scope erase (POST /people/{principal}/erasure) is the security tier's.
  const securityOperator = useSecurityOperator();
  const { status } = useShellSetupStatus();
  const [inv, setInv] = React.useState<CredentialInventory | null>(null);
  const [screenStatus, setStatus] = React.useState<ScreenStatus | "no_meta">("loading");
  const [errorMessage, setErrorMessage] = React.useState<string | undefined>(undefined);
  const [filter, setFilter] = React.useState<string | null>(null);
  const [eraseTarget, setEraseTarget] = React.useState<EraseTarget | null>(null);
  const [eraseDataOpen, setEraseDataOpen] = React.useState(false);

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
  const groups = React.useMemo(() => groupByPerson(filtered), [filtered]);
  // The footer's own store name: the EXTERNAL store present, if
  // any — the "Stored in" column (mixedStores) is the per-row distinction;
  // the footer's job is only to say where that store's OWN audit trail lives.
  const footerStore: "local" | "vault" | "key_vault" = stores.has("azurekv")
    ? "key_vault"
    : stores.has("vaultkv")
      ? "vault"
      : "local";

  return (
    <div className="mx-auto max-w-[1200px] px-6 py-6">
      <PageHeader
        title={INVENTORY.TITLE}
        description={INVENTORY.LEDE}
        actions={
          <>
            {securityOperator && (
              <Button variant="outline" onClick={() => setEraseDataOpen(true)}>
                {ERASE_DATA.BUTTON}
              </Button>
            )}
            <Button variant="outline" onClick={() => setEraseTarget("by-email")}>
              {INVENTORY.ERASE_BY_EMAIL}
            </Button>
          </>
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
                    <Link to="/admin/settings">{INVENTORY.OPEN_SETTINGS}</Link>
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
                    {groups.flatMap((g) =>
                      g.rows.map((r, i) => (
                        <TableRow key={`${r.person}-${r.provider}`}>
                          {i === 0 && (
                            <TableCell rowSpan={g.rows.length} className="whitespace-nowrap align-top font-medium">
                              {g.email || g.person}
                              {g.email && <span className="ml-1.5 text-xs text-muted-foreground">{g.person}</span>}
                            </TableCell>
                          )}
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
                          {i === 0 && (
                            <TableCell rowSpan={g.rows.length} className="align-top">
                              <Button
                                size="sm"
                                variant="outline"
                                onClick={() => setEraseTarget({ principal: g.person, label: g.email || g.person })}
                              >
                                {INVENTORY.ERASE_ROW}
                              </Button>
                            </TableCell>
                          )}
                        </TableRow>
                      )),
                    )}
                  </TableBody>
                </Table>
              </div>

              <p className="mt-3 text-xs text-muted-foreground">
                {footerStore === "local" ? INVENTORY.FOOTER_LOCAL : INVENTORY.FOOTER(footerStore)}{" "}
                <Link to="/admin/audit" className="text-info hover:underline">
                  {INVENTORY.OPEN_AUDIT}
                </Link>
              </p>
            </div>
          )}
        </div>
      )}

      {/* Another admin-only read: only once the screen has resolved as allowed. */}
      {screenStatus !== "forbidden" && screenStatus !== "loading" && <AdminMintedTokens />}

      <EraseDataDialog
        open={eraseDataOpen}
        retentionLine={eraseRetentionLine(status?.credential_storage)}
        onOpenChange={setEraseDataOpen}
        onErased={load}
      />
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
      // §5 (f): a >=500 never finished, so it gets its OWN sentence — the
      // erase may have partly landed, unlike a 4xx refusal, whose server
      // sentence (route-neutral 422, or the operator-namespace 400) is shown
      // as sent.
      setError(e instanceof HttpError && e.status >= 500 ? ERASE.FAILED(label) : getErrorMessage(e));
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
            {/* §5 (d)'s order: the count, then Key Vault's recoverable-days
                line (when it applies), then the audit line last. */}
            {result.count > 0 && result.store && !result.purged && !!result.recoverable_days && (
              <p className="text-body text-muted-foreground">{ERASE.KEY_VAULT_RECOVERABLE(result.recoverable_days)}</p>
            )}
            {result.count > 0 && <p className="text-body text-muted-foreground">{ERASE.DONE_AUDIT}</p>}
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
