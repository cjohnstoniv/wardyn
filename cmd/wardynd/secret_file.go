// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

// secretFileSetting pairs one secret-carrying boot setting with its _FILE
// twin: a PATH to a file holding the value, so a Vault Agent injector, the
// Secrets Store CSI driver or a projected Secret volume can deliver it without
// the value ever entering the process environment (where cloud posture
// scanners flag it and /proc/<pid>/environ keeps it for the process lifetime).
type secretFileSetting struct {
	name    string
	fileVar string
	value   *string
}

// secretFileSettings is the ONE list of boot settings that accept a _FILE twin:
// every wardynd boot var docs/ENV.md marks as a secret and that carries the
// value itself. WARDYN_TLS_KEY is absent because it is already a path.
func secretFileSettings(f *bootFlags) []secretFileSetting {
	return []secretFileSetting{
		{"WARDYN_PG_DSN", "WARDYN_PG_DSN_FILE", f.dsn},
		{"WARDYN_PG_MIGRATE_DSN", "WARDYN_PG_MIGRATE_DSN_FILE", f.migrateDSN},
		{"WARDYN_ADMIN_TOKEN", "WARDYN_ADMIN_TOKEN_FILE", f.adminToken},
		{"WARDYN_AGE_KEY", "WARDYN_AGE_KEY_FILE", f.ageKey},
		{"WARDYN_OIDC_CLIENT_SECRET", "WARDYN_OIDC_CLIENT_SECRET_FILE", f.oidcClientSecret},
		{"WARDYN_DIRECTORY_CLIENT_SECRET", "WARDYN_DIRECTORY_CLIENT_SECRET_FILE", f.dirSecret},
		{"WARDYN_AUDIT_SINKS", "WARDYN_AUDIT_SINKS_FILE", f.auditSinks},
	}
}

// resolveSecretFiles fills each setting whose _FILE twin is set from that file,
// once, at boot — the same read-once posture as WARDYN_TRUSTED_CA_FILE, so a
// rotated file takes effect on the next restart, never mid-process.
//
// Setting both forms is refused, not ranked: a silent precedence between two
// ways of naming one secret is how an operator rotates the one that is not in
// effect (daemonProxyBothSetRefusal's reasoning). Every error names the var and
// the path, never the content.
func resolveSecretFiles(settings []secretFileSetting) error {
	for _, s := range settings {
		path := strings.TrimSpace(os.Getenv(s.fileVar))
		if path == "" {
			continue
		}
		if *s.value != "" {
			return fmt.Errorf("refusing to start: both %s (or its flag) and %s are set — unset one; they name the same secret two different ways", s.name, s.fileVar)
		}
		v, err := readSecretFile(s.fileVar, path)
		if err != nil {
			return err
		}
		*s.value = v
	}
	return nil
}

// readSecretFile reads one _FILE value. Exactly one trailing line ending is
// trimmed (what `echo` and most secret writers append); any other whitespace is
// part of the value, as it would be in the env var. The mode is checked on the
// OPENED descriptor, so the file checked is the file read.
func readSecretFile(fileVar, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("refusing to start: %s=%q is unreadable: %w", fileVar, path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("refusing to start: %s=%q is unreadable: %w", fileVar, path, err)
	}
	owner := -1
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		owner = int(st.Uid)
	}
	if err := checkSecretFileMode(fileVar, path, fi.Mode().Perm(), owner, os.Geteuid()); err != nil {
		return "", err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("refusing to start: %s=%q is unreadable: %w", fileVar, path, err)
	}
	v := strings.TrimSuffix(string(raw), "\n")
	v = strings.TrimSuffix(v, "\r")
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("refusing to start: %s=%q is empty — the secret it names would silently fall back to unset", fileVar, path)
	}
	return v, nil
}

// checkSecretFileMode is the mode rule, split out so every delivery shape is
// testable without chown. owner is the file's uid (-1 when unknown).
//
// Group- or world-WRITABLE is always refused: anyone in that set could swap the
// secret before the next boot, and no supported delivery produces it.
//
// Other-READABLE is refused only on a file wardynd's own non-root uid owns — the
// hand-made host file, which `chmod 640` fixes. It stays allowed everywhere a
// supported mechanism produces it, which is why this differs from
// WARDYN_DAEMON_PROXY_SECRET's 0600 rule: a Secret volume is root-owned 0440
// (group-read added by the kubelet under the chart's fsGroup), a Secrets Store
// CSI file is root-owned 0644 and reachable by a non-root reader only through
// the other-read bit, and Vault Agent writes 0644 as its own uid by default.
func checkSecretFileMode(fileVar, path string, perm os.FileMode, owner, euid int) error {
	if perm&0o022 != 0 {
		return fmt.Errorf("refusing to start: %s=%q is mode %04o — a group- or world-writable secret file lets someone else replace it; remove the write bits (chmod 640)", fileVar, path, perm)
	}
	if perm&0o004 != 0 && euid != 0 && owner == euid {
		return fmt.Errorf("refusing to start: %s=%q is mode %04o and owned by wardynd's own uid %d — any local user can read it; chmod 640 it (under Vault Agent, set vault.hashicorp.com/agent-inject-perms-<name>: \"0440\")", fileVar, path, perm, euid)
	}
	return nil
}
