// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sys/unix"

	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// recoveryManifestVersion is the only manifest format this binary writes or accepts (recovery-set.md §2.2).
const recoveryManifestVersion = 1

// recoveryManifestFile is the manifest's name inside the set directory.
const recoveryManifestFile = "manifest.json"

// recoveryManifest ties the database snapshot to the staged audit spool and the key configuration
// (recovery-set.md §2.2). It holds no secret value, DSN user or password, and no subject.
type recoveryManifest struct {
	Manifest       int                `json:"manifest"`
	SetID          string             `json:"set_id"`
	CreatedAt      string             `json:"created_at"`
	WardyndVersion string             `json:"wardynd_version"`
	Source         recoverySource     `json:"source"`
	Snapshot       recoverySnapshot   `json:"snapshot"`
	Keys           recoveryKeys       `json:"keys"`
	AuditSpool     recoverySpool      `json:"audit_spool"`
	Recordings     recoveryRecordings `json:"recordings"`
	Drives         []recoveryDrive    `json:"drives"`
}

// recoveryKeys is the key configuration the set was taken under, as fingerprints and counts only
// (recovery-set.md §4.1): the age recipient (public), the KEK mode as set, and the row count per key
// class in the dump. All of it comes from configuration and the database; no key service is called.
type recoveryKeys struct {
	AgeRecipient string `json:"age_recipient"`
	// PlatformRecipient is the recipient of WARDYN_PLATFORM_KEY_FILE's identity, when it is set.
	PlatformRecipient string `json:"platform_recipient,omitempty"`
	KEKMode           string `json:"kek_mode"`
	// KEKKeys is each key-service mount and key name that is set, by its environment name.
	KEKKeys map[string]string `json:"kek_keys,omitempty"`
	Classes map[string]int    `json:"classes"`
}

// manifestSources is what -recovery-manifest reads from the resolved flags.
type manifestSources struct {
	DSN, Set, SpoolBase string
	AgeKey, KEKMode     string
	RecordingStore      string
	PlatformKeyFile     string
	KEKKeys             map[string]string
}

func manifestSourcesOf(f *bootFlags, set string) manifestSources {
	return manifestSources{DSN: *f.dsn, Set: set, SpoolBase: filepath.Base(*f.auditSpool), AgeKey: *f.ageKey, KEKMode: kekModeOf(f),
		RecordingStore: *f.recordingSel, PlatformKeyFile: *f.platformKeyFile, KEKKeys: kekKeyNames(f)}
}

