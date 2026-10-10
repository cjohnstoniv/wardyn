# 0.9 recovery set: inventory, manifest and isolated restore rehearsal (#1514)

Status: **Draft for owner review.** Decisions marked OD-R*n* wait for the owner ([Owner decisions](#11-owner-decisions)).
This design sets the public contract the issue asks for first: snapshot identity, encrypted-key handling, storage backends and safe target selection.
It amends the lane brief where the code disagrees with it ([§1.2](#12-amendments-to-the-brief)).

## 1. Scope

### 1.1 What ships

| Verb | Flag | Touches | Decrypts |
|---|---|---|---|
| Inventory | `wardynd -recovery-inventory` | Resolved flags only; no connection | Nothing |
| Manifest | `wardynd -recovery-manifest -recovery-set <dir>` | Live database (read, writer stopped) and the staged set | Nothing |
| Verify | `wardynd -recovery-verify -recovery-set <dir>` | The database at `WARDYN_PG_DSN` (read) and the set; the in-place spool if present | Nothing |
| Rehearse | `wardynd -recovery-rehearse -recovery-set <dir>` | A scratch database at `WARDYN_REHEARSE_PG_DSN` (read), the set, one probe session on live | Every stored row, in memory ([OD-R1](#11-owner-decisions)) |

- All four are maintenance modes dispatched by `maintenanceMode` in [`cmd/wardynd/migrate_secrets.go`](../../../cmd/wardynd/migrate_secrets.go), siblings of `-migrate-only` and `-rewrap`. No new binary.
- None has an HTTP route or a console surface. The issue forbids exposing per-person credential values through an admin interface; a host CLI run by whoever already holds the keys exposes nothing new.
- None creates, deletes or exports storage, keys or credentials. The operator makes the scratch database and the set directory, and removes them.
- `pg_dump`, provider snapshots and the manual recipes in [Back them up](../../OPERATIONS.md#back-them-up) and [Restore them](../../OPERATIONS.md#restore-them) keep working unchanged.

### 1.2 Amendments to the brief

| Brief proposal | This design | Reason |
|---|---|---|
| Refuse when `system_identifier` and `current_database()` equal live's | Dropped. A lock-visibility probe replaces it ([§5](#5-safe-target-selection)) | A provider snapshot restore is a physical copy and keeps live's `system_identifier` and database name, so the check refuses a supported flow |
| Refuse when the `sweeper_leader` lease looks live | Replaced by `acquireQuiet` on the target | `sweeper_leader` has no expiry: leadership is a session advisory lock. `db.AdvisoryLockHeld` is not database-scoped, so it would see live's lock from a scratch database on the same server |
| Drive digests through the drive-probe image; a Job on Kubernetes | No Docker or Kubernetes API call. Drives reach every verb as plain directories in the set ([§6](#6-storage-backends)) | The probe image only tests readability. One directory layout covers volume, `host_path` and PVC |
| `-rehearse-dsn` on the command line | `WARDYN_REHEARSE_PG_DSN` | A password in argv is readable by every local user through the process list |
| Manifest holds each drive's owner subject | Drive id, home and object name only | The object maps to its person through the restored database; the plaintext manifest carries less personal data |
| Snapshot identity from `VerifyAuditChain` at create | `max(seq)` and that row's `row_hash`, one query | The writer is stopped while create runs; a full chain sweep is `O(log)` and lengthens that window. The rehearsal runs the full sweep |

### 1.3 Out of scope

- A general backup framework, a scheduler, object-storage backends, or retention of sets.
- Taking the dump. Wardyn never reads a dump file; it reads the database the dump was loaded into.
- A boot-time pairing gate. `-recovery-verify` is an explicit step before the first boot after a restore.
- Restoring key-service or external-store contents (Vault Transit, Vault KV, Azure Key Vault). Their own backup covers them.

## 2. The set

### 2.1 Items

| Item | Source | In the set as | Notes |
|---|---|---|---|
| Database | `WARDYN_PG_DSN` | The operator's dump or provider snapshot, outside the set directory | Identified by its audit head ([§3](#3-snapshot-identity)) |
| Audit spool and sidecars | `WARDYN_AUDIT_SPOOL`, `<spool>.consumed`, `<spool>.quarantine` | `audit/<spool basename>` and its two sidecars | Each file may be absent |
| Recordings | `WARDYN_RECORDING_STORE` and `WARDYN_RECORDING_DIR` | `recordings/`, only under the `fs` store | `pg` is in the database; `off` has none |
| Drive objects | One per drive and home, named by `types.DriveObjectName` | `drives/<drive id>/<home>/` | Volume, `host_path` subtree or PVC contents |
| Keys | `WARDYN_AGE_KEY`, `WARDYN_PLATFORM_KEY_FILE`, `WARDYN_KEK` and its service | **Never.** Fingerprints only, in the manifest | [§4](#4-encrypted-key-handling) |
| Manifest | Written by `-recovery-manifest` | `manifest.json`, mode `0600` | Never overwritten |

- The set directory is a staging copy the operator fills with the same copy commands the recipes use today, into this layout instead of tarballs.
- Runner-device keys (0.9 client mode) and the desktop's `age.key` are generated on the device and stay out, as [Recovery set by deployment](../../OPERATIONS.md#recovery-set-by-deployment) already says.

### 2.2 Manifest fields

| Field | Content | Never contains |
|---|---|---|
| `manifest` | `1`. Any other value is refused | |
| `set_id` | A random UUID naming the set in reports and audit; never used for matching | |
| `created_at`, `wardynd_version` | When and by which release | |
| `source.db` | Host, port and database name | User, password, query parameters |
| `snapshot` | `audit_head_seq`, `audit_head_hash`, `schema_migration` | |
| `keys` | Fingerprints and row counts per key class ([§4.1](#41-what-the-set-holds)) | Any identity, key, token, wrapped key or value |
| `audit_spool` | Basename; per file `present` or `absent`, size, `sha256` | File contents |
| `recordings` | Store kind; under `fs`, entry count and tree digest | Recording contents |
| `drives` | Per object: drive id, home, backend, object name, `staged` or `not_in_set`, entry count, tree digest | Subject, file contents |

### 2.3 Tree digest

- One line per entry, sorted bytewise by path: `path \t type \t perm \t size \t sha256`, newline-terminated.
- `type` is `f`, `d` or `l`. A symlink's `sha256` is over its target string; it is never followed.
- `perm` is the octal permission bits. Owner, group and times are left out, because a restore onto another host changes them.
- The digest is `sha256` over the joined lines. Any other file type (socket, device, FIFO) fails the digest by name.
- Reads go through `os.Root` on the item's directory, so no entry resolves outside it.

## 3. Snapshot identity

The database snapshot is identified by its audit head at quiesce: `max(seq)` of `audit_events`, that row's `row_hash`, and the newest `schema_migrations` filename.

- It is content identity. It survives a `pg_dump` load into any cluster and a provider snapshot restore. The Postgres system identifier and WAL position do not survive a `pg_dump` load.
- The spool pairing hazard is about audit events, so the audit head is the right key. A newer `.consumed` cursor beside an older dump skips events ([Audit fallback recovery](../../OPERATIONS.md#audit-fallback-recovery)).
- `-recovery-manifest` and `-recovery-verify` refuse unless their database is quiet, through `acquireQuiet` in [`cmd/wardynd/migrate_only.go`](../../../cmd/wardynd/migrate_only.go): the single-instance lock is free and no other client is connected.
- It writes no audit row to the live database. A row written after the dump would move the head past the snapshot it describes.
- Verify and rehearse refuse a database whose identity differs, naming `audit head`, and a sidecar whose hash differs, naming that file. That is the "refuses a mismatched pair" acceptance.

> [!NOTE]
> Two dumps with the same audit head and different non-audited rows are indistinguishable by this identity. Pairing stays correct, because the spool only ever holds audit events.

## 4. Encrypted-key handling

### 4.1 What the set holds

| Key material | In the set | Never in the set | Who can open the set's ciphertext |
|---|---|---|---|
| `WARDYN_AGE_KEY` (`local` KEK) | The `age1…` recipient | The identity | Whoever holds the identity and the dump: residual 48(b), unchanged |
| `WARDYN_PLATFORM_KEY_FILE` | Its recipient | The file | Holder of the file, for boot-key rows only |
| `WARDYN_KEK=transit` or `azurekv` | Mode, mount and key names, newest version per key | Vault token, Entra secret, any wrapped key | Whoever the key service lets unwrap |
| Principal keys (`principal_keys`) | Row count per purpose and state, from the dump | Unwrapped keys | As the key-domain KEK above them |
| Store mode (`vaultkv`, `azurekv`) | Count of pointer rows | Values; they live in the external store | The external store's own access |
| Desktop `age.key` | Recipient, when present on the device | The file ([DESKTOP.md](../../DESKTOP.md#why-agekey-never-rides-in-an-mdm-payload)) | The device only |

- Row counts per key class (`kek_id` prefix and version) let the rehearsal say every row of the set was tried, not a sample.
- Fingerprints let the rehearsal report "this host holds a different key than the set was taken with" before any decrypt.

### 4.2 How the rehearsal proves the keys

- It opens the target with the read path only: `Store.open` and `Store.openPrincipal` in [`internal/secretstore/pg/pg.go`](../../../internal/secretstore/pg/pg.go), reached through one new exported read-only iterator. It never calls `newSecretStore`, whose `convertSecretStore` rewrites pre-envelope rows.
- A pre-envelope (`enc_version` 0) row is age-decrypted in memory and reported as `converts on first boot`; nothing is written.
- Each plaintext is checked by the AEAD tag, then cleared with `clear`. No value reaches a log, the report, an error or the audit row.
- Errors name rows by reference (owner and name), as the store's own refusals already do.
- A principal key destroyed by an erasure (`subjectkey.ErrDataLoss`) is counted `destroyed`, not failed: that is the erase working.
- Store-mode pointer rows are counted `external` and never read. Reading one reads the live external store, which proves nothing about the set.
- Rows under `transit` or `azurekv` are unwrapped through the configured key service ([OD-R7](#11-owner-decisions)). The service logs each unwrap.
- The rehearsal grants no capability: the operator running it already holds the keys and the dump, which open every value offline today.

## 5. Safe target selection

### 5.1 Refusals

The rehearsal refuses, exit `3`, before reading any row, when any check below holds. Each check runs in order and the first refusal stops the run.

| # | Check | Refuses when | Why it is needed |
|---|---|---|---|
| T1 | Static compare | The target's host, port and database (after `pgconn.ParseConfig`, host lowercased) equal live's or the manifest's `source.db` | Catches a typo before any connection |
| T2 | Live reachable | `WARDYN_PG_DSN` does not connect ([OD-R3](#11-owner-decisions)) | T3 needs live; a rehearsal is a drill run while live is up |
| T3 | Lock visibility | A session on live takes a session advisory lock on a random 64-bit key; the target sees that key in `pg_locks` for its own database oid | Proves "not the same database" through any hostname alias, IP, port-forward or pooler |
| T4 | Standby | `pg_is_in_recovery()` is true on the target | A streaming standby of live passes T3 and would verify live, not the set |
| T5 | Quiet target | `acquireQuiet` on the target fails: the single-instance lock is held or another client is connected | Nothing may serve the target while it is read |
| T6 | Set overlap | The set directory is, contains or sits inside the spool's directory, `WARDYN_RECORDING_DIR` or any `WARDYN_USER_DRIVE_HOST_ROOTS` entry, compared by `os.SameFile` along each ancestor | A bind mount or symlink alias of a live path is caught |
| T7 | Version | The target's newest migration, the manifest's and this binary's are not all equal | The rehearsal cannot migrate a read-only target ([OD-R6](#11-owner-decisions)) |

- The T3 key comes from `crypto/rand`. Both sessions close before the rehearsal reads anything, and the probe touches no live row.
- A scratch database on the live server passes T3, because advisory locks are per database. That keeps the existing [scratch-database recipe](../../OPERATIONS.md#restore-rehearse-into-a-scratch-database-first) usable.

> [!IMPORTANT]
> Why refuse a read-only run against live at all: right after a quiesced backup, live's head, spool and files equal the manifest. A rehearsal pointed at live would pass and prove nothing about the set.

### 5.2 Read-only on the target

- The target pool sets `default_transaction_read_only=on` as a runtime parameter, and every query runs in a `READ ONLY` transaction.
- The rehearsal never calls `db.Migrate`, never opens the spool through `api.NewAuditSpool`, and opens recordings without creating the erasure directory.
- The set directory is read through `os.Root`. The tests run it with the set tree mode `0555` and the files `0444`.

## 6. Storage backends

| Item | 0.9 backend | How the verbs reach it |
|---|---|---|
| Database | Postgres via DSN | `pg_dump` load or provider snapshot; Wardyn reads the loaded database |
| Recordings, `pg` store | In the database | Opened from the target with `recording.OpenJoined` |
| Recordings, `fs` store | Directory | `recordings/` in the set |
| Drives: Docker volume, `host_path`, PVC | Directory | `drives/<drive id>/<home>/` in the set |
| Spool and sidecars | Files | `audit/` in the set |

- No verb calls the Docker API, the Kubernetes API or an object store. Copying a volume or a claim into the set is the operator's step, as it is today.
- On Helm the verbs run in an operator-authored pod that mounts the set and, for drives, the restored claims ([OD-R4](#11-owner-decisions)). The chart renders nothing new.
- On the managed desktop they run with `docker compose run --rm wardynd` and the set mounted. Stop the converge job first.
- A drive object with no directory in the set is `not_in_set` ([OD-R5](#11-owner-decisions)).

## 7. Rehearsal flow

### 7.1 Operator steps

1. Stop the writer and take the dump or snapshot as [Back them up](../../OPERATIONS.md#back-them-up) says.
2. Copy the spool, sidecars, `fs` recordings and drive objects into a fresh set directory in the [layout](#21-items).
3. Run the manifest, still with the writer stopped:

   ```sh
   wardynd -recovery-manifest -recovery-set /backup/set-2026-10-10
   ```

4. Start the writer. Store the set and the dump; keep the keys apart, as today.
5. Later, load the dump (or restore the snapshot) into a scratch database, and restore the set into a new directory from where it is stored.
6. Rehearse, with the live deployment's key configuration in the environment:

   ```sh
   WARDYN_REHEARSE_PG_DSN='postgres://check@scratch:5432/wardyn_restorecheck' \
     wardynd -recovery-rehearse -recovery-set /scratch/set-2026-10-10
   ```

7. Drop the scratch database and remove the restored directory yourself.

### 7.2 What the rehearsal does

1. Reads and validates the manifest; refuses an unknown `manifest` version.
2. Runs T1–T7.
3. Checks pairing: audit head against `snapshot`, each sidecar against `audit_spool`. A mismatch refuses, exit `3`.
4. Writes `recovery.rehearse.begin` to live ([OD-R2](#11-owner-decisions)).
5. Runs `PG.VerifyAuditChain` in [`internal/store/auditchain.go`](../../../internal/store/auditchain.go) on the target.
6. Key use: opens every row per [§4.2](#42-how-the-rehearsal-proves-the-keys); compares counts per class with the manifest.
7. Recordings: under `fs`, the tree digest, then each cast's header parses. Under `pg`, each recording's header parses.
8. Drives: each `staged` object's tree digest against the manifest.
9. Prints the report, writes `recovery.rehearse` to live, exits.

### 7.3 Report and exit codes

The report is JSON on stdout: `set_id`, `verdict`, and one entry per check with `result`, counts and failing references. It holds no value, path contents or credential.

| Exit | Verdict | Meaning |
|---|---|---|
| `0` | `pass` | Every check passed and every manifest item was covered |
| `1` | | Could not run: bad flags, unreadable manifest, target unreachable |
| `3` | `refused` | A target, pairing, version or audit refusal; nothing was decrypted |
| `4` | `fail` | At least one check failed |
| `5` | `incomplete` | Every check that ran passed, and some item was `not_in_set` ([OD-R5](#11-owner-decisions)) |

- Exit `2` is skipped because Go's `flag` package uses it for a parse error.
- Verify uses `0` and `3` only.

## 8. Audit

| Verb | Audit | Why |
|---|---|---|
| Inventory | None | Reads flags only |
| Manifest | None | A row on live would move the head past the snapshot ([§3](#3-snapshot-identity)) |
| Verify | None | Read-only gate before the first boot; nothing is decrypted |
| Rehearse | `recovery.rehearse.begin` before any decrypt; `recovery.rehearse` with the verdict ([OD-R2](#11-owner-decisions)) | Every stored value is opened in memory |

- Both rehearse rows go to the live trail with the store's chain-locked insert. They never use the live spool, which a running wardynd owns.
- The row data: `set_id`, manifest head seq, target host, port and database, per-check results and counts. No row references, no subjects.
- No `secret.read` row per opened value. The target is read-only, and the per-row reads are summarised by count in the verdict row.
- New actions need rows in [AUDIT-ACTIONS.md](../../AUDIT-ACTIONS.md) in the same change, cited by symbol.

## 9. Failure modes

| Condition | Outcome | Where |
|---|---|---|
| A writer or other client is on live during `-recovery-manifest` | Refused, exit `3`, naming the session | `acquireQuiet` |
| `manifest.json` already exists | Refused; never overwritten | Manifest |
| A file of another type in a set item | Digest fails naming the path | Tree digest |
| Target fails T1–T7 | Refused, exit `3`, naming the check | [§5.1](#51-refusals) |
| Audit head or a sidecar differs | Refused naming `audit head` or the sidecar | Verify, rehearse |
| Chain sweep fails | `fail`, with `broken_seq` and reason | Audit chain check |
| Wrong age key or platform file | `fail`, naming the class and row references; fingerprint mismatch printed first | Key use |
| Key service does not answer | `fail`, "could not be unlocked", never `pass` | Key use |
| Row counts per class differ from the manifest | `fail`, naming the class | Key use |
| A cast header does not parse, or the recordings digest differs | `fail`, naming the key or the digest | Recordings |
| A drive digest differs | `fail`, naming drive id and home | Drives |
| `recovery.rehearse.begin` cannot be written | Refused before any decrypt ([OD-R2](#11-owner-decisions)) | Audit |
| Interrupted | No verdict printed; nothing was written to the target or the set | Any |

## 10. Residuals and threat-model delta

Proposed text for the Docs lane, as a new numbered residual in [THREAT-MODEL.md](../../../threatmodel/THREAT-MODEL.md). The lead assigns the number.

- (a) **The manifest is not authenticated** ([OD-R8](#11-owner-decisions)). It detects a mismatched pair; anyone who can rewrite the stored set can rewrite it to match.
- (b) **Identity is the audit head.** Non-audited rows written between two dumps with one head are not told apart.
- (c) **A restored pre-erasure set resurrects erased data.** That covers credentials, principal keys, recordings and drive bytes, while the wrapping key lives. Residual 48(a) and (b) already state the horizon; the rehearsal opens such rows and reports `destroyed` only for keys erased before the set was taken.
- (d) **T6 sees only this process's mounts.** A live volume mounted only at the set path, in a container where the live path is absent, is not caught.
- (e) **The set can be the staging copy itself.** A rehearsal then proves the copy, not the backup store's restore. The procedure says to restore into a fresh directory; Wardyn cannot check where bytes came from.
- (f) **The rehearsal decrypts every stored value in memory** ([OD-R1](#11-owner-decisions)), as the operator holding keys and dump already can offline. It adds no capability; it adds the audited begin row.
- (g) **Scratch copies are the operator's to dispose of.** Wardyn never deletes them, by the issue's rule.
- (h) **Logical replicas pass T3 and T4.** A logically replicated copy of live is a different database; at quiesce it would pass as the set.
- Other sealed blobs (run masking manifests, sealed audit fields, sealed proxy configs) are reached through keys the rehearsal proves, but are not opened themselves.

## 11. Owner decisions

1. **OD-R1. Which stored rows the rehearsal opens.**
   - (a) Boot keys plus one row per key class.
   - (b) Every row: value decrypted in memory, AEAD-checked, cleared, never output.
   - (c) Unwrap every data key; decrypt boot keys only.
   - Recommendation: (b). A pass that skipped corrupt rows overclaims, and whoever runs it already holds every key.
2. **OD-R2. Audit of a rehearsal.**
   - (a) `recovery.rehearse.begin` on live, required before any decrypt (refuse if unwritable), plus a best-effort verdict row.
   - (b) Verdict row only, best effort.
   - (c) No audit; stdout report only.
   - Recommendation: (a), the shape of the broker's mint row.
3. **OD-R3. Live database unreachable at rehearsal time.**
   - (a) Refuse.
   - (b) Allow with an acknowledgement flag, keeping T1 and T4–T7.
   - Recommendation: (a). A rehearsal is a drill; a real disaster follows the restore procedure and `-recovery-verify`.
4. **OD-R4. Drive bytes on Kubernetes (PVC).**
   - (a) Operator-authored pod mounting restored claims under the set layout; documented example; Wardyn creates nothing.
   - (b) Wardyn creates a Job through the k8s runner.
   - (c) Manual check only; PVC drives always `not_in_set`.
   - Recommendation: (a).
5. **OD-R5. Drive objects missing from the set.**
   - (a) Verdict `incomplete`, exit `5`, listing them.
   - (b) Verdict `fail`.
   - (c) Ignored.
   - Recommendation: (a). It is honest without forcing every unused grant into a drill.
6. **OD-R6. Release skew between the set and the rehearsing binary.**
   - (a) Refuse unless the newest migration is equal on target, manifest and binary.
   - (b) Allow a newer binary, reading the older schema unmigrated.
   - Recommendation: (a). The restore procedure restores on the same release, then upgrades.
7. **OD-R7. Key-service identity during a rehearsal (`transit`, `azurekv`).**
   - (a) The deployment's configured identity.
   - (b) A separate unwrap-only identity for rehearsals.
   - Recommendation: (a) for 0.9, documented; (b) as a follow-up.
8. **OD-R8. Manifest authenticity.**
   - (a) Plain JSON, residual (a).
   - (b) Signed with the deployment's signing key; the rehearsal verifies with the restored key, which also proves that key.
   - Recommendation: (a) for 0.9. (b) changes the format, so `manifest` would become `2`.

## 12. Slices

Every slice is Z0 tier. "Free" means a free executor may build it; "core" means Sonnet/high or Opus. Each slice's PG tests follow the lane rules: a skipped test is not evidence.

| Slice | Model | Builds | Owns |
|---|---|---|---|
| F-1514b | Free | `-recovery-inventory`: one JSON item per [§2.1](#21-items) row, with `item`, `location`, `source`, `set_path`, `note`; deployment from `WARDYN_MANAGED_DIR` and the runner | `cmd/wardynd/recovery_inventory.go` and test; flag and dispatch; one bullet in OPERATIONS.md |
| F-1514c | Free | `internal/recoveryset` (manifest type, tree digest, compare); `-recovery-manifest` and `-recovery-verify` | `internal/recoveryset/`; `cmd/wardynd/recovery_manifest.go` and tests |
| F-1514d | Core | `-recovery-rehearse` skeleton: T1–T7, read-only target pool, pairing, chain sweep, report, exit codes | `cmd/wardynd/recovery_rehearse.go`, `recovery_target.go` and tests; `acquireQuiet` may take a caller's pool |
| F-1514e | Core | Key use ([§4.2](#42-how-the-rehearsal-proves-the-keys)) and the audit rows ([§8](#8-audit)) | `cmd/wardynd/recovery_keyuse.go` and tests; the read-only iterator in `internal/secretstore/pg/`; AUDIT-ACTIONS.md rows |
| F-1514f | Free | Recordings and drive checks in the rehearsal | `cmd/wardynd/recovery_files.go` and tests |
| F-1514g | Free | Docs: a task page under `docs/operations/`, the set-layout recipes, the Helm pod example, the "What is not proven" replacement | `docs/operations/recovery-rehearsal.md`; OPERATIONS.md; `TestRecoverySetByDeploymentDocumented` |

### 12.1 DONE WHEN

A test named "fails when X is removed" is a mutation proof: REPORT.md shows it failing with X reverted, then passing.

- **F-1514b**
  - `TestRecoveryInventory_ListsEveryItemWithALocation` passes for the Compose, desktop and Helm flag sets.
  - `TestRecoveryInventory_NeverPrintsASecretValue` sets canaries in `WARDYN_AGE_KEY`, the DSN password and a sink token; it fails when the DSN prints unstripped.
  - The mode exits `0` with the DSN pointing at `127.0.0.1:1`: it opens no connection.
- **F-1514c**
  - Digest tests: order-independent; a perm change and a one-byte flip change it; a symlink is not followed; owner and mtime are ignored; a FIFO is refused.
  - PG: create then verify passes. One more audit row refuses naming `audit head`, and the test fails with the head compare removed.
  - PG: a changed `.consumed` refuses naming it; another attached client refuses; an existing manifest refuses; an unknown `manifest` version refuses.
- **F-1514d**
  - One PG test per T-check, each failing with its check removed.
  - The T3 test reaches live as `localhost` and the target as `127.0.0.1`, and refuses. A scratch database on the live server passes.
  - A write through the target pool fails as read-only. A chain break yields `fail` with `broken_seq`.
- **F-1514e**
  - The right key passes; a wrong age key fails naming the class.
  - Target row counts and audit head are unchanged after a run. A fake external store records zero reads.
  - `TestRehearse_NoCredentialValueInAnyOutput` searches stdout, stderr, the manifest and the audit rows for canaries, on pass and fail paths.
  - Its canaries: the age key, both DSN passwords, a stored value and a Vault token. It fails when an error embeds the DSN.
  - The begin row precedes the first decrypt, and the AUDIT-ACTIONS guards pass.
- **F-1514f**
  - `fs` and `pg` recordings pass; a truncated cast fails naming it.
  - A drive one-byte flip fails, and the test fails if the compare is reduced to an entry count.
  - A missing object yields `incomplete`. The run passes with the set tree at mode `0555`.
- **F-1514g**
  - The guard pins the replacement sentence and states what the old pin protected.
  - The new page is in a doc-form `.list` with a budget; `make lint` and the full `go test ./...` pass.
  - One Compose rehearsal run is recorded in REPORT.md, or marked UNVERIFIED.

- Order: b, then c, then d, then e. f and g follow e. b alone is the cut-tier partial; b and c give the pairing gate.
- If `maintenanceMode` passes its `gocyclo` cap, c moves the recovery verbs into one `recoveryMode` dispatcher. Each verb refuses being combined with another maintenance mode, as `-migrate-only` does.
- c, d and e put their CHANGELOG lines in REPORT.md; e puts the residual text of [§10](#10-residuals-and-threat-model-delta) there too.
