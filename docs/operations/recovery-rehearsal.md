> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Recovery set and restore rehearsal

Take a recovery set that ties a database snapshot to its audit spool, recordings and drive bytes, then prove the set on a scratch database before you need it.
The verbs are maintenance modes of `wardynd`, with no HTTP route and no console page.
Design: [recovery-set.md](../design/0.9/recovery-set.md).

## The four verbs

| Verb | What it does | Touches |
|---|---|---|
| `wardynd -recovery-inventory` | Lists each recovery-set item and its location for this deployment | Flags only; no connection |
| `wardynd -recovery-manifest -recovery-set <dir>` | Writes `manifest.json` into the set, with the writer stopped | The live database (read) and the set |
| `wardynd -recovery-verify -recovery-set <dir>` | Refuses unless the database and the spool in the set are the pair the manifest recorded | The database at `WARDYN_PG_DSN` (read) and the set |
| `wardynd -recovery-rehearse -recovery-set <dir>` | Proves pairing, audit chain, key use, recordings and drive bytes on a restored copy | The database at `WARDYN_REHEARSE_PG_DSN` (read), the set, and two audit rows on live |

- None creates, deletes or exports storage, keys or credentials. You make and remove the scratch database and the directories.
- `pg_dump`, provider snapshots and the manual steps in [Back them up](../OPERATIONS.md#back-them-up) keep working unchanged.

## The set layout

| Path in the set | Holds | Copy it from |
|---|---|---|
| `audit/<spool name>`, `.consumed`, `.quarantine` | The audit spool and its two sidecars; any may be absent | `WARDYN_AUDIT_SPOOL` and its siblings |
| `recordings/` | Casts; only under `WARDYN_RECORDING_STORE=fs`. The `pg` store is in the dump | `WARDYN_RECORDING_DIR` |
| `drives/<drive id>/<home>/` | One directory per drive object | The Docker volume, `host_path` subtree or claim of that drive |
| `manifest.json` | Written by the manifest verb, mode `0600`, never overwritten | |

- Keys are never in the set. The manifest holds the age recipient (public), the `WARDYN_KEK` mode and a row count per key class.
- The manifest names a drive by id, home and object name, with no subject field. A managed home is a hash, but a `host_path` home built from the `sub` or `email_local` template is the person's claim, so such a manifest names people.
- Where each item lives per deployment: [Recovery set by deployment](../OPERATIONS.md#recovery-set-by-deployment).

## Take a set

1. Stop the writer, then take the dump or snapshot as [Back them up](../OPERATIONS.md#back-them-up) says.
2. Copy the spool, its sidecars, any `fs` recordings and the drive objects into a new, empty directory in the layout above.
3. Write the manifest with the writer still stopped. The verb refuses while any other client is connected.

   ```sh
   wardynd -recovery-manifest -recovery-set /backup/set-2026-10-10
   ```

4. Start the writer. Store the set and the dump together, and keep the keys apart.

## Rehearse a set

1. Load the dump, or restore the snapshot, into a scratch database. Never into the live one.
2. Restore the set into a new directory.
3. Run the rehearsal with the deployment's key settings in the environment and the scratch database in `WARDYN_REHEARSE_PG_DSN`. The DSN is an environment variable and not a flag, so a password never shows in the process list.

   ```sh
   WARDYN_REHEARSE_PG_DSN='postgres://check@scratch:5432/wardyn_restorecheck' \
     wardynd -recovery-rehearse -recovery-set /scratch/set-2026-10-10
   ```

4. Read the JSON report and the exit code.
5. Drop the scratch database and remove the restored directory yourself.

On Compose and the managed desktop, run the verbs with `docker compose run --rm wardynd`, the set mounted, and stop the converge job first. On Helm, use the pod below.

## Reading the result

| Exit | Verdict | Meaning |
|---|---|---|
| `0` | `pass` | Every check passed and every manifest item was in the set |
| `1` | | The run could not start or finish: bad flags, an unreadable set or manifest, an unreachable scratch database, or a read or connection that failed, which can happen after the begin row |
| `3` | `refused` | A target, pairing or version refusal, an unknown manifest version, an unreachable live database, or a begin row that could not be written; nothing was decrypted |
| `4` | `fail` | At least one check failed |
| `5` | `incomplete` | Every check that ran passed, and some drive or recordings tree was not in the set |

- The report lists each check with counts and failing references, such as a drive id and home. It holds no credential value.
- A refusal names the first check that stopped the run.

| Refusal | Why |
|---|---|
| Target names more than one host | A fallback host could be one no check saw |
| Target is the live database or the manifest's source | A rehearsal against live passes and proves nothing |
| Live database does not answer | The lock probe that tells the two databases apart needs live |
| Target shares a server lock with live | The target is live under another name |
| Target is a standby, or has a writer or another client | A standby of live would verify live |
| Set overlaps a live spool, recording or drive path | The set must be a copy |
| Newest migration of target, manifest and binary differ | A read-only target cannot be migrated |
| Audit head or a spool file differs from the manifest | The dump and the set are not one pair |

## Kubernetes: the rehearsal pod

The chart renders nothing for this. You write a pod that mounts the restored set.
The pod reads the scratch database from a Secret you own, and carries the deployment's key settings from the Secret that holds them. The rehearsal reads the set only, so copy each restored drive claim's contents into `drives/<drive id>/<home>/` of the set first, for example with a Job that mounts both.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: wardyn-rehearse
spec:
  restartPolicy: Never
  containers:
    - name: rehearse
      image: ghcr.io/cjohnstoniv/wardynd:0.9.0
      args: ["-recovery-rehearse", "-recovery-set", "/set"]
      envFrom:
        - secretRef: {name: wardyn-keys}
      env:
        - name: WARDYN_REHEARSE_PG_DSN
          valueFrom: {secretKeyRef: {name: rehearse-scratch-db, key: dsn}}
        - name: WARDYN_PG_DSN
          valueFrom: {secretKeyRef: {name: wardyn-postgres-dsn, key: dsn}}
        - name: WARDYN_AGE_KEY
          valueFrom: {secretKeyRef: {name: wardyn-postgres-dsn, key: age-key}}
      volumeMounts:
        - {name: set, mountPath: /set, readOnly: true}
  volumes:
    - name: set
      persistentVolumeClaim: {claimName: restored-recovery-set, readOnly: true}
```

- `WARDYN_PG_DSN` is the live database: the target checks probe it, and the two audit rows are written to it. Both `secretKeyRef` entries name the DSN Secret, under `postgres.dsn.key` and `age-key`; it holds `age-key` only with `secrets.ageKeyFromSecret`, so use the `secrets.ageKeySecretRef` Secret for `WARDYN_AGE_KEY` if you set one.
- Delete the pod and the scratch database when the report is saved.

## What a pass proves

| Check | Proves | Does not prove |
|---|---|---|
| Pairing | The restored database is at the manifest's audit head and the spool files are the recorded bytes | That nothing was written between the dump and the manifest; that case refuses, naming `audit head` |
| Audit chain | Every audit row's hash chain verifies | |
| Key use | The keys in this process open every stored row: credentials, principal keys, pre-envelope rows. Unwrapping through a Vault Transit or Azure Key Vault key is the code path in use, but this release's tests do not exercise it | Values held in an external store, which are counted `external` and never read |
| Recordings | Each cast parses, and under `fs` the tree equals the manifest's | That a recording is the one a person remembers |
| Drives | Each staged drive object equals the manifest's digest, and no drive object is in the set that the manifest does not list | Homes that were never staged: the manifest records the homes in the set, and a drive with none |

- The verdict does not look for rows in the target that the manifest does not describe, except later audit rows (pairing) and extra stored secrets (counted per key class). The manifest is the only reference ([residual](../design/0.9/recovery-set.md#10-residuals-and-threat-model-delta) (a)).
- A restored set from before an erasure brings the erased data back while the wrapping key lives.
- The manifest is plain JSON and not signed: it detects a mismatched pair, and anyone who can rewrite the stored set can rewrite it.
- The rehearsal opens every stored value in memory, as an operator holding the keys and the dump already can offline. The begin row `recovery.rehearse.begin` records that it did.
- The target checks defend against mistakes. They do not stop a key-holder who controls both database addresses.
