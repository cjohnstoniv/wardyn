// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
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
		{"WARDYN_APPROVAL_NOTIFY", "WARDYN_APPROVAL_NOTIFY_FILE", f.approvalNotify},
		{"WARDYN_ORG_ENROLMENT_TOKEN", "WARDYN_ORG_ENROLMENT_TOKEN_FILE", f.orgEnrolToken},
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

// readSecretFile reads one _FILE value under the shared mode rule
// (cliutil.ReadSecretFile). Exactly one trailing line ending is trimmed (what
// `echo` and most secret writers append); any other whitespace, INCLUDING an
// internal blank line, is part of the value, as it would be in the env var
// (a PEM or JSON body keeps its shape). A SECOND trailing line ending —
// content that still ends in "\n" after the one trim — refuses rather than
// silently keeping a hidden newline on the end of the value: every setting
// here (a DSN, a token, an age key, a JSON audit-sinks blob) is a value whose
// own shape never legitimately ends in a blank line, and a writer that
// appended two (e.g. `cat one-line-file >> out; echo >> out`, or a doubled
// heredoc) is a mistake this can catch instead of shipping a secret with an
// invisible extra byte on the end.
func readSecretFile(fileVar, path string) (string, error) {
	raw, err := cliutil.ReadSecretFile(fileVar, path)
	if err != nil {
		return "", fmt.Errorf("refusing to start: %w", err)
	}
	v := strings.TrimSuffix(string(raw), "\n")
	v = strings.TrimSuffix(v, "\r")
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("refusing to start: %s=%q is empty — the secret it names would silently fall back to unset", fileVar, path)
	}
	if strings.HasSuffix(v, "\n") || strings.HasSuffix(v, "\r") {
		return "", fmt.Errorf("refusing to start: %s=%q has more than one trailing line ending — only one (\\n or \\r\\n) is trimmed, and a value that still ends in a newline or carriage return after that is almost always an accidental blank line, not part of the secret; fix the file so it ends in exactly one line ending", fileVar, path)
	}
	return v, nil
}
