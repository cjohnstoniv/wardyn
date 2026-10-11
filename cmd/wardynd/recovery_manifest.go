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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/version"
)

// recoveryManifestVersion is the only manifest format this binary writes or accepts (recovery-set.md §2.2).
const recoveryManifestVersion = 1

// recoveryManifestFile is the manifest's name inside the set directory.
const recoveryManifestFile = "manifest.json"

// recoveryManifest ties the database snapshot to the staged audit spool (recovery-set.md §2.2). It holds no
// secret value, DSN user or password, and no subject. Keys and the recordings and drives entries are not in
// this slice.
type recoveryManifest struct {
	Manifest       int              `json:"manifest"`
	SetID          string           `json:"set_id"`
	CreatedAt      string           `json:"created_at"`
	WardyndVersion string           `json:"wardynd_version"`
	Source         recoverySource   `json:"source"`
	Snapshot       recoverySnapshot `json:"snapshot"`
	AuditSpool     recoverySpool    `json:"audit_spool"`
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
	return createRecoveryManifest(context.Background(), *f.dsn, set, filepath.Base(*f.auditSpool))
}

// createRecoveryManifest writes <set>/manifest.json once, mode 0600. It never overwrites an existing manifest.
func createRecoveryManifest(ctx context.Context, dsn, set, spoolBase string) error {
	if spoolBase == "." || spoolBase == "" {
		return errors.New("refusing to write the manifest: WARDYN_AUDIT_SPOOL names no spool file")
	}
	path := filepath.Join(set, recoveryManifestFile)
	if _, err := os.Stat(path); err == nil {
		return refusedTo("write the manifest", "%s already exists and is never overwritten", recoveryManifestFile)
	}
	s, err := acquireQuiet(ctx, dsn, 30*time.Second, "write the manifest")
	if err != nil {
		return err
	}
	defer s.close()

	snap, err := readRecoverySnapshot(ctx, s.conn)
	if err != nil {
		return err
	}
	src, err := recoverySourceOf(dsn)
	if err != nil {
		return err
	}
	spool, err := stageSpoolFiles(filepath.Join(set, "audit"), spoolBase)
	if err != nil {
		return err
	}
	m := recoveryManifest{
		Manifest: recoveryManifestVersion, SetID: uuid.NewString(), CreatedAt: time.Now().UTC().Format(time.RFC3339),
		WardyndVersion: version.Version, Source: src, Snapshot: snap, AuditSpool: spool,
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
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
	m, err := readRecoveryManifest(filepath.Join(set, recoveryManifestFile))
	if err != nil {
		return err
	}
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
	got, err := stageSpoolFiles(filepath.Join(set, "audit"), m.AuditSpool.Basename)
	if err != nil {
		return err
	}
	for _, c := range []struct {
		name      string
		want, got recoveryFile
	}{
		{m.AuditSpool.Basename, m.AuditSpool.Spool, got.Spool},
		{m.AuditSpool.Basename + ".consumed", m.AuditSpool.Consumed, got.Consumed},
		{m.AuditSpool.Basename + ".quarantine", m.AuditSpool.Quarantine, got.Quarantine},
	} {
		if c.want != c.got {
			return refusedTo("verify the set", "%s does not match its manifest entry (present %t size %d, want present %t size %d)",
				c.name, c.got.Present, c.got.Size, c.want.Present, c.want.Size)
		}
	}
	return nil
}

// readRecoveryManifest reads and checks the manifest's version.
func readRecoveryManifest(path string) (recoveryManifest, error) {
	var m recoveryManifest
	body, err := os.ReadFile(path)
	if err != nil {
		return m, fmt.Errorf("read %s: %w", recoveryManifestFile, err)
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return m, fmt.Errorf("parse %s: %w", recoveryManifestFile, err)
	}
	if m.Manifest != recoveryManifestVersion {
		return m, refusedTo("verify the set", "%s is manifest version %d; this binary reads version %d", recoveryManifestFile, m.Manifest, recoveryManifestVersion)
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

// recoverySourceOf is the DSN's host, port and database, which is all the manifest may name. A DSN that does
// not parse is refused without echoing it: a parse error can carry the DSN.
func recoverySourceOf(dsn string) (recoverySource, error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return recoverySource{}, errors.New("WARDYN_PG_DSN does not parse")
	}
	return recoverySource{Host: cfg.Host, Port: int(cfg.Port), Database: cfg.Database}, nil
}

// stageSpoolFiles hashes <dir>/<base>, its .consumed and its .quarantine. An absent file is recorded as absent.
func stageSpoolFiles(dir, base string) (recoverySpool, error) {
	out := recoverySpool{Basename: base}
	var err error
	if out.Spool, err = hashStagedFile(filepath.Join(dir, base)); err != nil {
		return out, err
	}
	if out.Consumed, err = hashStagedFile(filepath.Join(dir, base+".consumed")); err != nil {
		return out, err
	}
	if out.Quarantine, err = hashStagedFile(filepath.Join(dir, base+".quarantine")); err != nil {
		return out, err
	}
	return out, nil
}

func hashStagedFile(path string) (recoveryFile, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return recoveryFile{}, nil
	}
	if err != nil {
		return recoveryFile{}, fmt.Errorf("open staged file %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return recoveryFile{}, fmt.Errorf("read staged file %s: %w", filepath.Base(path), err)
	}
	return recoveryFile{Present: true, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}
