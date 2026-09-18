# Independent cross-review — 2026-09-18

Review lane: lifecycle agent. Read-only source review; no product changes.

## Audit fallback backup guidance — b2042f52

No blocking correctness issue found.

- `NewAuditSpool` reads the consumed cursor and counts the remaining spool and quarantine lines at startup.
- `seedSpoolCursor` accepts a bounded offset with a matching content fingerprint. Missing, unreadable, malformed, out-of-range, or mismatched cursors return zero. It does not check any database snapshot identifier.
- `Drain` persists cursor progress after successful database writes, compacts or truncates consumed bytes, and fsyncs quarantine before retiring an irrecoverable event. Replay is at-least-once; quarantine is not automatically replayed.
- Therefore undrained and quarantined events can be absent from Postgres, and a newer cursor combined with an older database snapshot can skip absent events. The new matched-recovery-set and writer-quiescence instructions correctly describe that risk.
- Compose maps `/data/audit` to its project-prefixed audit volume and places the spool there. The Helm template puts the default spool in `/tmp` on an `emptyDir`, or on the persistence PVC when enabled. The backup/storage statements match those templates.
- This review verifies documentation against code. It does not independently establish an operator's backups are consistent or prove a production restore.

Inspected `internal/api/auditspool.go` (`NewAuditSpool`, `Drain`, compaction and quarantine handling), `internal/api/auditspool_cursor.go` (`writeSpoolCursor`, `seedSpoolCursor`), and the Compose/Helm volume and environment configuration.

## SSH revocation guidance — 8da1885b

No blocking correctness issue found.

- SSH-key routes are in the human/API-token authenticated group. Registration stores an independent key record; it is not linked to the creating API token for later revocation.
- `handleRevokeSessions` changes session cutoffs and revokes matching API tokens. It does not remove SSH-key records.
- `sshAuth` loads the fingerprint and its stored key, checks run ownership, and requires an admin role with a fresh role stamp only for a foreign-run override. It does not consult session revocation.
- `handleSSHConn` retains the authenticated principal/run for subsequent channels. Channel bridges call `sshFreshRun` to reject unavailable runs, but do not reread the key or session cutoff. Deleting a registration therefore does not terminate an authenticated connection or remove its authority to open more channels to the same running run.
- `handleListSSHKeys` and `handleDeleteSSHKey` are principal-scoped. Deletion path-unescapes the fingerprint; percent-encoding it as one path segment is correct. The CLI exposes `ensure` and `list`, not a delete command.
- Separate registration removal and successful affected-run teardown are accurate incident-response instructions. Including foreign runs reached with an admin override is appropriate.

Inspected `internal/api/routes.go`, `sshkeys.go`, `sessions.go`, `sshgateway.go`, `sshgateway_channels.go`, and `cmd/wardyn/sshkey.go`. This is source review; no SSH account, session, key, or running sandbox was changed.

## Proxy upload correction — fe5c40a9

No blocking correctness or test issue found. The common brokered-upload path checks actual bytes with a cap-plus-one read, denies oversized input before forwarding, emits the deny decision, and preserves complete exact-limit bodies. Fixed limits make integer overflow in the additional-byte calculation unreachable here.

Independent focused test rerun passed in 0.038s:

```sh
go test ./internal/egress/proxy \
  -run 'TestBrokeredScanUploadRejectsOversizeInsteadOfTruncating|TestBrokeredUploadBodyBoundary' \
  -count=1
```

The initial restricted execution could not bind an HTTP test listener. The approved local-server execution passed; the sandbox error was not treated as a product defect.
