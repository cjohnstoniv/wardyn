// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// rehearseDSNEnv names the scratch database. It is an environment variable and not a flag so a
// password in it never shows in the process list (recovery-set.md §1.2).
const rehearseDSNEnv = "WARDYN_REHEARSE_PG_DSN"

// recoveryRehearseMode is `wardynd -recovery-rehearse -recovery-set <dir>` (recovery-set.md §7): it
// reads the set's manifest, rehearses against WARDYN_REHEARSE_PG_DSN and exits with the verdict's code.
func recoveryRehearseMode(f *bootFlags) error {
	if *f.recoverySet == "" {
		return errors.New("-recovery-rehearse needs -recovery-set <dir>")
	}
	dsn := strings.TrimSpace(os.Getenv(rehearseDSNEnv))
	if dsn == "" {
		return errors.New("-recovery-rehearse needs " + rehearseDSNEnv + " set to the scratch database the dump was loaded into")
	}
	dir := filepath.Clean(*f.recoverySet)
	m, err := readRecoveryManifest(dir, "rehearse")
	if err != nil {
		return err
	}
	if m.Source.Port < 1 || m.Source.Port > 65535 {
		return refusedTo("rehearse", "%s names source port %d, outside 1 to 65535", recoveryManifestFile, m.Source.Port)
	}
	return rehearse(context.Background(), f, rehearseSetOf(dir, m), dsn, os.Stdout, os.Stderr)
}

// rehearseSetOf is what the rehearsal needs of a read manifest.
func rehearseSetOf(dir string, m recoveryManifest) rehearseSet {
	return rehearseSet{
		ID: m.SetID, Dir: dir, HeadSeq: m.Snapshot.AuditHeadSeq, HeadHash: m.Snapshot.AuditHeadHash,
		Migration:    m.Snapshot.SchemaMigration,
		Source:       dsnIdentity{Host: m.Source.Host, Port: uint16(m.Source.Port), Database: m.Source.Database},
		AgeRecipient: m.Keys.AgeRecipient, KEKMode: m.Keys.KEKMode, KeyClasses: m.Keys.Classes, Spool: m.AuditSpool, Recordings: m.Recordings, Drives: m.Drives,
	}
}

// kekModeOf is WARDYN_KEK as the manifest records it: empty means local.
func kekModeOf(f *bootFlags) string {
	if mode := strings.TrimSpace(*f.vault.kek); mode != "" {
		return mode
	}
	return "local"
}
