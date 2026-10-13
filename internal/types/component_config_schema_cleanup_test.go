// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

// Whatever follows the one schema value is refused, whether it opens a value or closes one that was never
// opened; only whitespace may follow.
func TestDecodeComponentConfigSchemaRefusesAnythingAfterTheValue(t *testing.T) {
	const schema = `{"schema_version":1,"fields":[]}`
	for name, tc := range map[string]struct {
		src     string
		trailer bool
	}{
		"a closing bracket":       {schema + `]`, true},
		"a closing brace":         {schema + `}`, true},
		"a second object":         {schema + `{"schema_version":1,"fields":[]}`, true},
		"a newline":               {schema + "\n", false},
		"nothing":                 {schema, false},
		"a newline and a bracket": {schema + "\n]", true},
	} {
		_, issues := DecodeComponentConfigSchema([]byte(tc.src))
		var got bool
		for _, i := range issues {
			if i.Message == "must be exactly one JSON value" {
				got = true
			}
		}
		if got != tc.trailer {
			t.Errorf("%s: refused as trailing data = %v (issues %v), want %v", name, got, issues, tc.trailer)
		}
	}
}

func TestConfigKeyLooksSecretClosedGaps(t *testing.T) {
	for _, key := range []string{"JWT_SIGNING_KEY", "ENCRYPTION_KEY", "DB_CONNECTION_STRING", "connectionString", "IMAGE_PULL_SECRET"} {
		if !ConfigKeyLooksSecret(key) {
			t.Errorf("ConfigKeyLooksSecret(%q) = false, want it read as a secret", key)
		}
	}
	for _, key := range []string{"SECRET_FILE", "SECRET_PATH", "SECRET_STORE", "secret_files", "secretPaths", "SECRET_STORES"} {
		if ConfigKeyLooksSecret(key) {
			t.Errorf("ConfigKeyLooksSecret(%q) = true, want a plain setting", key)
		}
	}
}
