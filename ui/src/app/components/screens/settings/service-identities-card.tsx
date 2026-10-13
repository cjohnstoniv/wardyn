/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Your account: "Service identities I sponsor" (#1508, mock M-SVC v2). An admin creates a service identity and
// names its sponsor; the sponsor, signed in, mints its tokens here (the shared token dialog shows each once),
// and lists and revokes them. Creating, suspending and retiring one stay the API's and the CLI's. The card
// renders nothing when the person sponsors none, and where there is no person to sponsor (local mode).
import * as React from "react";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { serviceIdentities } from "../../../lib/api/service-identities";
import { getErrorMessage } from "../../../lib/format";
import type { ApiToken, ServiceIdentity, ServiceIdentityState } from "../../../lib/types/service-identities";
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
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../ui/table";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { MINTED } from "../../wardyn/copy/credentials";
import { inWords, SERVICES as T } from "../../wardyn/copy/service-tokens";
import { Chip } from "../../wardyn/primitives";
import { ErrorState, TableSkeleton } from "../../wardyn/states";
import { TokenDialog } from "./token-dialog";

type Load = "loading" | "ready" | "error" | "unavailable" | "hidden";
type Panel = { load: "loading" | "ready" | "error"; rows: ApiToken[] };

const CHIP: Record<ServiceIdentityState, { label: string; tone: "success" | "warning" | "neutral" }> = {
  active: { label: T.STATE_ACTIVE, tone: "success" },
  suspended: { label: T.STATE_SUSPENDED, tone: "warning" },
  retired: { label: T.STATE_RETIRED, tone: "neutral" },
  sponsor_inactive: { label: T.STATE_SPONSOR_INACTIVE, tone: "warning" },
};
const HINT: Partial<Record<ServiceIdentityState, string>> = {
  suspended: T.HINT_SUSPENDED,
  retired: T.HINT_RETIRED,
  sponsor_inactive: T.HINT_SPONSOR_INACTIVE,
};

function loadFailure(e: unknown): Load {
  if (e instanceof HttpError) {
    if (e.reason === "needs_sso" || (e.status === 403 && e.reason === "api_token_no_human")) return "hidden";
    if (e.reason === "people_store_unavailable") return "unavailable";
  }
  return "error";
}

export function ServiceIdentitiesCard() {
  const [state, setState] = React.useState<Load>("loading");
  const [items, setItems] = React.useState<ServiceIdentity[]>([]);
  const [open, setOpen] = React.useState<Record<string, boolean>>({});
  const [panels, setPanels] = React.useState<Record<string, Panel>>({});
  const [minting, setMinting] = React.useState<string | null>(null);
  const [revoking, setRevoking] = React.useState<{ svc: string; token: ApiToken } | null>(null);
  const [revokeBusy, setRevokeBusy] = React.useState(false);
  const card = React.useRef<HTMLElement>(null);
  // The row the confirmation was opened from: the dialog stays mounted after it closes, with `revoking` cleared.
  const revokedFrom = React.useRef("");

  const load = React.useCallback(() => {
    serviceIdentities
      .listMine()
      .then((rows) => {
        setItems(rows);
        setState("ready");
      })
      .catch((e: unknown) => setState(loadFailure(e)));
  }, []);
  React.useEffect(load, [load]);

  const loadTokens = React.useCallback((name: string) => {
    setPanels((p) => ({ ...p, [name]: { load: "loading", rows: p[name]?.rows ?? [] } }));
    serviceIdentities
      .listTokens(name)
      .then((rows) => setPanels((p) => ({ ...p, [name]: { load: "ready", rows } })))
      .catch(() => setPanels((p) => ({ ...p, [name]: { load: "error", rows: [] } })));
  }, []);

  const toggle = (name: string) => {
    const next = !open[name];
    setOpen((o) => ({ ...o, [name]: next }));
    if (next) loadTokens(name);
  };

  const revoke = async () => {
    if (!revoking || revokeBusy) return;
    setRevokeBusy(true);
    try {
      await serviceIdentities.revokeToken(revoking.svc, revoking.token.id);
      toast.success(MINTED.REVOKED_TOAST);
      loadTokens(revoking.svc);
    } catch (e) {
      toast.error(getErrorMessage(e));
    } finally {
      setRevoking(null);
      setRevokeBusy(false);
    }
  };

  if (state === "hidden" || (state === "ready" && items.length === 0 && !minting)) return null;

  const active = items.filter((s) => s.state === "active").length;
  const focusCard = (selector: string) => {
    const el = card.current?.querySelector<HTMLElement>(selector) ?? card.current?.querySelector<HTMLElement>("h3 button");
    el?.focus();
  };

  return (
    <CollapsibleCard
      ref={card}
      title={T.TITLE}
      summary={state === "ready" && items.length > 0 ? T.SUMMARY(active, items.length) : undefined}
      testId="service-identities-card"
    >
      {state === "loading" ? (
        <div role="status" aria-label={T.LOADING}>
          <span className="sr-only">{T.LOADING}</span>
          <TableSkeleton rows={2} cols={3} />
        </div>
      ) : state === "error" ? (
        <div role="group" aria-label={T.LOAD_ERROR_LABEL}>
          <ErrorState as="h4" onRetry={() => {
            setState("loading");
            load();
          }} />
        </div>
      ) : state === "unavailable" ? (
        <div role="alert" className="rounded-md border border-border bg-muted px-3 py-2 text-sm">
          <strong className="font-medium">{T.UNAVAILABLE_TITLE}</strong>
          <br />
          {T.UNAVAILABLE_BODY}
        </div>
      ) : (
        <div className="space-y-3">
          <p className="text-xs text-muted-foreground">{T.LEDE}</p>
          <ul className="space-y-2">
            {items.map((s) => (
              <ServiceRow
                key={s.name}
                si={s}
                open={!!open[s.name]}
                panel={panels[s.name]}
                onToggle={() => toggle(s.name)}
                onMint={() => setMinting(s.name)}
                onRetryTokens={() => loadTokens(s.name)}
                onRevoke={(token) => {
                  revokedFrom.current = s.name;
                  setRevoking({ svc: s.name, token });
                }}
              />
            ))}
          </ul>
        </div>
      )}

      {minting && (
        <TokenDialog
          kind="mint"
          service={minting}
          initialName=""
          submit={(body) => serviceIdentities.mintToken(minting, body)}
          onCreated={() => {
            setOpen((o) => ({ ...o, [minting]: true }));
            loadTokens(minting);
          }}
          onRefused={async (e) => {
            if (e.reason === "service_identity_not_found") {
              load();
              return true;
            }
            // Inactive: the row tells the new state. Final when it is gone, as when the sponsor changed.
            const rows = await serviceIdentities.listMine().catch(() => null);
            if (!rows) return false;
            setItems(rows);
            return !rows.some((r) => r.name === minting);
          }}
          onClose={(created) => {
            setMinting(null);
            if (created) toast.success(T.MINTED_TOAST);
          }}
          restoreFocus={() => focusCard(`[data-mint="${minting}"]`)}
        />
      )}

      <AlertDialog open={!!revoking} onOpenChange={(o) => !o && !revokeBusy && setRevoking(null)}>
        <AlertDialogContent
          onCloseAutoFocus={(e) => {
            e.preventDefault();
            focusCard(`[data-toggle="${revokedFrom.current}"]`);
          }}
        >
          <AlertDialogHeader>
            <AlertDialogTitle>{revoking ? T.REVOKE_TITLE(revoking.token.name, revoking.svc) : ""}</AlertDialogTitle>
            <AlertDialogDescription>{revoking ? T.REVOKE_BODY(revoking.svc) : ""}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={revokeBusy}>{MINTED.REVOKE_CANCEL}</AlertDialogCancel>
            <AlertDialogAction
              className={buttonVariants({ variant: "destructive" })}
              disabled={revokeBusy}
              onClick={(e) => {
                e.preventDefault();
                void revoke();
              }}
            >
              {MINTED.REVOKE_CONFIRM}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </CollapsibleCard>
  );
}

function ServiceRow({
  si,
  open,
  panel,
  onToggle,
  onMint,
  onRetryTokens,
  onRevoke,
}: {
  si: ServiceIdentity;
  open: boolean;
  panel: Panel | undefined;
  onToggle: () => void;
  onMint: () => void;
  onRetryTokens: () => void;
  onRevoke: (t: ApiToken) => void;
}) {
  const usable = si.state === "active";
  const chip = CHIP[si.state] ?? { label: si.state, tone: "neutral" as const };
  const hint = HINT[si.state];
  const hintId = `svc-hint-${si.name}`;
  const panelId = `svc-tokens-${si.name}`;
  return (
    <li className="rounded-lg border border-border px-3 py-2" data-svc={si.name}>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <p className="flex flex-wrap items-center gap-2 text-sm font-medium text-foreground">
            <span className="font-mono text-xs">{si.name}</span>
            <Chip tone={chip.tone} dot mono={!(si.state in CHIP)}>
              {chip.label}
            </Chip>
          </p>
          {si.description && <p className="text-xs text-muted-foreground">{si.description}</p>}
          {hint && (
            <p id={hintId} className="text-xs text-muted-foreground">
              {hint}
            </p>
          )}
        </div>
        <div className="flex shrink-0 gap-2">
          {/* A button that cannot act stays focusable and says why beside it (CONSOLE-RULES §8). */}
          <Button
            type="button"
            variant="outline"
            size="sm"
            data-mint={si.name}
            aria-disabled={!usable}
            aria-describedby={!usable && hint ? hintId : undefined}
            aria-label={T.MINT_LABEL(si.name)}
            className={usable ? undefined : "opacity-50"}
            onClick={() => usable && onMint()}
          >
            {T.MINT}
          </Button>
          <Button
            type="button"
            variant="outline"
            size="sm"
            data-toggle={si.name}
            aria-expanded={open}
            aria-controls={panelId}
            aria-label={open ? T.HIDE_TOKENS_LABEL(si.name) : T.SHOW_TOKENS_LABEL(si.name)}
            onClick={onToggle}
          >
            {open ? T.HIDE_TOKENS : T.SHOW_TOKENS}
          </Button>
        </div>
      </div>
      {open && (
        <div id={panelId} className="mt-3 border-t border-border pt-3">
          {!panel || panel.load === "loading" ? (
            <div role="status" aria-label={T.LOADING_TOKENS}>
              <span className="sr-only">{T.LOADING_TOKENS}</span>
              <TableSkeleton rows={2} cols={3} />
            </div>
          ) : panel.load === "error" ? (
            <div role="group" aria-label={T.TABLE_LABEL(si.name)}>
              <ErrorState as="h4" onRetry={onRetryTokens} />
            </div>
          ) : panel.rows.length === 0 ? (
            <p className="text-body text-muted-foreground">
              {T.NO_TOKENS}
              {usable ? ` ${T.NO_TOKENS_MINT(si.name)}` : ""}
            </p>
          ) : (
            <Table aria-label={T.TABLE_LABEL(si.name)}>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>{T.COL_TOKEN}</TableHead>
                  <TableHead>{T.COL_EXPIRES}</TableHead>
                  <TableHead>{T.COL_LAST_USED}</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {panel.rows.map((t) => (
                  <TokenRow key={t.id} t={t} svc={si.name} onRevoke={onRevoke} />
                ))}
              </TableBody>
            </Table>
          )}
        </div>
      )}
    </li>
  );
}

function TokenRow({ t, svc, onRevoke }: { t: ApiToken; svc: string; onRevoke: (t: ApiToken) => void }) {
  const expired = !!t.expires_at && Date.parse(t.expires_at) <= Date.now();
  const live = !t.revoked_at && !expired;
  return (
    <TableRow>
      <TableCell>
        <span className="flex flex-wrap items-center gap-2">
          <span className="font-mono text-xs">{t.name}</span>
          {t.revoked_at ? <Chip tone="neutral" dot>{T.REVOKED}</Chip> : expired ? <Chip tone="neutral" dot>{T.EXPIRED}</Chip> : null}
        </span>
        <span className="block text-xs text-muted-foreground">{T.CREATED(inWords(t.created_at))}</span>
      </TableCell>
      <TableCell className="text-muted-foreground">
        {t.revoked_at
          ? T.REVOKED_AT(inWords(t.revoked_at))
          : t.expires_at
            ? expired
              ? T.EXPIRED_AT(inWords(t.expires_at))
              : inWords(t.expires_at)
            : T.NEVER}
      </TableCell>
      <TableCell className="text-muted-foreground">{t.last_used_at ? inWords(t.last_used_at) : T.NEVER}</TableCell>
      <TableCell>
        {live && (
          <Button type="button" size="sm" variant="outline" aria-label={T.REVOKE_LABEL(t.name, svc)} onClick={() => onRevoke(t)}>
            {T.REVOKE}
          </Button>
        )}
      </TableCell>
    </TableRow>
  );
}