// kekKeyNames is the key-service mount and key names that are set, by environment name: names only,
// read from configuration, so a rehearsal can say which keys the set needs. The Transit mount has a
// default, so it is named only beside a Transit key.
func kekKeyNames(f *bootFlags) map[string]string {
	names := map[string]string{}
	for env, v := range map[string]*string{
		"WARDYN_VAULT_TRANSIT_KEY":              f.vault.transitKey,
		"WARDYN_VAULT_TRANSIT_KEY_PLATFORM":     f.vault.transitKeyPlatform,
		"WARDYN_AWS_KMS_KEY":                    f.vault.awsKMSKey,
		"WARDYN_AZURE_KEK_KEY":                  f.azure.kekKey,
		"WARDYN_AZURE_KEK_SIGNING_KEY":          f.azure.kekSigningKey,
		"WARDYN_AZURE_KEK_KEY_PLATFORM":         f.azure.kekKeyPlatform,
		"WARDYN_AZURE_KEK_SIGNING_KEY_PLATFORM": f.azure.kekSigningKeyPlatform,
	} {
		if v != nil && strings.TrimSpace(*v) != "" {
			names[env] = strings.TrimSpace(*v)
		}
	}
	if names["WARDYN_VAULT_TRANSIT_KEY"] != "" || names["WARDYN_VAULT_TRANSIT_KEY_PLATFORM"] != "" {
		if m := f.vault.transitMount; m != nil && strings.TrimSpace(*m) != "" {
			names["WARDYN_VAULT_TRANSIT_MOUNT"] = strings.TrimSpace(*m)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

type recoverySource struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
}

// recoverySnapshot is the database's identity at quiesce (recovery-set.md §3): the newest audit row's seq and
// row_hash, and the newest applied migration's filename. An empty audit trail is seq 0 with an empty hash.
type recoverySnapshot struct {
	AuditHeadSeq    int64  `json:"audit_head_seq"`
	AuditHeadHash   string `json:"audit_head_hash"`
	SchemaMigration string `json:"schema_migration"`
}

// recoverySpool names the audit spool and its two sidecars, each staged under <set>/audit/.
type recoverySpool struct {
	Basename   string       `json:"basename"`
	Spool      recoveryFile `json:"spool"`
	Consumed   recoveryFile `json:"consumed"`
	Quarantine recoveryFile `json:"quarantine"`
}

// recoveryFile is one staged file: present false with size and sha256 empty when it is absent.
type recoveryFile struct {
	Present bool   `json:"present"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
}

// recoveryManifestMode dispatches -recovery-manifest and -recovery-verify (recoveryMode). Both need the
// database to themselves, so each goes through acquireQuiet.
func recoveryManifestMode(f *bootFlags, verify bool) error {
	if *f.recoverySet == "" {
		return errors.New("-recovery-manifest and -recovery-verify need -recovery-set <dir>")
	}
	set := filepath.Clean(*f.recoverySet)
	if verify {
		return verifyRecoveryManifest(context.Background(), *f.dsn, set)
	}
	return createRecoveryManifest(context.Background(), manifestSourcesOf(f, set))
}

// createRecoveryManifest writes <set>/manifest.json once, mode 0600. It never overwrites an existing manifest.
func createRecoveryManifest(ctx context.Context, in manifestSources) error {
	dsn, set, spoolBase := in.DSN, in.Set, in.SpoolBase
	// An empty WARDYN_AUDIT_SPOOL disables the spool: the manifest records none, and verify pairs none.
	if spoolBase == "." {
		spoolBase = ""
	}
	root, err := os.OpenRoot(set)
	if err != nil {
		return fmt.Errorf("open the set: %w", err)
	}
	defer root.Close()
	if _, err := root.Lstat(recoveryManifestFile); err == nil {
		return refusedTo("write the manifest", "%s already exists and is never overwritten", recoveryManifestFile)
	}
	s, err := acquireQuiet(ctx, dsn, 30*time.Second, "write the manifest")
	if err != nil {
		return err
	}
	defer s.close()
	// The session's lock connection stays checked out while stageFiles takes a second one from the pool.
	if n := s.pool.Config().MaxConns; n < 2 {
		return refusedTo("write the manifest", "pool_max_conns is %d; the manifest needs at least 2 connections (the single-instance lock holds one). Raise pool_max_conns in the database URL", n)
	}

	snap, err := readRecoverySnapshot(ctx, s.conn)
	if err != nil {
		return err
	}
	src, err := recoverySourceOf(dsn)
	if err != nil {
		return err
	}
	spool, err := stageSpoolFiles(root, spoolBase)
	if err != nil {
		return refusedTo("write the manifest", "%v", err)
	}
	keys, err := recoveryKeysOf(ctx, s.conn, in)
	if err != nil {
		return err
	}
	recordings, drives, err := stageFiles(ctx, s.pool, root, in.RecordingStore)
	if err != nil {
		return refusedTo("write the manifest", "%v", err)
	}
	m := recoveryManifest{
		Manifest: recoveryManifestVersion, SetID: uuid.NewString(), CreatedAt: time.Now().UTC().Format(time.RFC3339),
		WardyndVersion: version.Version, Source: src, Snapshot: snap, Keys: keys, AuditSpool: spool, Recordings: recordings, Drives: drives,
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	f, err := root.OpenFile(recoveryManifestFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("write %s: %w", recoveryManifestFile, err)
	}
	if _, err := f.Write(append(body, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", recoveryManifestFile, err)
	}
	return f.Close()
}

// verifyRecoveryManifest refuses unless the database at dsn is the snapshot the manifest recorded and each
// staged spool file hashes to its manifest entry. A refusal names what differs: "audit head" for the database,
// the file's name for a sidecar.
func verifyRecoveryManifest(ctx context.Context, dsn, set string) error {
	m, err := readRecoveryManifest(set, "verify the set")
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(set)
	if err != nil {
		return fmt.Errorf("open the set: %w", err)
	}
	defer root.Close()
	s, err := acquireQuiet(ctx, dsn, 30*time.Second, "verify the set")
	if err != nil {
		return err
	}
	defer s.close()

	snap, err := readRecoverySnapshot(ctx, s.conn)
	if err != nil {
		return err
	}
	if snap != m.Snapshot {
		return refusedTo("verify the set", "the database's audit head (seq %d, row_hash %q, schema migration %q) is not the manifest's (seq %d, row_hash %q, schema migration %q); the dump and the set are not one pair",
			snap.AuditHeadSeq, snap.AuditHeadHash, snap.SchemaMigration, m.Snapshot.AuditHeadSeq, m.Snapshot.AuditHeadHash, m.Snapshot.SchemaMigration)
	}
	got, err := stageSpoolFiles(root, m.AuditSpool.Basename)
	if err != nil {
		return refusedTo("verify the set", "%v", err)
	}
	return pairSpool("verify the set", m.AuditSpool, got)
}

// pairSpool refuses unless each staged spool file is its manifest entry, naming the first that is not.
func pairSpool(verb string, want, got recoverySpool) error {
	for _, c := range []struct {
		name      string
		want, got recoveryFile
	}{
		{want.Basename, want.Spool, got.Spool},
		{want.Basename + ".consumed", want.Consumed, got.Consumed},
		{want.Basename + ".quarantine", want.Quarantine, got.Quarantine},
	} {
		if c.want != c.got {
			return refusedTo(verb, "%s does not match its manifest entry (present %t size %d, want present %t size %d)",
				c.name, c.got.Present, c.got.Size, c.want.Present, c.want.Size)
		}
	}
	return nil
}

// recoveryManifestMaxBytes bounds how much of the manifest is read: it holds no secret and a few hundred
// bytes per drive, so a file past this is not one this binary wrote.
const recoveryManifestMaxBytes = 16 << 20

// readRecoveryManifest reads and checks the manifest of the set at dir, through the set's own root. verb is
// what the caller was about to do ("verify the set", "rehearse"), so a refusal names the verb that refused.
// The open is non-blocking and only a regular file is read: a FIFO named manifest.json is refused, not waited on.
func readRecoveryManifest(dir, verb string) (recoveryManifest, error) {
	var m recoveryManifest
	root, err := os.OpenRoot(dir)
	if err != nil {
		return m, fmt.Errorf("open the set: %w", err)
	}
	defer root.Close()
	f, err := root.OpenFile(recoveryManifestFile, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return m, fmt.Errorf("read %s: %w", recoveryManifestFile, err)
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return m, refusedTo(verb, "%s is not a regular file", recoveryManifestFile)
	}
	body, err := io.ReadAll(io.LimitReader(f, recoveryManifestMaxBytes))
	if err != nil {
		return m, fmt.Errorf("read %s: %w", recoveryManifestFile, err)
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return m, fmt.Errorf("parse %s: %w", recoveryManifestFile, err)
	}
	if m.Manifest != recoveryManifestVersion {
		return m, refusedTo(verb, "%s is manifest version %d; this binary reads version %d", recoveryManifestFile, m.Manifest, recoveryManifestVersion)
	}
	if b := m.AuditSpool.Basename; b != "" && (b == "." || b != filepath.Base(b)) {
		return m, refusedTo(verb, "%s names no spool file: audit_spool.basename must be a plain file name, or empty when the spool is disabled", recoveryManifestFile)
	}
	return m, nil
}

// readRecoverySnapshot reads the audit head and the newest migration filename, in two queries on the one
// connection that holds the lock.
func readRecoverySnapshot(ctx context.Context, conn *pgxpool.Conn) (recoverySnapshot, error) {
	var snap recoverySnapshot
	if err := conn.QueryRow(ctx, `SELECT COALESCE(max(seq), 0), COALESCE((SELECT row_hash FROM audit_events ORDER BY seq DESC LIMIT 1), '') FROM audit_events`).
		Scan(&snap.AuditHeadSeq, &snap.AuditHeadHash); err != nil {
		return snap, fmt.Errorf("read the audit head: %w", err)
	}
	if err := conn.QueryRow(ctx, `SELECT COALESCE(max(filename), '') FROM schema_migrations`).Scan(&snap.SchemaMigration); err != nil {
		return snap, fmt.Errorf("read the schema migration: %w", err)
	}
	return snap, nil
}

// recoveryKeysOf reads the key fingerprints: the age recipient from the configured identity, the KEK mode as
// set, and the dump's row count per key class. A key that does not parse is refused without echoing it.
func recoveryKeysOf(ctx context.Context, conn *pgxpool.Conn, in manifestSources) (recoveryKeys, error) {
	k, err := recoveryKeyFingerprints(in)
	if err != nil {
		return k, err
	}
	err = secretstorepg.ListRows(ctx, conn, func(c secretstorepg.CheckedRow) { k.Classes[keyClassOf(c)]++ })
	return k, err
}

// recoveryKeyFingerprints is recoveryKeysOf's configuration half: the recipients and the key names.
func recoveryKeyFingerprints(in manifestSources) (recoveryKeys, error) {
	k := recoveryKeys{KEKMode: in.KEKMode, KEKKeys: in.KEKKeys, Classes: map[string]int{}}
	if key := strings.TrimSpace(in.AgeKey); key != "" {
		id, err := age.ParseX25519Identity(key)
		if err != nil {
			return k, errors.New("WARDYN_AGE_KEY does not parse as an age identity")
		}
		k.AgeRecipient = id.Recipient().String()
	}
	if strings.TrimSpace(in.PlatformKeyFile) != "" {
		// readPlatformKey's own refusals are boot sentences; the manifest names the variable only.
		id, err := readPlatformKey(in.PlatformKeyFile, in.AgeKey)
		if err != nil {
			return k, errors.New("WARDYN_PLATFORM_KEY_FILE does not hold a usable age identity")
		}
		k.PlatformRecipient = id.Recipient().String()
	}
	return k, nil
}

// recoverySourceOf is the DSN's host, port and database, which is all the manifest may name. A DSN that does
// not parse is refused without echoing it: a parse error can carry the DSN.
func recoverySourceOf(dsn string) (recoverySource, error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return recoverySource{}, errors.New("WARDYN_PG_DSN does not parse")
	}
	return recoverySource{Host: strings.ToLower(cfg.Host), Port: int(cfg.Port), Database: cfg.Database}, nil
}

// stageSpoolFiles hashes audit/<base>, its .consumed and its .quarantine in the set. An absent file is
// recorded as absent; one that is not a regular file, or that resolves outside the set, is an error. An
// empty base is a disabled spool: nothing is staged.
func stageSpoolFiles(root *os.Root, base string) (recoverySpool, error) {
	out := recoverySpool{Basename: base}
	if base == "" {
		return out, nil
	}
	var err error
	if out.Spool, err = hashStagedFile(root, "audit/"+base); err != nil {
		return out, err
	}
	if out.Consumed, err = hashStagedFile(root, "audit/"+base+".consumed"); err != nil {
		return out, err
	}
	if out.Quarantine, err = hashStagedFile(root, "audit/"+base+".quarantine"); err != nil {
		return out, err
	}
	return out, nil
}

// hashStagedFile hashes rel inside root. It opens nothing but a regular file: the open is non-blocking,
// so a FIFO is refused by name and never waited on.
func hashStagedFile(root *os.Root, rel string) (recoveryFile, error) {
	name := filepath.Base(rel)
	f, err := root.OpenFile(rel, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return recoveryFile{}, nil
	}
	if err != nil {
		return recoveryFile{}, fmt.Errorf("open staged file %s: %w", name, err)
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return recoveryFile{}, fmt.Errorf("staged file %s is not a regular file", name)
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return recoveryFile{}, fmt.Errorf("read staged file %s: %w", name, err)
	}
	return recoveryFile{Present: true, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}
