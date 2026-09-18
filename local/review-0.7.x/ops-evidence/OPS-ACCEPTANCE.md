# Isolated PostgreSQL backup/restore rehearsal

Result: PASS on 2026-09-18, code commit `117c9e046b0041a7004e095dd3fdb57e92f56d05`
(documentation-only delta from `dfa89f60`). No tracked files were changed for this rehearsal.

## Environment and boundaries

- Container: `wardyn-review-07x-pg`, Docker socket `unix:///var/run/docker.sock`.
- Database endpoint: `localhost:58432`; PostgreSQL 17.10, Debian image, x86_64.
- Database role: `wardyn`, superuser; this does not prove a split migrator/runtime role deployment.
- Created only `wardyn_review_ops_source_20260918` and `wardyn_review_ops_restore_20260918`.
- Both names were confirmed absent before creation. Neither database was shared with application or browser tests.
- No existing cluster, production database, daemon, recording-test database, volume, or container was altered.
- Both rehearsal databases remain in the review container for inspection. No cleanup deletion was performed.

## Executed

The ignored `main.go` helper calls existing production APIs: `db.Connect`, `db.Migrate`,
`store.InsertAuditEvent`, `PG.VerifyAuditChain`, the PostgreSQL secret store, and the
PostgreSQL recording store. It follows OPERATIONS' plain `pg_dump`/`psql` restore shape.

1. Applied all 62 embedded migrations to a fresh database; repeated migration was successful with the same tracking count.
2. Inserted three chained synthetic audit events, one age-encrypted synthetic secret, and one synthetic asciicast through existing storage APIs.
3. Ran `docker --host unix:///var/run/docker.sock exec -i wardyn-review-07x-pg pg_dump -U wardyn wardyn_review_ops_source_20260918` to `ops-backup.sql`.
4. Restored into the separate empty database with `psql -U wardyn -v ON_ERROR_STOP=1 -q wardyn_review_ops_restore_20260918`.
5. Verified restored migration count, three audit rows, and byte-identical chain status/head hash. Re-ran `db.Migrate` against the restored database without altering its chain.
6. Decrypted the restored secret with the original age identity and confirmed an independently generated wrong identity is refused. Verified the recording payload byte-for-byte.
7. Confirmed restored audit UPDATE, DELETE, and TRUNCATE guards reject writes. Each attempt used a rolled-back transaction, including the unexpected-success path.
8. Appended a fourth audit event through the production insert path; its `prev_hash` matched the restored head and the full four-row chain verified. Source chain remained unchanged.

The build used `GOFLAGS=-buildvcs=false` for the temporary-worktree Go VCS discovery issue.
The helper exited 0. `ops-transcript.log` captures migration, restore, and final results.
`ops-results.json` contains the complete before/after chain statuses.

Pre-dump and restored head: `b29f015ba80eed94b31b8b9037126a31835431e4bc050094696fa6362a22268c`.
Post-restore append head: `16210fddbcf505da434cdf87d432a116fb5f4d32c130fdc390e2b016f8d33401`.
Audit sequence gaps are expected from migration canaries/rolled-back writes; the verifier accepts the valid chain.

## Artifacts

- Executed temporary `main.go`, before adding the SPDX header to the preserved copy: SHA256 `ac2766a18b3a08aae7f7dc14be1095cde416fc5eafb6ec459b757dd1ddbebb91`.
- `ops-backup.sql`: 45,531 bytes; SHA256 `cf0b8f70f6859b9585ad8e6b8c1e9451f6b4c0d5cdc321a51a251e71dd3d6a0d`.
- `ops-results.json`: SHA256 `64dc455826917e6428186e930cd268374fd97a932f533719bee6319c9435c45f`.
- `ops-transcript.log`: complete executed transcript.

The age identities were generated for synthetic data and held only in this helper's memory.
No real credentials were used. The dump remains useful for database/chain inspection; its
synthetic ciphertext is deliberately not accompanied by a saved private key.

## Not established by this check

- Full application restart, API authorization, HTTP audit endpoint behavior, or launching a restored workspace/run.
- Persistent age-key recovery from Kubernetes Secret, secret manager, or Compose environment across processes/hosts.
- Helm/Compose scale-down, restore orchestration, live cluster recovery, or source-to-different-PostgreSQL-version compatibility.
- Filesystem recordings, PVC snapshots, user-drive bytes and labels, external registries, or pending audit spool/quarantine recovery.
- Production-sized data, backup consistency while concurrent writers run, interrupted dump/restore recovery, PITR/WAL, storage failure, or recovery-time objectives.
- Managed PostgreSQL roles/permissions and split migrator/runtime ownership.
- Off-box SIEM head comparison or detection of an attacker who can rewrite and re-chain the entire log.

This is a small synthetic database roundtrip and integrity proof, not a completed production disaster-recovery exercise.
